package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"vision/internal/diff"
	"vision/internal/gallery"
	"vision/internal/store"
)

const Address = "127.0.0.1:4747"

type SnapRequest struct {
	Project store.Project    `json:"project"`
	Key     string           `json:"key"`
	Variant string           `json:"variant"`
	Note    string           `json:"note,omitempty"`
	Session string           `json:"session,omitempty"`
	PNG     string           `json:"png"`
	Capture store.Conditions `json:"conditions"`
}

type VerdictRequest struct {
	Project string `json:"project"`
	Key     string `json:"key"`
	Variant string `json:"variant"`
	Digest  string `json:"digest"`
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

type Item struct {
	Project         string    `json:"project"`
	Branch          string    `json:"branch"`
	Key             string    `json:"key"`
	Variant         string    `json:"variant"`
	Digest          string    `json:"digest"`
	Status          string    `json:"status"`
	ChangedFraction float64   `json:"changedFraction,omitempty"`
	Incomparable    string    `json:"incomparable,omitempty"`
	ShotURL         string    `json:"shotUrl"`
	BaselineURL     string    `json:"baselineUrl,omitempty"`
	DiffURL         string    `json:"diffUrl,omitempty"`
	TS              time.Time `json:"-"`
	Steps           []Step    `json:"steps,omitempty"`
}

type Step struct {
	Digest   string `json:"digest"`
	ThumbURL string `json:"thumbUrl,omitempty"`
}

type Server struct {
	mux     sync.Mutex
	clients map[chan Item]struct{}
}

func New() *Server { return &Server{clients: make(map[chan Item]struct{})} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.gallery)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/queue", s.queue)
	mux.HandleFunc("POST /api/snap", s.snap)
	mux.HandleFunc("POST /api/verdict", s.verdict)
	mux.HandleFunc("GET /media/{kind}/{project}/{digest}", s.media)
	mux.HandleFunc("GET /events", s.events)
	return mux
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(Address, s.Handler())
}

func (s *Server) gallery(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(gallery.HTML)
}

func (s *Server) snap(w http.ResponseWriter, r *http.Request) {
	var req SnapRequest
	if err := decode(r.Body, &req); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	if err := store.ParseKey(req.Key); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	if err := store.ParseVariant(req.Variant); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	pngData, err := base64.StdEncoding.DecodeString(req.PNG)
	if err != nil {
		problem(w, http.StatusBadRequest, errors.New("invalid PNG encoding"))
		return
	}
	digest := diff.Digest(pngData)
	record := store.Snap{SchemaVersion: store.SchemaVersion, TS: time.Now().UTC(), Project: req.Project.Name, Key: req.Key, Variant: req.Variant, Digest: digest, Branch: req.Project.Branch, SHA: req.Project.SHA, Dirty: req.Project.Dirty, Worktree: req.Project.Worktree, Session: req.Session, Note: req.Note, Conditions: req.Capture}
	if err := store.AppendSnap(req.Project.ID, record, pngData); err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	item, pending, err := makeItem(req.Project.ID, req.Project.Name, record)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	if pending {
		s.publish(item)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schemaVersion": 1, "digest": digest, "queued": pending, "item": item, "warning": variantWarning(req.Project.ID, req.Key, req.Variant)})
}

func (s *Server) verdict(w http.ResponseWriter, r *http.Request) {
	var req VerdictRequest
	if err := decode(r.Body, &req); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	if req.Verdict != "ok" && req.Verdict != "flag" {
		problem(w, http.StatusBadRequest, errors.New("verdict must be ok or flag"))
		return
	}
	if req.Verdict == "flag" && strings.TrimSpace(req.Note) == "" {
		problem(w, http.StatusBadRequest, errors.New("flag requires a note"))
		return
	}
	note := store.Note{SchemaVersion: store.SchemaVersion, TS: time.Now().UTC(), Key: req.Key, Variant: req.Variant, Digest: req.Digest, Verdict: req.Verdict, Note: strings.TrimSpace(req.Note)}
	if err := store.AppendNote(req.Project, note); err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	if req.Verdict == "ok" {
		if err := store.CopyBaseline(req.Project, req.Key, req.Variant, req.Digest); err != nil {
			problem(w, http.StatusInternalServerError, err)
			return
		}
	}
	// Approving is the moment a shot stops being the baseline, so it is the moment old
	// shots become reclaimable. A failed prune costs disk and nothing else, so it must
	// never fail a verdict the human already gave.
	_ = store.Prune(req.Project)
	writeJSON(w, http.StatusCreated, note)
}

func (s *Server) queue(w http.ResponseWriter, _ *http.Request) {
	items, err := Queue()
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func Queue() ([]Item, error) {
	ids, err := store.ProjectIDs()
	if err != nil {
		return nil, err
	}
	var items []Item
	seen := make(map[string]bool)
	for _, id := range ids {
		snaps, err := store.Snaps(id)
		if err != nil {
			return nil, err
		}
		notes, err := store.Notes(id)
		if err != nil {
			return nil, err
		}
		decided := make(map[string]bool)
		for _, n := range notes {
			decided[n.Digest] = true
		}
		name := id[:min(8, len(id))]
		for _, snap := range snaps {
			if decided[snap.Digest] || seen[snap.Digest] {
				continue
			}
			seen[snap.Digest] = true
			if snap.Project != "" {
				name = snap.Project
			}
			item, pending, err := makeItem(id, name, snap)
			if err != nil {
				return nil, err
			}
			if pending {
				items = append(items, item)
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].TS.After(items[j].TS) })
	attachSteps(items)
	return items, nil
}

func makeItem(project, name string, snap store.Snap) (Item, bool, error) {
	item := Item{Project: name, Branch: snap.Branch, Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Status: "new", ShotURL: mediaURL("shot", project, snap.Digest), TS: snap.TS}
	baseline, _ := store.BaselinePath(project, snap.Key, snap.Variant)
	b, err := os.ReadFile(baseline)
	if errors.Is(err, os.ErrNotExist) {
		return item, true, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	shot, err := os.ReadFile(mustShotPath(project, snap.Digest))
	if err != nil {
		return Item{}, false, err
	}
	query := "?key=" + url.QueryEscape(snap.Key) + "&variant=" + url.QueryEscape(snap.Variant)
	item.BaselineURL = mediaURL("baseline", project, snap.Digest) + query
	if diff.Digest(b) == snap.Digest {
		return item, false, nil
	}
	item.Status = "changed"
	baseSnap, ok := baselineRecord(project, snap.Key, snap.Variant)
	if ok {
		if err := diff.Comparable(baseSnap.Conditions, snap.Conditions); err != nil {
			item.Incomparable = err.Error()
			return item, true, nil
		}
	}
	_, fraction, err := diff.PNG(b, shot)
	if err != nil {
		item.Incomparable = err.Error()
		return item, true, nil
	}
	item.ChangedFraction = fraction
	item.DiffURL = mediaURL("diff", project, snap.Digest) + query
	return item, true, nil
}

func baselineRecord(project, key, variant string) (store.Snap, bool) {
	snaps, _ := store.Snaps(project)
	notes, _ := store.Notes(project)
	approved := make(map[string]bool)
	for _, n := range notes {
		if n.Verdict == "ok" && n.Key == key && n.Variant == variant {
			approved[n.Digest] = true
		}
	}
	for i := len(snaps) - 1; i >= 0; i-- {
		if approved[snaps[i].Digest] {
			return snaps[i], true
		}
	}
	return store.Snap{}, false
}

func attachSteps(items []Item) {
	for i := range items {
		base := strings.Split(items[i].Key, "#")[0]
		for _, candidate := range items {
			if strings.Split(candidate.Key, "#")[0] == base && strings.Contains(candidate.Key, "#") && candidate.Variant == items[i].Variant {
				items[i].Steps = append(items[i].Steps, Step{Digest: candidate.Digest, ThumbURL: candidate.ShotURL})
			}
		}
	}
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	kind, project, digest := r.PathValue("kind"), r.PathValue("project"), r.PathValue("digest")
	var data []byte
	var err error
	switch kind {
	case "shot":
		data, err = os.ReadFile(mustShotPath(project, digest))
	case "baseline", "diff":
		if store.ParseKey(r.URL.Query().Get("key")) != nil || store.ParseVariant(r.URL.Query().Get("variant")) != nil {
			http.NotFound(w, r)
			return
		}
		base, e := store.BaselinePath(project, r.URL.Query().Get("key"), r.URL.Query().Get("variant"))
		if e != nil {
			err = e
			break
		}
		data, err = os.ReadFile(base)
		if kind == "diff" && err == nil {
			shot, e := os.ReadFile(mustShotPath(project, digest))
			if e != nil {
				err = e
			} else {
				data, _, err = diff.PNG(data, shot)
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(data)
}

func mustShotPath(project, digest string) string { p, _ := store.ShotPath(project, digest); return p }
func mediaURL(kind, project, digest string) string {
	return "/media/" + kind + "/" + project + "/" + strings.TrimPrefix(digest, "sha256:")
}

func variantWarning(project, key, variant string) string {
	snaps, _ := store.Snaps(project)
	for _, snap := range snaps {
		if snap.Key == key && snap.Variant != variant {
			return fmt.Sprintf("%s has existing variant %q, new variant %q", key, snap.Variant, variant)
		}
	}
	return ""
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		problem(w, http.StatusInternalServerError, errors.New("stream unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan Item, 8)
	s.mux.Lock()
	s.clients[ch] = struct{}{}
	s.mux.Unlock()
	defer func() { s.mux.Lock(); delete(s.clients, ch); s.mux.Unlock() }()
	for {
		select {
		case item := <-ch:
			b, _ := json.Marshal(item)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) publish(item Item) {
	s.mux.Lock()
	defer s.mux.Unlock()
	for ch := range s.clients {
		select {
		case ch <- item:
		default:
		}
	}
}

func decode(r io.Reader, dst any) error {
	d := json.NewDecoder(io.LimitReader(r, 20<<20))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"schemaVersion": 1, "error": err.Error()})
}

func ProjectName(id string) string {
	dir, _ := store.ProjectDir(id)
	snaps, _ := store.ReadJSONL[store.Snap](filepath.Join(dir, "index.jsonl"))
	if len(snaps) > 0 && snaps[len(snaps)-1].Worktree != "" {
		return filepath.Base(snaps[len(snaps)-1].Worktree)
	}
	return id[:min(8, len(id))]
}

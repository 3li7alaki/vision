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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"vision/internal/diff"
	"vision/internal/gallery"
	"vision/internal/icon"
	"vision/internal/store"
)

const Address = "127.0.0.1:4747"

// projectIDPattern permits only the SHA-256 hex ids Identify emits because request ids
// reach store paths. Unchecked ids could otherwise write outside the state directory.
var projectIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]+$`)

type SnapRequest struct {
	Project  store.Project     `json:"project"`
	Key      string            `json:"key"`
	Variant  string            `json:"variant"`
	Dims     map[string]string `json:"dims,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
	Note     string            `json:"note,omitempty"`
	Session  string            `json:"session,omitempty"`
	Thread   string            `json:"thread,omitempty"`
	PNG      string            `json:"png"`
	Capture  store.Conditions  `json:"conditions"`
	Elements []store.Element   `json:"elements,omitempty"`
}

type VerdictRequest struct {
	Project string         `json:"project"`
	Key     string         `json:"key"`
	Variant string         `json:"variant"`
	Digest  string         `json:"digest"`
	Verdict string         `json:"verdict"`
	Note    string         `json:"note"`
	Regions []store.Region `json:"regions,omitempty"`
}

type WithdrawRequest struct {
	Project string `json:"project"`
	Key     string `json:"key"`
	Variant string `json:"variant"`
	Session string `json:"session"`
	Thread  string `json:"thread"`
}

type Item struct {
	Project         string            `json:"project"`
	ProjectID       string            `json:"projectId"`
	Branch          string            `json:"branch"`
	Thread          string            `json:"thread,omitempty"`
	Key             string            `json:"key"`
	Variant         string            `json:"variant"`
	Dims            map[string]string `json:"dims,omitempty"`
	Meta            map[string]string `json:"meta,omitempty"`
	Group           string            `json:"group"`
	Digest          string            `json:"digest"`
	Status          string            `json:"status"`
	ChangedFraction float64           `json:"changedFraction,omitempty"`
	Incomparable    string            `json:"incomparable,omitempty"`
	Changes         []string          `json:"changes,omitempty"`
	ShotURL         string            `json:"shotUrl"`
	BaselineURL     string            `json:"baselineUrl,omitempty"`
	DiffURL         string            `json:"diffUrl,omitempty"`
	TS              time.Time         `json:"ts"`
	Steps           []Step            `json:"steps,omitempty"`
}

// Step names its key as well as its digest, because two steps of a flow can show
// identical pixels and are still two reviews.
type Step struct {
	Key      string `json:"key"`
	Digest   string `json:"digest"`
	ThumbURL string `json:"thumbUrl,omitempty"`
}

type Server struct {
	// Serialize ledger writes so a verdict cannot land between a withdrawal's check and append.
	ledger  sync.Mutex
	mux     sync.Mutex
	clients map[chan Item]struct{}
}

func New() *Server { return &Server{clients: make(map[chan Item]struct{})} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.gallery)
	mux.HandleFunc("GET /p/{project}", s.gallery)
	mux.HandleFunc("GET /p/{project}/b/{branch...}", s.gallery)
	mux.HandleFunc("GET /p/{project}/t/{thread}", s.gallery)
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/queue", s.queue)
	mux.HandleFunc("POST /api/snap", s.snap)
	mux.HandleFunc("POST /api/verdict", s.verdict)
	mux.HandleFunc("POST /api/withdraw", s.withdraw)
	mux.HandleFunc("GET /media/{kind}/{project}/{digest}", s.media)
	mux.HandleFunc("GET /media/icon/{project}", s.projectIcon)
	mux.HandleFunc("GET /events", s.events)
	return mux
}

func (s *Server) ListenAndServe() error {
	// Only the daemon owns retention. CLI reads must never sweep another session's
	// state, and handler-only callers (including tests) must not start background work.
	sweep := func(now time.Time) {
		removed, err := store.SweepSessionState(now, store.SessionStateMaxAge)
		if removed > 0 {
			fmt.Fprintf(os.Stderr, "vision: removed %d expired session state files\n", removed)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "vision: session state sweep:", err)
		}
	}
	sweep(time.Now())
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case now := <-ticker.C:
				sweep(now)
			case <-done:
				return
			}
		}
	}()
	return http.ListenAndServe(Address, s.Handler())
}

func (s *Server) gallery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(gallery.HTML)
}

func (s *Server) snap(w http.ResponseWriter, r *http.Request) {
	var req SnapRequest
	if err := decode(r.Body, &req); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	if !projectIDPattern.MatchString(req.Project.ID) {
		problem(w, http.StatusBadRequest, errors.New("invalid project id"))
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
	s.ledger.Lock()
	defer s.ledger.Unlock()
	record := store.Snap{SchemaVersion: store.SchemaVersion, TS: time.Now().UTC(), Project: req.Project.Name, Key: req.Key, Variant: req.Variant, Dims: req.Dims, Meta: req.Meta, Digest: digest, Branch: req.Project.Branch, SHA: req.Project.SHA, Dirty: req.Project.Dirty, Worktree: req.Project.Worktree, Session: req.Session, Thread: req.Thread, Note: req.Note, Conditions: req.Capture}
	if err := store.WriteElements(req.Project.ID, digest, req.Elements); err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	if err := store.AppendSnap(req.Project.ID, record, pngData); err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	snaps, err := store.Snaps(req.Project.ID)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	closed, err := store.Closed(req.Project.ID, snaps)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	item, pending, err := makeItem(req.Project.ID, req.Project.Name, record)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	pending = pending && !closed[record.Review()]
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
	if !projectIDPattern.MatchString(req.Project) {
		problem(w, http.StatusBadRequest, errors.New("invalid project id"))
		return
	}
	// ReadElements opens shots/<digest>.json, and CopyBaseline also opens a file by digest.
	// An unchecked digest permits path traversal on this unauthenticated endpoint.
	if !digestPattern.MatchString(req.Digest) {
		problem(w, http.StatusBadRequest, errors.New("invalid digest"))
		return
	}
	// These are trust-boundary caps on an unauthenticated local endpoint, not product limits.
	if len(req.Regions) > 50 {
		problem(w, http.StatusBadRequest, errors.New("at most 50 regions are allowed"))
		return
	}
	for i := range req.Regions {
		region := &req.Regions[i]
		if region.Tone != "issue" && region.Tone != "praise" && region.Tone != "question" {
			problem(w, http.StatusBadRequest, errors.New("region tone must be issue, praise or question"))
			return
		}
		region.Text = strings.TrimSpace(region.Text)
		if region.Text == "" || utf8.RuneCountInString(region.Text) > 2000 {
			problem(w, http.StatusBadRequest, errors.New("region text must be non-empty and at most 2000 runes"))
			return
		}
		if region.X < 0 || region.Y < 0 || region.W < 0 || region.H < 0 {
			problem(w, http.StatusBadRequest, errors.New("region coordinates and dimensions must be non-negative"))
			return
		}
		region.Element = ""
	}
	if req.Verdict != "ok" && req.Verdict != "flag" {
		problem(w, http.StatusBadRequest, errors.New("verdict must be ok or flag"))
		return
	}
	s.ledger.Lock()
	defer s.ledger.Unlock()
	// Naming a mark is optional context, so a missing or unreadable snapshot must never
	// fail a verdict the human already gave.
	if len(req.Regions) > 0 {
		elements, err := store.ReadElements(req.Project, req.Digest)
		if err == nil {
			snaps, err := store.Snaps(req.Project)
			if err == nil {
				for i := len(snaps) - 1; i >= 0; i-- {
					if snaps[i].Digest != req.Digest {
						continue
					}
					for j := range req.Regions {
						req.Regions[j].Element = resolve(elements, req.Regions[j], snaps[i].Conditions.DPR)
					}
					break
				}
			}
		}
	}
	rendered := render(strings.TrimSpace(req.Note), req.Regions)
	if req.Verdict == "flag" && rendered == "" {
		problem(w, http.StatusBadRequest, errors.New("flag requires a note"))
		return
	}
	note := store.Note{SchemaVersion: store.SchemaVersion, TS: time.Now().UTC(), Key: req.Key, Variant: req.Variant, Digest: req.Digest, Verdict: req.Verdict, Note: rendered, Regions: req.Regions}
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

func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	var req WithdrawRequest
	if err := decode(r.Body, &req); err != nil {
		problem(w, http.StatusBadRequest, err)
		return
	}
	if !projectIDPattern.MatchString(req.Project) {
		problem(w, http.StatusBadRequest, errors.New("invalid project id"))
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
	if req.Thread == "" {
		problem(w, http.StatusBadRequest, errors.New("thread must not be empty"))
		return
	}
	s.ledger.Lock()
	defer s.ledger.Unlock()
	snaps, err := store.Snaps(req.Project)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	closed, err := store.Closed(req.Project, snaps)
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	var pending []store.Snap
	seen := make(map[string]bool)
	for _, snap := range snaps {
		if snap.Key == req.Key && snap.Variant == req.Variant && snap.ThreadID() == req.Thread && !closed[snap.Review()] && !seen[snap.Digest] {
			pending = append(pending, snap)
			seen[snap.Digest] = true
		}
	}
	if len(pending) == 0 {
		problem(w, http.StatusNotFound, fmt.Errorf("nothing pending for %s@%s on this thread", req.Key, req.Variant))
		return
	}
	digests := make([]string, 0, len(pending))
	for _, snap := range pending {
		withdrawal := store.Withdrawal{SchemaVersion: store.SchemaVersion, TS: time.Now().UTC(), Key: req.Key, Variant: req.Variant, Digest: snap.Digest, Session: req.Session, Thread: req.Thread}
		if err := store.AppendWithdrawal(req.Project, withdrawal); err != nil {
			problem(w, http.StatusInternalServerError, err)
			return
		}
		digests = append(digests, snap.Digest)
	}
	_ = store.Prune(req.Project)
	for _, snap := range pending {
		name := snap.Project
		if name == "" {
			name = ProjectName(req.Project)
		}
		s.publish(Item{ProjectID: req.Project, Project: name, Thread: req.Thread, Key: req.Key, Variant: req.Variant, Digest: snap.Digest, Status: "withdrawn"})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schemaVersion": store.SchemaVersion, "withdrawn": digests})
}

// ponytail: A loose box resolves by its center rather than overlap. Upgrade to
// overlap-weighted scoring if real annotations show misses.
func resolve(elements []store.Element, r store.Region, dpr float64) string {
	if dpr <= 0 {
		dpr = 1
	}
	cx, cy := (float64(r.X)+float64(r.W)/2)/dpr, (float64(r.Y)+float64(r.H)/2)/dpr
	var best *store.Element
	for i := range elements {
		e := &elements[i]
		if !(e.X <= cx && cx < e.X+e.W && e.Y <= cy && cy < e.Y+e.H) {
			continue
		}
		if best == nil || e.W*e.H < best.W*best.H || (e.W*e.H == best.W*best.H && e.Depth > best.Depth) {
			best = e
		}
	}
	if best == nil {
		return ""
	}
	name := []rune(best.Name)
	if len(name) > 60 {
		name = append(name[:60], '.', '.', '.')
	}
	return fmt.Sprintf("%s %q", best.Role, string(name))
}

func render(free string, regions []store.Region) string {
	var lines []string
	if free != "" {
		lines = append(lines, free)
	}
	for i, r := range regions {
		tone := r.Tone
		if r.Flow {
			tone += " (every step)"
		}
		where := fmt.Sprintf("at %d,%d", r.X, r.Y)
		if r.Element != "" {
			where = "on " + r.Element
		} else if r.W != 0 || r.H != 0 {
			where = fmt.Sprintf("box %d,%d %dx%d", r.X, r.Y, r.W, r.H)
		}
		lines = append(lines, fmt.Sprintf("[%d] %s %s: %s", i+1, tone, where, strings.ReplaceAll(r.Text, "\n", "\n    ")))
	}
	return strings.Join(lines, "\n")
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	pending, err := PendingCount()
	if err != nil {
		problem(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": pending})
}

// PendingCount is the cheap half of Queue: it counts live pending reviews without
// decoding a single PNG. Queue itself runs a full pixel diff per
// item, which is far too expensive for something a status line polls on a timer.
//
// It agrees with Queue because decisions belong to key, variant and digest together.
// Identical content under two keys is two reviews. Only a re-snap of the same key and
// variant with the same digest is already decided by an earlier verdict.
func PendingCount() (int, error) {
	ids, err := store.ProjectIDs()
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, id := range ids {
		n, err := PendingCountFor(id)
		if err != nil {
			return 0, err
		}
		pending += n
	}
	return pending, nil
}

// PendingCountFor counts one project's undecided shots. A review waiting in another repo
// is not this repo's work, so anything that speaks for the current project (a status line,
// `vision status`) counts here rather than through the daemon-wide total.
func PendingCountFor(project string) (int, error) {
	return PendingCountForSession(project, "")
}

// Empty session preserves the project-wide count. Filter before deduplicating because a
// review first captured by another session can still belong to this session as well.
func PendingCountForSession(project, session string) (int, error) {
	thread, err := store.ThreadFor(project, session)
	if err != nil {
		return 0, err
	}
	snaps, err := store.Snaps(project)
	if err != nil {
		return 0, err
	}
	closed, err := store.Closed(project, snaps)
	if err != nil {
		return 0, err
	}
	pending, seen := 0, make(map[string]bool)
	for _, snap := range snaps {
		if session != "" && snap.ThreadID() != thread {
			continue
		}
		review := store.ReviewID(snap.Key, snap.Variant, snap.Digest)
		if closed[snap.Review()] || seen[review] {
			continue
		}
		seen[review] = true
		if !store.HasShot(project, snap.Digest) {
			continue
		}
		pending++
	}
	return pending, nil
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
		closed, err := store.Closed(id, snaps)
		if err != nil {
			return nil, err
		}
		name := id[:min(8, len(id))]
		for _, snap := range snaps {
			review := store.ReviewID(snap.Key, snap.Variant, snap.Digest)
			if closed[snap.Review()] || seen[id+"@"+review] {
				continue
			}
			seen[id+"@"+review] = true
			// A record whose picture is gone cannot be reviewed, and one such record must
			// not take down every other project's queue with it, so it is skipped here and
			// in the pending count alike.
			if !store.HasShot(id, snap.Digest) {
				continue
			}
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
	newest := make(map[string]time.Time)
	for _, item := range items {
		if item.TS.After(newest[item.Group]) {
			newest[item.Group] = item.TS
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Group != items[j].Group {
			if newest[items[i].Group].Equal(newest[items[j].Group]) {
				return items[i].Group < items[j].Group
			}
			return newest[items[i].Group].After(newest[items[j].Group])
		}
		if items[i].Variant != items[j].Variant {
			return items[i].Variant < items[j].Variant
		}
		return items[i].TS.After(items[j].TS)
	})
	attachSteps(items)
	return items, nil
}

func makeItem(project, name string, snap store.Snap) (Item, bool, error) {
	// Dimensions are read back out of the variant rather than out of snap.Dims, even though
	// the two are identical by construction. The variant is what actually chose the baseline
	// this shot is measured against, so anything else on screen would let a reviewer read one
	// set of dimensions while the comparison used another.
	item := Item{Project: name, ProjectID: project, Branch: snap.Branch, Thread: snap.ThreadID(), Key: snap.Key, Variant: snap.Variant, Dims: store.DecodeVariant(snap.Variant), Meta: snap.Meta, Group: project + "/" + snap.Key, Digest: snap.Digest, Status: "new", ShotURL: mediaURL("shot", project, snap.Digest), TS: snap.TS}
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
	if ok {
		item.Changes = diff.Changes(baseSnap.Conditions, snap.Conditions, b, shot)
	} else {
		item.Changes = diff.Changes(store.Conditions{}, store.Conditions{}, b, shot)
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
			approved[store.ReviewID(n.Key, n.Variant, n.Digest)] = true
		}
	}
	for i := len(snaps) - 1; i >= 0; i-- {
		if approved[store.ReviewID(snaps[i].Key, snaps[i].Variant, snaps[i].Digest)] {
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
				items[i].Steps = append(items[i].Steps, Step{Key: candidate.Key, Digest: candidate.Digest, ThumbURL: candidate.ShotURL})
			}
		}
	}
}

func (s *Server) projectIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "max-age=300")
	// SVGs can also be opened as documents, so sandbox them on the gallery's origin.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	project := r.PathValue("project")
	if !projectIDPattern.MatchString(project) {
		http.NotFound(w, r)
		return
	}
	snaps, err := store.Snaps(project)
	if err != nil || len(snaps) == 0 {
		http.NotFound(w, r)
		return
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].TS.After(snaps[j].TS) })
	name := ""
	var worktrees []string
	seen := make(map[string]bool)
	for _, snap := range snaps {
		if name == "" && snap.Project != "" {
			name = snap.Project
		}
		if snap.Worktree != "" && !seen[snap.Worktree] {
			seen[snap.Worktree] = true
			worktrees = append(worktrees, snap.Worktree)
		}
	}
	if name == "" {
		http.NotFound(w, r)
		return
	}
	path, err := icon.Resolve(name, worktrees)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vision: project icon:", err)
	}
	contentType := ""
	switch strings.ToLower(filepath.Ext(path)) {
	case ".svg":
		contentType = "image/svg+xml"
	case ".ico":
		contentType = "image/x-icon"
	case ".png":
		contentType = "image/png"
	case ".jpg", ".jpeg":
		contentType = "image/jpeg"
	case ".webp":
		contentType = "image/webp"
	default:
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
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

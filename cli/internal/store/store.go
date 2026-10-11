package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const SchemaVersion = 1

// Media extensions are closed so mislabeled evidence cannot silently choose a file format.
var mediaExtensions = map[string]string{
	"image/png":  ".png",
	"video/mp4":  ".mp4",
	"video/webm": ".webm",
}

func MediaExtension(media string) (string, error) {
	if media == "" {
		media = "image/png"
	}
	ext, ok := mediaExtensions[media]
	if !ok {
		return "", fmt.Errorf("unknown media %q", media)
	}
	return ext, nil
}

func ValidateMedia(media string) error {
	_, err := MediaExtension(media)
	return err
}

// Viewport bucket boundaries, named for the class they open rather than the one they close,
// because "mobileMax = 600" reads as "600 is mobile" when mobile actually ends at 599. This
// is the calibration knob of the dimension model: it decides that a 390 and a 414 wide shot
// share one baseline. A real device landing near an edge is what will move these, and they
// stay constants rather than configuration until that actually happens.
const (
	tabletMinWidth  = 600
	desktopMinWidth = 1024
)

// The dimension vocabulary is closed on purpose. An open one lets "--dim schema=dark" quietly
// mint a second baseline, and at review time a shot measured against the wrong baseline is
// indistinguishable from a real visual regression. A typo has to be an error, not a fork.
var dimensionNames = map[string]bool{
	"flow": true, "state": true, "case": true, "scheme": true,
	"vp": true, "locale": true, "role": true,
}

type Conditions struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	DPR    float64 `json:"dpr"`
	URL    string  `json:"url"`
	Scheme string  `json:"scheme"`
	// Selector is the CSS selector an element shot was clipped to, empty for a viewport
	// shot. An element and the viewport around it are different pictures, never a diff.
	Selector string `json:"selector,omitempty"`
}

type Snap struct {
	SchemaVersion int               `json:"schemaVersion"`
	TS            time.Time         `json:"ts"`
	Project       string            `json:"project,omitempty"`
	Key           string            `json:"key"`
	Variant       string            `json:"variant"`
	Dims          map[string]string `json:"dims,omitempty"`
	Meta          map[string]string `json:"meta,omitempty"`
	Digest        string            `json:"digest"`
	Media         string            `json:"media,omitempty"`
	Branch        string            `json:"branch"`
	SHA           string            `json:"sha"`
	Dirty         bool              `json:"dirty"`
	Worktree      string            `json:"worktree"`
	Session       string            `json:"session"`
	Thread        string            `json:"thread,omitempty"`
	Note          string            `json:"note,omitempty"`
	Conditions    Conditions        `json:"conditions"`
}

// Element is one accessibility node of a shot, in CSS pixels relative to the stored
// image's top-left corner. Multiply by the snap's Conditions.DPR to get image pixels.
type Element struct {
	Role  string  `json:"role"`
	Name  string  `json:"name"`
	Depth int     `json:"depth"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
}

// Region is one mark a human drew on a shot in the gallery, in image pixels. Element is
// filled by the daemon, never trusted from the client.
type Region struct {
	X       int      `json:"x"`
	Y       int      `json:"y"`
	W       int      `json:"w"`
	H       int      `json:"h"`
	Tone    string   `json:"tone"`
	Text    string   `json:"text"`
	Flow    bool     `json:"flow,omitempty"`
	Element string   `json:"element,omitempty"`
	T       *float64 `json:"t,omitempty"`
}

type Note struct {
	SchemaVersion int       `json:"schemaVersion"`
	TS            time.Time `json:"ts"`
	Key           string    `json:"key"`
	Variant       string    `json:"variant"`
	Digest        string    `json:"digest"`
	Verdict       string    `json:"verdict"`
	Note          string    `json:"note,omitempty"`
	Regions       []Region  `json:"regions,omitempty"`
}

// Withdrawals stay separate from human verdicts so notes readers, cursors and hooks
// never need to filter agent actions out of notes.jsonl.
type Withdrawal struct {
	SchemaVersion int       `json:"schemaVersion"`
	TS            time.Time `json:"ts"`
	Key           string    `json:"key"`
	Variant       string    `json:"variant"`
	Digest        string    `json:"digest"`
	Session       string    `json:"session"`
	Thread        string    `json:"thread"`
}

// ReviewID keeps decisions separate when identical pixels belong to different keys or variants.
func ReviewID(key, variant, digest string) string {
	return key + "@" + variant + "@" + digest
}

// Review scopes agent actions to a thread while human verdicts still apply to identical reviews.
type Review struct {
	Thread string
	ID     string
}

func (s Snap) Review() Review {
	return Review{Thread: s.ThreadID(), ID: ReviewID(s.Key, s.Variant, s.Digest)}
}

// Closed derives the single pending rule: decided, withdrawn and superseded reviews
// are closed. Ledger order defines newest so clock skew cannot revive a stale review.
func Closed(project string, snaps []Snap) (map[Review]bool, error) {
	notes, err := Notes(project)
	if err != nil {
		return nil, err
	}
	withdrawals, err := Withdrawals(project)
	if err != nil {
		return nil, err
	}
	decided := make(map[string]bool, len(notes))
	for _, note := range notes {
		decided[ReviewID(note.Key, note.Variant, note.Digest)] = true
	}
	closed := make(map[Review]bool)
	for _, w := range withdrawals {
		closed[Review{Thread: w.Thread, ID: ReviewID(w.Key, w.Variant, w.Digest)}] = true
	}
	latest := make(map[[3]string]string)
	for _, snap := range snaps {
		latest[[3]string{snap.ThreadID(), snap.Key, snap.Variant}] = snap.Digest
	}
	for _, snap := range snaps {
		if decided[ReviewID(snap.Key, snap.Variant, snap.Digest)] || latest[[3]string{snap.ThreadID(), snap.Key, snap.Variant}] != snap.Digest {
			closed[snap.Review()] = true
		}
	}
	return closed, nil
}

type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Root     string `json:"root"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	SHA      string `json:"sha"`
	Dirty    bool   `json:"dirty"`
}

var keyPattern = regexp.MustCompile(`^[^/#\s]+/[^/#\s]+(?:#[1-9][0-9]*)?$`)

func ParseKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("invalid key %q: want <feature>/<slug> with optional #n", key)
	}
	return nil
}

func ParseVariant(variant string) error {
	if variant == "" || strings.ContainsAny(variant, "/@\x00\r\n") {
		return fmt.Errorf("invalid variant %q", variant)
	}
	if strings.Contains(variant, "=") {
		if _, err := decodeVariant(variant); err != nil {
			return fmt.Errorf("invalid variant %q: %w", variant, err)
		}
	}
	return nil
}

// EncodeVariant renders dimensions into the variant token that names a baseline on disk.
// Sorting is what makes it an identity rather than a label: the same set of dimensions has
// to produce the same string whatever order the caller happened to build the map in, or two
// runs of the same check would measure against two different baselines.
func EncodeVariant(dims map[string]string) (string, error) {
	keys := make([]string, 0, len(dims))
	for key, value := range dims {
		if !dimensionNames[key] {
			return "", fmt.Errorf("unknown dimension %q", key)
		}
		if value == "" {
			return "", fmt.Errorf("dimension %q must not have an empty value", key)
		}
		if strings.ContainsAny(key, "=,") || strings.ContainsAny(value, "=,") {
			return "", errors.New("dimension key and value must not contain = or ,")
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "", errors.New("at least one dimension is required")
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+dims[key])
	}
	return strings.Join(pairs, ","), nil
}

// DecodeVariant returns nil for a variant this scheme does not own, which is how every
// baseline taken before dimensions existed keeps working: "default" and "mobile-dark" are
// opaque tokens, they decode to nothing, and nothing on disk had to be renamed for that.
func DecodeVariant(variant string) map[string]string {
	if !strings.Contains(variant, "=") {
		return nil
	}
	dims, err := decodeVariant(variant)
	if err != nil {
		return nil
	}
	return dims
}

func decodeVariant(variant string) (map[string]string, error) {
	dims := make(map[string]string)
	for _, pair := range strings.Split(variant, ",") {
		parts := strings.Split(pair, "=")
		if len(parts) != 2 {
			return nil, errors.New("dimensions must use k=v pairs")
		}
		key, value := parts[0], parts[1]
		if _, exists := dims[key]; exists {
			return nil, fmt.Errorf("duplicate dimension %q", key)
		}
		dims[key] = value
	}
	// Re-encoding and comparing is the cheapest way to reject a variant that would name a
	// baseline no encoder could ever produce. Without it "scheme=dark,flow=pay" and
	// "flow=pay,scheme=dark" are two files holding the same thing, and only one of them is
	// ever compared against.
	encoded, err := EncodeVariant(dims)
	if err != nil {
		return nil, err
	}
	if encoded != variant {
		return nil, errors.New("dimensions must be canonically ordered")
	}
	return dims, nil
}

func ViewportClass(width int) string {
	if width < tabletMinWidth {
		return "mobile"
	}
	if width < desktopMinWidth {
		return "tablet"
	}
	return "desktop"
}

func StateHome() (string, error) {
	if p := os.Getenv("VISION_STATE_HOME"); p != "" {
		return filepath.Abs(p)
	}
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return filepath.Join(p, "vision"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "vision"), nil
}

func Identify(dir string) (Project, error) {
	worktree, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Project{}, errors.New("vision must run inside a Git repository")
	}
	worktree, err = filepath.EvalSymlinks(worktree)
	if err != nil {
		return Project{}, err
	}
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Project{}, err
	}
	root := worktree
	if filepath.Base(common) == ".git" {
		root = filepath.Dir(common)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Project{}, err
	}
	remote, _ := git(dir, "config", "--get", "remote.origin.url")
	remote = canonicalRemote(remote)
	sum := sha256.Sum256([]byte(remote + "\n" + root))
	branch, _ := git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	sha, _ := git(dir, "rev-parse", "--short", "HEAD")
	status, _ := git(dir, "status", "--porcelain")
	return Project{ID: hex.EncodeToString(sum[:]), Name: filepath.Base(root), Root: root, Worktree: worktree, Branch: branch, SHA: sha, Dirty: status != ""}, nil
}

func canonicalRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.HasPrefix(remote, "git@") {
		if at := strings.IndexByte(remote, '@'); at >= 0 {
			if colon := strings.IndexByte(remote[at+1:], ':'); colon >= 0 {
				host := remote[at+1 : at+1+colon]
				remote = "https://" + host + "/" + remote[at+1+colon+1:]
			}
		}
	}
	remote = strings.TrimSuffix(remote, "/")
	remote = strings.TrimSuffix(remote, ".git")
	return strings.ToLower(remote)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}

func ProjectDir(id string) (string, error) {
	home, err := StateHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "projects", id), nil
}

func ShotPath(project, digest string) (string, error) {
	return ShotPathMedia(project, digest, "")
}

func ShotPathMedia(project, digest, media string) (string, error) {
	ext, err := MediaExtension(media)
	if err != nil {
		return "", err
	}
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shots", strings.TrimPrefix(digest, "sha256:")+ext), nil
}

func ElementsPath(project, digest string) (string, error) {
	path, err := ShotPath(project, digest)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".png") + ".json", nil
}

func BaselinePath(project, key, variant string) (string, error) {
	return BaselinePathMedia(project, key, variant, "")
}

func BaselinePathMedia(project, key, variant, media string) (string, error) {
	ext, err := MediaExtension(media)
	if err != nil {
		return "", err
	}
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "base", filepath.FromSlash(key)+"@"+variant+ext), nil
}

// HasShot reports whether a snap's picture is still on disk. The index outlives pictures by
// design (see Prune), so anything that has to show or count a shot asks this first rather than
// assuming the record and the file travel together.
func HasShot(project string, snap Snap) bool {
	p, err := ShotPathMedia(project, snap.Digest, snap.Media)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func AppendSnap(project string, snap Snap, data []byte) error {
	path, err := ShotPathMedia(project, snap.Digest, snap.Media)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	dir, _ := ProjectDir(project)
	return appendJSON(filepath.Join(dir, "index.jsonl"), snap)
}

func WriteElements(project, digest string, elements []Element) error {
	if len(elements) == 0 {
		return nil
	}
	path, err := ElementsPath(project, digest)
	if err != nil {
		return err
	}
	b, err := json.Marshal(elements)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		// Identical pixels keep the first DOM, so later captures cannot rewrite the snapshot.
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func ReadElements(project, digest string) ([]Element, error) {
	path, err := ElementsPath(project, digest)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var elements []Element
	if err := json.Unmarshal(b, &elements); err != nil {
		return nil, err
	}
	return elements, nil
}

func AppendNote(project string, note Note) error {
	dir, err := ProjectDir(project)
	if err != nil {
		return err
	}
	return appendJSON(filepath.Join(dir, "notes.jsonl"), note)
}

func AppendWithdrawal(project string, w Withdrawal) error {
	dir, err := ProjectDir(project)
	if err != nil {
		return err
	}
	return appendJSON(filepath.Join(dir, "withdrawn.jsonl"), w)
}

func appendJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func ReadJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1024*1024)
	for s.Scan() {
		var v T
		if err := json.Unmarshal(s.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, s.Err()
}

func Snaps(project string) ([]Snap, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return nil, err
	}
	return ReadJSONL[Snap](filepath.Join(dir, "index.jsonl"))
}

func Notes(project string) ([]Note, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return nil, err
	}
	return ReadJSONL[Note](filepath.Join(dir, "notes.jsonl"))
}

func Withdrawals(project string) ([]Withdrawal, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return nil, err
	}
	return ReadJSONL[Withdrawal](filepath.Join(dir, "withdrawn.jsonl"))
}

func NotesForSession(project, session string) ([]Note, error) {
	notes, err := Notes(project)
	if err != nil {
		return nil, err
	}
	thread, err := ActiveThreadFor(project, session)
	if err != nil {
		return nil, err
	}
	return notesForThread(project, thread, notes)
}

// Verdicts belong to content, not to a reader. Joining through the snap ledger keeps old
// notes usable without rewriting them, and lets both sessions see a verdict when they
// captured identical content. The join stays inside one project, just like the ledger.
func notesForThread(project, thread string, notes []Note) ([]Note, error) {
	snaps, err := Snaps(project)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]bool)
	for _, snap := range snaps {
		if snap.ThreadID() == thread {
			digests[snap.Digest] = true
		}
	}
	filtered := notes[:0]
	for _, note := range notes {
		if digests[note.Digest] {
			filtered = append(filtered, note)
		}
	}
	return filtered, nil
}

func ProjectIDs() ([]string, error) {
	home, err := StateHome()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(home, "projects"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

func CopyBaseline(project, key, variant, digest string) error {
	return CopyBaselineMedia(project, key, variant, digest, "")
}

func CopyBaselineMedia(project, key, variant, digest, media string) error {
	src, err := ShotPathMedia(project, digest, media)
	if err != nil {
		return err
	}
	dst, err := BaselinePathMedia(project, key, variant, media)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".baseline-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = io.Copy(tmp, in); err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(name, dst)
}

// KeepSuperseded is how many closed shots survive per key and variant, on top of
// the current baseline. Zero would make a mistaken ok unrecoverable, since the shot it
// replaced would be gone the moment it was replaced.
const KeepSuperseded = 2

// pruneGrace spares any picture younger than this. AppendSnap writes the picture before its
// index line, so a Prune triggered by a verdict in that gap would see a file with no record
// and delete a shot nobody has reviewed. A minute is far past that gap and far short of
// anything a human would notice as wasted disk.
const pruneGrace = time.Minute

// Prune deletes shot media and element snapshots that no longer serve anyone: not
// pending, not the current baseline, and older than the last KeepSuperseded closed shots
// for their key and variant. Superseded and withdrawn reviews enter the same retention
// window as decided reviews. All file types use the same digest and grace window.
// A shared file survives if any review on any thread still needs it.
//
// It never touches index.jsonl, notes.jsonl or withdrawn.jsonl. Those ledgers preserve
// every capture, human verdict and agent withdrawal even when its picture is reclaimed.
func Prune(project string) error {
	snaps, err := Snaps(project)
	if err != nil {
		return err
	}
	closed, err := Closed(project, snaps)
	if err != nil {
		return err
	}
	notes, err := Notes(project)
	if err != nil {
		return err
	}
	verdict := make(map[string]string, len(notes))
	for _, n := range notes {
		verdict[ReviewID(n.Key, n.Variant, n.Digest)] = n.Verdict
	}
	keep := make(map[string]bool)
	decided := make(map[string][]Snap)
	for _, s := range snaps {
		if !closed[s.Review()] {
			// Pending: the human has not looked at it yet, so it is the one thing
			// pruning must never take.
			keep[s.Digest] = true
			continue
		}
		group := s.Key + "@" + s.Variant
		decided[group] = append(decided[group], s)
	}
	for _, shots := range decided {
		for i := len(shots) - 1; i >= 0; i-- {
			s := shots[i]
			if verdict[ReviewID(s.Key, s.Variant, s.Digest)] == "ok" {
				keep[s.Digest] = true
				break
			}
		}
		// The window is the current baseline plus KeepSuperseded behind it, so a shot
		// that was just replaced is still there to compare against.
		window := KeepSuperseded + 1
		for i := len(shots) - 1; i >= 0 && len(shots)-i <= window; i-- {
			keep[shots[i].Digest] = true
		}
	}
	dir, err := ProjectDir(project)
	if err != nil {
		return err
	}
	shots := filepath.Join(dir, "shots")
	entries, err := os.ReadDir(shots)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		known := ext == ".json"
		for _, mediaExt := range mediaExtensions {
			known = known || ext == mediaExt
		}
		if e.IsDir() || !known {
			continue
		}
		if keep["sha256:"+strings.TrimSuffix(name, ext)] {
			continue
		}
		if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < pruneGrace {
			continue
		}
		if err := os.Remove(filepath.Join(shots, name)); err != nil {
			return err
		}
	}
	return nil
}

type cursor struct {
	Offset int `json:"offset"`
}

func UnreadNotes(project string) ([]Note, error) {
	return unreadNotes(project, "")
}

func UnreadNotesForSession(project, session string) ([]Note, error) {
	thread, err := ActiveThreadFor(project, session)
	if err != nil {
		return nil, err
	}
	return unreadNotes(project, thread)
}

func unreadNotes(project, thread string) ([]Note, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return nil, err
	}
	notes, err := Notes(project)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "cursor.json")
	if thread != "" {
		// Hashing keeps caller ids out of filesystem paths and preserves existing cursor
		// names when thread == session. Two live sessions deliberately on one thread
		// share one read cursor: a successor continues where its predecessor stopped.
		path = filepath.Join(dir, "cursors", identityFile(thread))
	}
	var c cursor
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
	}
	if c.Offset > len(notes) {
		c.Offset = len(notes)
	}
	out := notes[c.Offset:]
	if thread != "" {
		out, err = notesForThread(project, thread, out)
		if err != nil {
			return nil, err
		}
	}
	// Offsets count the full append-only ledger, even for a session view. Filtering before
	// advancing also means a failed join cannot mark verdicts read that we never returned.
	c.Offset = len(notes)
	b, _ := json.Marshal(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

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
	Branch        string            `json:"branch"`
	SHA           string            `json:"sha"`
	Dirty         bool              `json:"dirty"`
	Worktree      string            `json:"worktree"`
	Session       string            `json:"session"`
	Note          string            `json:"note,omitempty"`
	Conditions    Conditions        `json:"conditions"`
}

type Note struct {
	SchemaVersion int       `json:"schemaVersion"`
	TS            time.Time `json:"ts"`
	Key           string    `json:"key"`
	Variant       string    `json:"variant"`
	Digest        string    `json:"digest"`
	Verdict       string    `json:"verdict"`
	Note          string    `json:"note,omitempty"`
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
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shots", strings.TrimPrefix(digest, "sha256:")+".png"), nil
}

func BaselinePath(project, key, variant string) (string, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "base", filepath.FromSlash(key)+"@"+variant+".png"), nil
}

func AppendSnap(project string, snap Snap, png []byte) error {
	path, err := ShotPath(project, snap.Digest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		if _, err = f.Write(png); err == nil {
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

func AppendNote(project string, note Note) error {
	dir, err := ProjectDir(project)
	if err != nil {
		return err
	}
	return appendJSON(filepath.Join(dir, "notes.jsonl"), note)
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
	src, err := ShotPath(project, digest)
	if err != nil {
		return err
	}
	dst, err := BaselinePath(project, key, variant)
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

// KeepSuperseded is how many already-decided shots survive per key and variant, on top of
// the current baseline. Zero would make a mistaken ok unrecoverable, since the shot it
// replaced would be gone the moment it was replaced.
const KeepSuperseded = 2

// Prune deletes shot files that no longer serve anyone: not pending, not the current
// baseline, and older than the last KeepSuperseded decided shots for their key and variant.
//
// It never touches index.jsonl or notes.jsonl. Those are the record of what was shot and
// what the human said about it, they are tiny, and they stay append-only forever. This is
// why pruning does not weaken the append-only invariant: a pruned shot still appears in the
// index, and a flag still appears in the notes, so no queue can be made to look clean. Only
// the picture is reclaimed, never the fact that it existed.
func Prune(project string) error {
	snaps, err := Snaps(project)
	if err != nil {
		return err
	}
	notes, err := Notes(project)
	if err != nil {
		return err
	}
	verdict := make(map[string]string, len(notes))
	for _, n := range notes {
		verdict[n.Digest] = n.Verdict
	}
	keep := make(map[string]bool)
	decided := make(map[string][]string)
	for _, s := range snaps {
		if verdict[s.Digest] == "" {
			// Pending: the human has not looked at it yet, so it is the one thing
			// pruning must never take.
			keep[s.Digest] = true
			continue
		}
		group := s.Key + "@" + s.Variant
		decided[group] = append(decided[group], s.Digest)
	}
	for _, digests := range decided {
		for i := len(digests) - 1; i >= 0; i-- {
			if verdict[digests[i]] == "ok" {
				keep[digests[i]] = true
				break
			}
		}
		// The window is the current baseline plus KeepSuperseded behind it, so a shot
		// that was just replaced is still there to compare against.
		window := KeepSuperseded + 1
		for i := len(digests) - 1; i >= 0 && len(digests)-i <= window; i-- {
			keep[digests[i]] = true
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
		if e.IsDir() || !strings.HasSuffix(name, ".png") {
			continue
		}
		if keep["sha256:"+strings.TrimSuffix(name, ".png")] {
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
	dir, err := ProjectDir(project)
	if err != nil {
		return nil, err
	}
	notes, err := Notes(project)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "cursor.json")
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
	c.Offset = len(notes)
	b, _ := json.Marshal(c)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

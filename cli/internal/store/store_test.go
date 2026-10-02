package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Pruning is the one thing here that deletes, so it gets the sharpest test: the shot the
// human has not judged yet must survive, and the record of every shot must survive whether
// its picture did or not.
func TestPruneKeepsPendingBaselineAndRecent(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	shot := func(n int) string {
		digest := "sha256:" + strings.Repeat(string(rune('a'+n)), 8)
		snap := Snap{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: digest}
		if err := AppendSnap("p", snap, []byte{byte(n)}); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	approve := func(digest string) {
		if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: digest, Verdict: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	// Four approved in order, then one nobody has looked at.
	oldest, superseded, previous, baseline := shot(0), shot(1), shot(2), shot(3)
	for _, d := range []string{oldest, superseded, previous, baseline} {
		approve(d)
	}
	pending := shot(4)
	// Age every picture past the grace window, which is covered by its own test below.
	old := time.Now().Add(-2 * pruneGrace)
	for _, d := range []string{oldest, superseded, previous, baseline, pending} {
		p, _ := ShotPath("p", d)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	exists := func(digest string) bool { return HasShot("p", digest) }
	for _, keep := range []string{pending, baseline, previous, superseded} {
		if !exists(keep) {
			t.Errorf("pruned a shot it must keep: %s", keep)
		}
	}
	if exists(oldest) {
		t.Errorf("kept a shot past the retention window: %s", oldest)
	}
	// The ledger is untouched: every shot still has its record, picture or not.
	snaps, err := Snaps("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 5 {
		t.Fatalf("prune changed the index: got %d records, want 5", len(snaps))
	}
}

// A snap writes its picture before its index line. A Prune that lands in between sees a
// picture with no record, and must not delete a shot nobody has reviewed yet.
func TestPruneSparesAFreshPictureWithNoRecordYet(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	if err := AppendSnap("p", Snap{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: "sha256:aaaa"}, []byte{1}); err != nil {
		t.Fatal(err)
	}
	p, _ := ShotPath("p", "sha256:bbbb")
	if err := os.WriteFile(p, []byte{2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	if !HasShot("p", "sha256:bbbb") {
		t.Fatal("pruned a picture written moments ago, before its record landed")
	}
}

func TestParseKeyAndVariant(t *testing.T) {
	for _, key := range []string{"checkout/empty-cart", "checkout/happy-path#12"} {
		if err := ParseKey(key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range []string{"checkout", "/empty", "checkout/happy#0", "checkout/happy#x"} {
		if ParseKey(key) == nil {
			t.Errorf("accepted %s", key)
		}
	}
	if ParseVariant("mobile-dark") != nil || ParseVariant("mobile/dark") == nil {
		t.Fatal("variant validation wrong")
	}
	for _, variant := range []string{"unknown=value", "vp=mobile,vp=desktop", "vp=mobile,scheme=dark"} {
		if ParseVariant(variant) == nil {
			t.Errorf("accepted malformed encoded variant %q", variant)
		}
	}
}

func TestEncodeVariantCanonical(t *testing.T) {
	first := map[string]string{"scheme": "dark", "state": "empty", "vp": "mobile"}
	second := make(map[string]string)
	second["vp"] = "mobile"
	second["state"] = "empty"
	second["scheme"] = "dark"
	a, err := EncodeVariant(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeVariant(second)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != "scheme=dark,state=empty,vp=mobile" {
		t.Fatalf("encoded variants differ: %q %q", a, b)
	}
}

func TestVariantCodec(t *testing.T) {
	want := map[string]string{"case": "error", "locale": "ar", "role": "admin"}
	encoded, err := EncodeVariant(want)
	if err != nil {
		t.Fatal(err)
	}
	got := DecodeVariant(encoded)
	if len(got) != len(want) {
		t.Fatalf("decoded length: got %d, want %d", len(got), len(want))
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: got %q, want %q", key, got[key], value)
		}
	}
	for _, legacy := range []string{"default", "mobile-dark"} {
		if decoded := DecodeVariant(legacy); decoded != nil {
			t.Errorf("legacy %q decoded as %#v, want nil", legacy, decoded)
		}
	}
}

func TestEncodeVariantRejectsInvalidDimensions(t *testing.T) {
	tests := map[string]map[string]string{
		"unknown key":  {"schema": "dark"},
		"empty key":    {"": "dark"},
		"empty value":  {"scheme": ""},
		"equals key":   {"sche=me": "dark"},
		"comma key":    {"sche,me": "dark"},
		"equals value": {"scheme": "da=rk"},
		"comma value":  {"scheme": "da,rk"},
	}
	for name, dims := range tests {
		t.Run(name, func(t *testing.T) {
			if encoded, err := EncodeVariant(dims); err == nil {
				t.Fatalf("encoded invalid dimensions as %q", encoded)
			}
		})
	}
}

func TestViewportClassBoundaries(t *testing.T) {
	tests := map[int]string{599: "mobile", 600: "tablet", 1023: "tablet", 1024: "desktop"}
	for width, want := range tests {
		if got := ViewportClass(width); got != want {
			t.Errorf("width %d: got %q, want %q", width, got, want)
		}
	}
}

func TestProjectIdentitySharedByLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "x")
	runGit(t, root, "commit", "-m", "initial")
	runGit(t, root, "remote", "add", "origin", "git@github.com:Owner/Repo.git")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", linked, "-b", "linked")
	a, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Identify(linked)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Root != b.Root {
		t.Fatalf("identities differ: %#v %#v", a, b)
	}
}

func TestUnreadNotesCursor(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for i := 0; i < 2; i++ {
		if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: string(rune('a' + i)), Verdict: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := UnreadNotes("p")
	if err != nil || len(first) != 2 {
		t.Fatalf("first: %d %v", len(first), err)
	}
	second, err := UnreadNotes("p")
	if err != nil || len(second) != 0 {
		t.Fatalf("second: %d %v", len(second), err)
	}
	if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: "c", Verdict: "flag", Note: "bad"}); err != nil {
		t.Fatal(err)
	}
	third, err := UnreadNotes("p")
	if err != nil || len(third) != 1 || third[0].Digest != "c" {
		t.Fatalf("third: %#v %v", third, err)
	}
}

func TestSessionNotesAndIndependentCursors(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	session := "../../untrusted/session"
	for _, snap := range []Snap{
		{Session: session, Digest: "mine"},
		{Session: "other", Digest: "theirs"},
		{Session: "other", Digest: "shared"},
		{Session: session, Digest: "shared"},
		{Session: session, Digest: "shared"},
	} {
		if err := AppendSnap("p", snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A matching session in another project must not pull its digest into this view.
	if err := AppendSnap("elsewhere", Snap{Session: session, Digest: "foreign"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"mine", "theirs", "shared", "foreign"} {
		if err := AppendNote("p", Note{Digest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(read func() ([]Note, error), want ...string) {
		t.Helper()
		notes, err := read()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, note := range notes {
			got = append(got, note.Digest)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("notes: got %v, want %v", got, want)
		}
	}
	mine := func() ([]Note, error) { return UnreadNotesForSession("p", session) }
	theirs := func() ([]Note, error) { return UnreadNotesForSession("p", "other") }
	all := func() ([]Note, error) { return UnreadNotes("p") }
	check(all, "mine", "theirs", "shared", "foreign")
	dir, err := ProjectDir("p")
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "cursor.json")
	// An unreadable global cursor must not affect a session read at all. Keeping these
	// bytes intact also proves that a scoped read never advances the old shared cursor.
	if err := os.WriteFile(global, []byte("invalid global cursor"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(mine, "mine", "shared")
	check(mine)
	check(theirs, "theirs", "shared")
	if b, err := os.ReadFile(global); err != nil || string(b) != "invalid global cursor" {
		t.Fatalf("session touched global cursor: %q %v", b, err)
	}
	for _, id := range []string{session, "other"} {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
		b, err := os.ReadFile(filepath.Join(dir, "cursors", hash[:16]+".json"))
		if err != nil || string(b) != "{\"offset\":4}\n" {
			t.Fatalf("session cursor: %q %v", b, err)
		}
	}
	if err := os.WriteFile(global, []byte("{\"offset\":4}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendNote("p", Note{Digest: "shared", Verdict: "flag"}); err != nil {
		t.Fatal(err)
	}
	check(mine, "shared")
	check(all, "shared")
	check(theirs, "shared")
	check(func() ([]Note, error) { return NotesForSession("p", session) }, "mine", "shared", "shared")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

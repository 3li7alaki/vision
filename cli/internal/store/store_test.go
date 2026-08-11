package store

import (
	"os"
	"os/exec"
	"path/filepath"
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

	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	exists := func(digest string) bool {
		p, err := ShotPath("p", digest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(p)
		return err == nil
	}
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

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

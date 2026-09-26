package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThreadMapping(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := "../../session/a"
	if got, err := ThreadFor("p", id); err != nil || got != id {
		t.Fatalf("default: %q %v", got, err)
	}
	if got, err := JoinThread("p", id, id); err != nil || got != id {
		t.Fatalf("default no-op: %q %v", got, err)
	}
	dir, _ := ProjectDir("p")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("default no-op wrote state: %v", err)
	}
	if got, err := JoinThread("p", id, "work"); err != nil || got != "work" {
		t.Fatalf("join: %q %v", got, err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
	path := filepath.Join(dir, "sessions", hash[:16]+".json")
	if b, err := os.ReadFile(path); err != nil || string(b) != "{\"thread\":\"work\"}\n" {
		t.Fatalf("mapping: %q %v", b, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := JoinThread("p", id, "work"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatalf("idempotent join rewrote mapping: %v", err)
	}
	if got, err := ThreadFor("p", id); err != nil || got != "work" {
		t.Fatalf("mapped: %q %v", got, err)
	}
	if got, err := ThreadFor("elsewhere", id); err != nil || got != id {
		t.Fatalf("mapping leaked across projects: %q %v", got, err)
	}
	// Make the target itself a mapped session. Joining the predecessor still follows
	// exactly one hop; traversing again would move the successor to unrelated work.
	if _, err := JoinThread("p", "work", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if got, err := JoinThread("p", "b", id); err != nil || got != "work" {
		t.Fatalf("one-hop join: %q %v", got, err)
	}
	if got, err := JoinThread("p", "b", "work"); err != nil || got != "work" {
		t.Fatalf("current thread must stay a no-op: %q %v", got, err)
	}
	if got, err := ThreadFor("p", "b"); err != nil || got != "work" {
		t.Fatalf("one-hop read: %q %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 3 {
		t.Fatalf("temporary mapping leaked: %v %v", entries, err)
	}
}

func TestJoinThreadValidation(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for _, bad := range []string{"", strings.Repeat("x", 201), strings.Repeat("é", 101), "a\nb", "a\rb", "a\tb", "a\x00b", "a\x7fb", "a\u0085b", "a\u2028b", "a\u2029b", "\xff"} {
		if _, err := JoinThread("p", "a", bad); err == nil {
			t.Errorf("accepted invalid thread %q", bad)
		}
	}
	if _, err := JoinThread("p", "", "work"); err == nil {
		t.Fatal("accepted empty session")
	}
	dir, _ := ProjectDir("p")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("invalid join wrote state: %v", err)
	}
	for _, good := range []string{strings.Repeat("é", 100), "../../work / thread"} {
		if got, err := JoinThread("p", "a", good); err != nil || got != good {
			t.Fatalf("valid thread %q: %q %v", good, got, err)
		}
	}
}

func TestThreadForRefusesBrokenMapping(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	if _, err := JoinThread("p", "a", "work"); err != nil {
		t.Fatal(err)
	}
	dir, _ := ProjectDir("p")
	path := filepath.Join(dir, "sessions", identityFile("a"))
	for _, body := range []string{"broken", `{}`, `{"thread":""}`, `{"thread":"bad\nthread"}`} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ThreadFor("p", "a"); err == nil {
			t.Errorf("silently defaulted broken mapping %q", body)
		}
		if _, err := JoinThread("p", "b", "a"); err == nil {
			t.Errorf("joined broken mapping %q", body)
		}
	}
}

func TestThreadCursorContinuity(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for _, snap := range []Snap{
		{Session: "a", Digest: "old"},
		{Session: "b", Thread: "a", Digest: "new"},
		{Session: "a", Thread: "different", Digest: "moved"},
	} {
		if err := AppendSnap("p", snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendNote("p", Note{Digest: "old"}); err != nil {
		t.Fatal(err)
	}
	first, err := UnreadNotesForSession("p", "a")
	if err != nil || len(first) != 1 || first[0].Digest != "old" {
		t.Fatalf("predecessor: %v %v", first, err)
	}
	if _, err := JoinThread("p", "b", "a"); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"new", "moved"} {
		if err := AppendNote("p", Note{Digest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	second, err := UnreadNotesForSession("p", "b")
	if err != nil || len(second) != 1 || second[0].Digest != "new" {
		t.Fatalf("successor replayed or lost notes: %v %v", second, err)
	}
	for _, id := range []string{"a", "b", "unrelated"} {
		if notes, err := UnreadNotesForSession("p", id); err != nil || len(notes) != 0 {
			t.Fatalf("%s unread: %v %v", id, notes, err)
		}
	}
	for _, id := range []string{"a", "b"} {
		if notes, err := NotesForSession("p", id); err != nil || len(notes) != 2 || notes[0].Digest != "old" || notes[1].Digest != "new" {
			t.Fatalf("%s all: %v %v", id, notes, err)
		}
	}
	if notes, err := NotesForSession("p", "unrelated"); err != nil || len(notes) != 0 {
		t.Fatalf("unrelated notes: %v %v", notes, err)
	}
}

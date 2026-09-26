package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
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
		if _, err := JoinThread("p", "a", "work"); err == nil {
			t.Errorf("overwrote broken mapping %q", body)
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

func TestOpenFlags(t *testing.T) {
	for _, tc := range []struct {
		name, variant, thread, digest string
		resnap                        bool
		delta                         time.Duration
		want                          int
	}{
		{name: "open", want: 1},
		{name: "different digest closes", resnap: true, variant: "mobile", thread: "work", digest: "new", delta: time.Second},
		{name: "same digest closes", resnap: true, variant: "mobile", thread: "work", digest: "old", delta: time.Second},
		{name: "other variant", resnap: true, variant: "desktop", thread: "work", digest: "new", delta: time.Second, want: 1},
		{name: "other thread", resnap: true, variant: "mobile", thread: "other", digest: "new", delta: time.Second, want: 1},
		{name: "equal timestamp", resnap: true, variant: "mobile", thread: "work", digest: "new", want: 1},
		{name: "older resnap", resnap: true, variant: "mobile", thread: "work", digest: "new", delta: -time.Second, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			// No Thread field: old snaps must retain their session as thread identity.
			if err := AppendSnap("p", Snap{Session: "work", Key: "cart/empty", Variant: "mobile", Digest: "old", TS: ts.Add(-time.Hour)}, nil); err != nil {
				t.Fatal(err)
			}
			for _, note := range []Note{
				{Key: "cart/empty", Variant: "mobile", Digest: "old", Verdict: "flag", Note: "latest", TS: ts},
				{Key: "cart/empty", Variant: "mobile", Digest: "old", Verdict: "flag", Note: "older", TS: ts.Add(-time.Minute)},
				{Key: "cart/other", Variant: "mobile", Digest: "unrelated", Verdict: "flag", TS: ts},
				{Key: "cart/empty", Variant: "mobile", Digest: "old", Verdict: "ok", TS: ts.Add(time.Minute)},
			} {
				if err := AppendNote("p", note); err != nil {
					t.Fatal(err)
				}
			}
			if tc.resnap {
				if err := AppendSnap("p", Snap{Session: "successor", Thread: tc.thread, Key: "cart/empty", Variant: tc.variant, Digest: tc.digest, TS: ts.Add(tc.delta)}, nil); err != nil {
					t.Fatal(err)
				}
			}
			got, err := OpenFlags("p", "work")
			if err != nil || len(got) != tc.want {
				t.Fatalf("open flags: %v %v, want %d", got, err, tc.want)
			}
			if tc.want > 0 && got[0].Note != "latest" {
				t.Fatalf("kept wrong flag: %+v", got)
			}
		})
	}
}

func TestSweepSessionState(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	now := time.Now().Truncate(time.Second)
	old := now.Add(-SessionStateMaxAge - time.Second)
	cutoff := now.Add(-SessionStateMaxAge)
	if n, err := SweepSessionState(now, SessionStateMaxAge); err != nil || n != 0 {
		t.Fatalf("empty sweep: %d %v", n, err)
	}
	for _, project := range []string{"p", "q"} {
		dir, _ := ProjectDir(project)
		for _, name := range []string{"sessions/old.json", "cursors/old.json", "sessions/fresh.json", "cursors/fresh.json", "sessions/boundary.json", "index.jsonl", "notes.jsonl", "cursor.json", "shots/old.png", "base/cart/empty@mobile.png"} {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			mtime := old
			if strings.Contains(name, "fresh") {
				mtime = now
			} else if strings.Contains(name, "boundary") {
				mtime = cutoff
			}
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := SweepSessionState(now, SessionStateMaxAge); err != nil || n != 4 {
		t.Fatalf("sweep: %d %v, want 4", n, err)
	}
	for _, project := range []string{"p", "q"} {
		dir, _ := ProjectDir(project)
		for _, name := range []string{"sessions/old.json", "cursors/old.json"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Errorf("expired %s survived: %v", name, err)
			}
		}
		for _, name := range []string{"sessions/fresh.json", "cursors/fresh.json", "sessions/boundary.json", "index.jsonl", "notes.jsonl", "cursor.json", "shots/old.png", "base/cart/empty@mobile.png"} {
			if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != name {
				t.Errorf("protected %s changed: %q %v", name, b, err)
			}
		}
	}
}

func TestThreadReadsAndExpiredCursor(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	if _, err := JoinThread("p", "a", "work"); err != nil {
		t.Fatal(err)
	}
	dir, _ := ProjectDir("p")
	path := filepath.Join(dir, "sessions", identityFile("a"))
	old := time.Now().Add(-SessionStateMaxAge - time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := ThreadFor("p", "a"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("ThreadFor wrote mapping: %v", err)
	}
	if _, err := ActiveThreadFor("p", "a"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().After(old) {
		t.Fatalf("active read did not touch mapping: %v", err)
	}
	if err := AppendSnap("p", Snap{Session: "a", Thread: "work", Digest: "old", TS: old}, nil); err != nil {
		t.Fatal(err)
	}
	if err := AppendNote("p", Note{Digest: "old", Verdict: "flag", TS: old}); err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(dir, "cursors", identityFile("work"))
	for i := 0; i < 2; i++ {
		notes, err := UnreadNotesForSession("p", "a")
		if err != nil || len(notes) != 1 {
			t.Fatalf("missing cursor must replay old notes: %v %v", notes, err)
		}
		if err := os.Chtimes(cursorPath, old, old); err != nil {
			t.Fatal(err)
		}
		if n, err := SweepSessionState(time.Now(), SessionStateMaxAge); err != nil || n != 1 {
			t.Fatalf("expired cursor sweep: %d %v", n, err)
		}
	}
}

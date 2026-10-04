package main

import (
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vision/internal/server"
	"vision/internal/store"
)

func TestSessionCommandsAndHandoff(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	t.Setenv("VISION_SESSION_ID", "a")
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	command := func(want string, args ...string) {
		t.Helper()
		out, err := commandOutput(t, func() error { return run(args) })
		if err != nil || strings.TrimSpace(out) != want {
			t.Fatalf("%v: %q %v, want %q", args, out, err, want)
		}
	}
	command("a", "session", "thread")
	for _, snap := range []store.Snap{
		{Session: "a", Digest: "old"},
		{Session: "a", Thread: "a", Digest: "new"},
		{Session: "a", Thread: "elsewhere", Digest: "elsewhere"},
	} {
		if err := store.AppendSnap(project.ID, snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendNote(project.ID, store.Note{Digest: "old", Verdict: "ok"}); err != nil {
		t.Fatal(err)
	}
	notesFor := func(id string, unread bool, want ...string) {
		t.Helper()
		args := []string{"notes", "--session", id, "--json"}
		if unread {
			args = append(args, "--unread")
		}
		out, err := commandOutput(t, func() error { return run(args) })
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Notes []store.Note }
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Notes) != len(want) {
			t.Fatalf("%v: %s, want %v", args, out, want)
		}
		for i, digest := range want {
			if result.Notes[i].Digest != digest {
				t.Fatalf("%v: %s, want %v", args, out, want)
			}
		}
	}
	notesFor("a", true, "old")
	t.Setenv("VISION_SESSION_ID", "b")
	command("joined a", "session", "join", "a")
	command("joined a", "session", "join", "a")
	command("a", "session", "thread")
	command(`{"schemaVersion":1,"session":"b","snaps":2,"thread":"a"}`, "session", "thread", "--json")

	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	handler := server.New().Handler()
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	for id, want := range map[string]int{"a": 1, "b": 1, "unrelated": 0} {
		out, err := commandOutput(t, func() error { return run([]string{"status", "--session", id, "--json"}) })
		var result struct{ Pending int }
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil || result.Pending != want {
			t.Fatalf("status %s: %s %v, want %d", id, out, err, want)
		}
	}
	if err := store.AppendNote(project.ID, store.Note{Digest: "new", Verdict: "flag", Note: "fix"}); err != nil {
		t.Fatal(err)
	}
	notesFor("b", true, "new")
	notesFor("a", true)
	notesFor("b", false, "old", "new")
	notesFor("unrelated", false)
	notesFor("unrelated", true)
	t.Setenv("VISION_SESSION_ID", "c")
	command("joined a", "session", "join", "a")
	command("a", "session", "thread")
}

func TestSessionCommandErrors(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for _, name := range []string{"VISION_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		t.Setenv(name, "")
	}
	for _, args := range [][]string{{"session", "join", "work"}, {"session", "thread"}, {"session", "thread", "--json"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "no session id") {
			t.Errorf("%v: %v", args, err)
		}
	}
	t.Setenv("VISION_SESSION_ID", "a")
	for _, args := range [][]string{
		{"session"}, {"session", "unknown"}, {"session", "join"},
		{"session", "join", "a", "extra"}, {"session", "thread", "extra"},
		{"session", "thread", "--unknown"},
	} {
		if err := run(args); err == nil || err.Error() != usageText {
			t.Errorf("%v: %v, want usage", args, err)
		}
	}
	for _, bad := range []string{"", "a\nb", strings.Repeat("x", 201)} {
		if err := run([]string{"session", "join", bad}); err == nil {
			t.Errorf("accepted thread %q", bad)
		}
	}
}

func TestSnapRecordsThread(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.JoinThread(project.ID, "successor", "predecessor"); err != nil {
		t.Fatal(err)
	}
	// Exercise CLI, request JSON, daemon and ledger together. The fake PinchTab only
	// supplies a PNG; no browser or installed daemon is touched by this regression check.
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.png")
	f, err := os.Create(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    cp "$VISION_TEST_PNG" "$2" || exit 1
    break
  fi
  shift
done
printf '%s\n' '{"url":"http://localhost/cart","colorScheme":"light","image":{"viewport":{"w":1,"h":1}}}'
`
	if err := os.WriteFile(filepath.Join(dir, "pinchtab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISION_TEST_PNG", fixture)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	handler := server.New().Handler()
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	for _, tc := range []struct{ session, thread string }{{"fresh", "fresh"}, {"successor", "predecessor"}, {"", ""}} {
		t.Setenv("VISION_SESSION_ID", tc.session)
		if _, err := commandOutput(t, func() error { return run([]string{"snap", "cart/thread", "--json"}) }); err != nil {
			t.Fatal(err)
		}
		snaps, err := store.Snaps(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := snaps[len(snaps)-1]
		if got.Session != tc.session || got.Thread != tc.thread {
			t.Fatalf("captured session=%q thread=%q, want %+v", got.Session, got.Thread, tc)
		}
	}
}

func TestOpenFlagsCommands(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.JoinThread(project.ID, "successor", "work"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.AppendSnap(project.ID, store.Snap{Session: "work", Key: "cart/empty", Variant: "mobile", Digest: "flagged", TS: now.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendNote(project.ID, store.Note{Key: "cart/empty", Variant: "mobile", Digest: "flagged", Verdict: "flag", Note: "fix CTA", TS: now}); err != nil {
		t.Fatal(err)
	}
	dir, _ := store.ProjectDir(project.ID)
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("mapping: %v %v", entries, err)
	}
	mapping := filepath.Join(dir, "sessions", entries[0].Name())
	checkOpen := func() {
		t.Helper()
		out, err := commandOutput(t, func() error {
			return run([]string{"notes", "--session", "successor", "--open", "--json"})
		})
		var result struct{ Notes []store.Note }
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Notes) != 1 || result.Notes[0].Note != "fix CTA" {
			t.Fatalf("open: %s %v", out, err)
		}
	}
	checkOpen()
	for _, name := range []string{"cursor.json", "cursors"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("--open created %s: %v", name, err)
		}
	}
	if _, err := store.UnreadNotesForSession(project.ID, "successor"); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(filepath.Join(dir, "cursors"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("cursor: %v %v", entries, err)
	}
	// Corrupt cursors prove --open never reads them, and unchanged bytes and mtimes
	// prove it never advances them. This applies to both cursor scopes.
	paths := []string{filepath.Join(dir, "cursor.json"), filepath.Join(dir, "cursors", entries[0].Name())}
	old := now.Add(-store.SessionStateMaxAge - time.Hour).Truncate(time.Second)
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("not JSON"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	checkOpen()
	checkOpen()
	for _, path := range paths {
		if b, err := os.ReadFile(path); err != nil || string(b) != "not JSON" {
			t.Fatalf("--open changed cursor: %q %v", b, err)
		}
		if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
			t.Fatalf("--open touched cursor: %v", err)
		}
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		server.New().Handler().ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	for _, args := range [][]string{
		{"notes", "--session", "successor", "--open", "--json"},
		{"notes", "--session", "successor", "--json"},
		{"notes", "--session", "successor", "--unread", "--json"},
		{"status", "--session", "successor", "--json"},
	} {
		// Restore valid cursors for unread. Neither notes nor status may sweep them.
		for _, path := range paths {
			if err := os.WriteFile(path, []byte(`{"offset":0}`), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(mapping, old, old); err != nil {
			t.Fatal(err)
		}
		out, err := commandOutput(t, func() error { return run(args) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if args[0] == "status" {
			var result struct{ OpenFlags *int }
			if err := json.Unmarshal([]byte(out), &result); err != nil || result.OpenFlags == nil || *result.OpenFlags != 1 {
				t.Fatalf("status openFlags: %s %v", out, err)
			}
		}
		if info, err := os.Stat(mapping); err != nil || !info.ModTime().After(old) {
			t.Fatalf("%v did not refresh mapping: %v", args, err)
		}
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("CLI swept cursor: %v", err)
			}
		}
	}
}

func TestNotesOpenUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--open"}, {"--open", "--session="},
		{"--session", "a", "--open", "--unread"},
		{"--session", "a", "--open", "--unread=false"},
		{"--session", "a", "--open", "--since", "1h"},
		{"--session", "a", "--open", "--since=0"},
	} {
		if err := notes(args); err == nil || err.Error() != usageText {
			t.Errorf("%v: %v, want usage", args, err)
		}
	}
}

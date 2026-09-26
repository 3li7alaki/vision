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
	command("joined a", "session", "join", "b")
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
printf '%s\n' '{"url":"http://localhost/cart","colorScheme":"light"}'
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

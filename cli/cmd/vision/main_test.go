package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"vision/internal/store"
)

func TestMainHelp(t *testing.T) {
	bin := buildBin(t)
	for _, spelling := range []string{"-h", "--help", "help"} {
		t.Run(spelling, func(t *testing.T) {
			cmd := exec.Command(bin, spelling)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("help must exit 0: %v (stderr=%q)", err, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("help must print to stdout (stderr=%q)", stderr.String())
			}
		})
	}
}

func TestMainUnknownArgument(t *testing.T) {
	bin := buildBin(t)
	cmd := exec.Command(bin, "definitely-not-a-command")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) {
		t.Fatalf("an unrecognized argument must exit non-zero: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("an unrecognized argument must not write stdout: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("an unrecognized argument must print a usage message to stderr")
	}
}

// A capture that could not report its scheme or its viewport is the normal failure here, not
// an exotic one: the browser this talks to drops evals and returns responses with no viewport
// under load. What must never happen is that gap turning into a missing dimension (which
// shares one baseline across both schemes) or into a confident wrong bucket.
func TestDeriveDims(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dims       map[string]string
		conditions store.Conditions
		want       map[string]string
	}{
		{
			name:       "measured",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{Scheme: "dark", Width: 390},
			want:       map[string]string{"flow": "payment", "scheme": "dark", "vp": "mobile"},
		},
		{
			name:       "explicit wins over measured",
			dims:       map[string]string{"scheme": "light", "vp": "desktop"},
			conditions: store.Conditions{Scheme: "dark", Width: 390},
			want:       map[string]string{"scheme": "light", "vp": "desktop"},
		},
		{
			name:       "browser told us nothing",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{},
			want:       map[string]string{"flow": "payment", "scheme": "unknown", "vp": "unknown"},
		},
		{
			name:       "negative width is not mobile",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{Scheme: "light", Width: -1},
			want:       map[string]string{"flow": "payment", "scheme": "light", "vp": "unknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deriveDims(tc.dims, tc.conditions)
			if !maps.Equal(tc.dims, tc.want) {
				t.Fatalf("deriveDims produced %v, want %v", tc.dims, tc.want)
			}
			// Whatever it produced has to name a baseline, or a flaky capture takes a
			// successful screenshot and then throws it away at the last step.
			if _, err := store.EncodeVariant(tc.dims); err != nil {
				t.Fatalf("derived dimensions must always encode: %v", err)
			}
		})
	}
}

func buildBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

func TestSessionIDPrecedence(t *testing.T) {
	for _, tc := range []struct{ vision, claude, codex, want string }{
		{"explicit", "claude", "codex", "explicit"},
		{"", "claude", "codex", "claude"},
		{"", "", "codex", "codex"},
		{"", "", "", ""},
	} {
		t.Setenv("VISION_SESSION_ID", tc.vision)
		t.Setenv("CLAUDE_CODE_SESSION_ID", tc.claude)
		t.Setenv("CODEX_THREAD_ID", tc.codex)
		if got := sessionID(); got != tc.want {
			t.Errorf("sessionID() = %q, want %q", got, tc.want)
		}
	}
}

func TestSessionFlagRejectsEmpty(t *testing.T) {
	for _, command := range []string{"notes", "status"} {
		for _, args := range [][]string{{"--session", ""}, {"--session="}, {"--session"}} {
			if err := run(append([]string{command}, args...)); err == nil || err.Error() != usageText {
				t.Errorf("%s %q: got %v, want usage error", command, args, err)
			}
		}
	}
}

func TestNotesSessionFilters(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, row := range []struct {
		digest, session, verdict string
		ts                       time.Time
	}{
		{"old", "mine", "flag", now.Add(-2 * time.Hour)},
		{"ok", "mine", "ok", now},
		{"flag", "mine", "flag", now},
		{"other", "other", "flag", now},
		{"anonymous", "", "flag", now},
	} {
		if err := store.AppendSnap(project.ID, store.Snap{Digest: row.digest, Session: row.session}, nil); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendNote(project.ID, store.Note{Digest: row.digest, Verdict: row.verdict, TS: row.ts}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--session", "mine"}, []string{"old", "ok", "flag"}},
		{[]string{"--session", "mine", "--since", "1h", "--flagged"}, []string{"flag"}},
		{[]string{"--session", "missing"}, nil},
		{[]string{"--session", "mine", "--unread", "--flagged"}, []string{"old", "flag"}},
		{[]string{"--session", "mine", "--unread"}, nil},
		{[]string{"--session", "other", "--unread"}, []string{"other"}},
		{[]string{"--unread"}, []string{"old", "ok", "flag", "other", "anonymous"}},
	} {
		out, err := commandOutput(t, func() error { return notes(append(tc.args, "--json")) })
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Notes []store.Note }
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, note := range result.Notes {
			got = append(got, note.Digest)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("notes %v: got %v, want %v", tc.args, got, tc.want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStatusSession(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"mine", "other"} {
		if err := store.AppendSnap(project.ID, store.Snap{Digest: session, Session: session}, nil); err != nil {
			t.Fatal(err)
		}
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, running := range []bool{true, false} {
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > 300*time.Millisecond {
				t.Error("status lost its 300ms timeout")
			}
			if !running {
				return nil, errors.New("daemon off")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"pending":9}`)), Header: make(http.Header)}, nil
		})
		out, err := commandOutput(t, func() error { return status([]string{"--session", "mine", "--json"}) })
		if (err == nil) != running {
			t.Fatalf("running=%v: unexpected status error %v", running, err)
		}
		var result struct {
			Running             bool
			Session             string
			Pending, PendingAll int
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		want := 0
		if running {
			want = 1
			if result.PendingAll != 9 {
				t.Errorf("lost daemon count: %s", out)
			}
		}
		if result.Session != "mine" || result.Running != running || result.Pending != want {
			t.Errorf("unexpected status: %s", out)
		}
	}
}

func commandOutput(t *testing.T, command func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	original := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = original }()
	commandErr := command()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), commandErr
}

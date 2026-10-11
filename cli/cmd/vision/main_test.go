package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vision/internal/server"
	"vision/internal/store"
)

func TestViewURL(t *testing.T) {
	for _, tc := range []struct {
		project, thread, want string
	}{
		{"", "", galleryURL},
		{"shop", "", galleryURL + "/p/shop"},
		{"shop", "abc", galleryURL + "/p/shop/t/abc"},
		{"odd/project name", "a/b c", galleryURL + "/p/odd%2Fproject%20name/t/a%2Fb%20c"},
		{"shop", "a?#%", galleryURL + "/p/shop/t/a%3F%23%25"},
	} {
		if got := viewURL(tc.project, tc.thread); got != tc.want {
			t.Errorf("viewURL(%q, %q) = %q, want %q", tc.project, tc.thread, got, tc.want)
		}
	}
}

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
			if !strings.Contains(stdout.String(), "[--as <variant> | --dim k=v...] [--selector <css>]") {
				t.Fatalf("help lost the selector flag: %q", stdout.String())
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

func TestViewportClaims(t *testing.T) {
	for _, tc := range []struct {
		variant   string
		dims      map[string]string
		width     int
		wantClaim string
		wantError string
	}{
		{"mobile-dark", nil, 2560, "mobile", "variant says mobile but the viewport is 2560x1297 (desktop); set the viewport first (pinchtab set viewport 390 844), then snap again"},
		{"mobile-dark", nil, 390, "mobile", ""},
		{"case=ar,scheme=dark,vp=desktop", nil, 1440, "desktop", ""},
		{"default", map[string]string{"vp": "mobile"}, 1440, "mobile", "pinchtab set viewport 390 844"},
		{"default", map[string]string{"vp": "tablet"}, 1440, "tablet", "pinchtab set viewport 820 1180"},
		{"default", map[string]string{"vp": "desktop"}, 390, "desktop", "pinchtab set viewport 1440 900"},
		{"default", nil, 2560, "", ""},
		{"mobile-vs-desktop", nil, 2560, "", ""},
		{"mobile-dark", nil, 0, "mobile", ""},
		{"mobile-dark", nil, -1, "mobile", ""},
		{"MOBILE_DARK", nil, 390, "mobile", ""},
		{"dark@tablet.light", nil, 820, "tablet", ""},
		{"automobile", nil, 1440, "", ""},
		{"mobile-mobile", nil, 390, "mobile", ""},
		{"mobile-dark", map[string]string{"flow": "payment"}, 1440, "", ""},
		{"mobile-dark", map[string]string{"vp": "desktop"}, 1440, "desktop", ""},
	} {
		t.Run(tc.variant+"/"+tc.wantClaim, func(t *testing.T) {
			claim := viewportClaim(tc.variant, tc.dims)
			if claim != tc.wantClaim {
				t.Fatalf("viewportClaim(%q, %v) = %q, want %q", tc.variant, tc.dims, claim, tc.wantClaim)
			}
			err := checkViewportClaim(claim, store.Conditions{Width: tc.width, Height: 1297})
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected mismatch: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestSelectorValidation(t *testing.T) {
	for _, tc := range []struct {
		name, selector string
		valid          bool
	}{
		{"empty", "", false},
		{"newline", "footer\n", false},
		{"tab", "footer\t", false},
		{"NUL", "footer\x00", false},
		{"unicode control", "footer\u0085", false},
		{"too long", strings.Repeat("a", 501), false},
		{"byte limit", strings.Repeat("é", 251), false},
		{"at limit", strings.Repeat("a", 500), true},
		{"CSS", `footer [data-role="card"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSelector(tc.selector); (err == nil) != tc.valid {
				t.Fatalf("validateSelector(%q) = %v, valid=%v", tc.selector, err, tc.valid)
			}
			if !tc.valid {
				if err := snap([]string{"capture/footer", "--selector=" + tc.selector}); err == nil || !strings.Contains(err.Error(), "selector must") {
					t.Fatalf("snap did not reject the selector before capture: %v", err)
				}
			}
		})
	}
}

func TestSnapChecksViewportBeforePosting(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	t.Setenv("VISION_SESSION_ID", "snap-test")
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.JoinThread(project.ID, "snap-test", "a/b c"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1440, 900))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && cp '" + path + "' \"$2\"; shift; done\nprintf '%s\\n' '{\"url\":\"http://localhost:3000/cart\",\"colorScheme\":\"light\",\"image\":{\"coordinateSpace\":\"viewport\",\"viewport\":{\"w\":1440,\"h\":900}}}'\n"
	if err := os.WriteFile(filepath.Join(dir, "pinchtab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, tc := range []struct {
		args     []string
		wantPost bool
	}{
		{[]string{"--as", "mobile-dark"}, false},
		{[]string{"--dim", "vp=mobile"}, false},
		{[]string{"--as", "mobile-dark", "--selector", "footer"}, false},
		{[]string{"--dim", "flow=payment", "--selector", "footer"}, true},
		{[]string{"--dim", "flow=payment", "--selector", "footer", "--json"}, true},
	} {
		posts := 0
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{}`
			if r.Method == http.MethodPost {
				posts++
				var req server.SnapRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				if req.Dims["vp"] != "desktop" || req.Capture.Selector != "footer" || req.Thread != "a/b c" {
					t.Errorf("lost derived viewport or selector: %#v", req)
				}
				body = `{"schemaVersion":1,"digest":"sha256:test","queued":true}`
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		out, err := commandOutput(t, func() error { return snap(append([]string{"capture/footer"}, tc.args...)) })
		if (posts == 1) != tc.wantPost || (err == nil) != tc.wantPost {
			t.Fatalf("snap %v: posts=%d err=%v", tc.args, posts, err)
		}
		if !tc.wantPost && !strings.Contains(err.Error(), "variant says mobile") {
			t.Fatalf("snap failed before the mismatch check: %v", err)
		}
		if tc.wantPost {
			want := galleryURL + "/p/" + project.Name + "/t/a%2Fb%20c"
			if slices.Contains(tc.args, "--json") {
				var result map[string]any
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatal(err)
				}
				if result["url"] != want || result["digest"] != "sha256:test" || result["queued"] != true {
					t.Errorf("snap JSON lost result fields or thread URL: %s", out)
				}
			} else if !strings.HasPrefix(out, "queued capture/footer@") || !strings.HasSuffix(out, "\n"+want+"\n") {
				t.Errorf("queued snap lost thread URL: %q", out)
			}
		}
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

func TestNotesWaitUsage(t *testing.T) {
	t.Setenv("VISION_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	for _, args := range [][]string{
		{"--wait", "1s", "--session", "mine", "--unread"},
		{"--wait", "1s", "--session", "mine", "--unread=false"},
		{"--wait", "1s", "--session", "mine", "--since=0"},
		{"--wait", "1s", "--session", "mine", "--open"},
		{"--wait", "1s", "--session", "mine", "--open=false"},
		{"--wait", "0", "--session", "mine"},
		{"--wait", "-1s", "--session", "mine"},
		{"--wait", "25h", "--session", "mine"},
		{"--wait", "1s"},
	} {
		if err := notes(args); err == nil || err.Error() != usageText {
			t.Errorf("notes %v: err=%v, want usage error", args, err)
		}
	}
}

func TestNotesWaitReceivesThreadVerdict(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			t.Setenv("VISION_SESSION_ID", "successor")
			project, err := store.Identify(".")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.JoinThread(project.ID, "successor", "shared"); err != nil {
				t.Fatal(err)
			}
			for _, snap := range []store.Snap{
				{Key: "cart/empty", Variant: "default", Digest: "mine", Session: "predecessor", Thread: "shared"},
				{Key: "cart/other", Variant: "default", Digest: "other", Session: "other"},
			} {
				if err := store.AppendSnap(project.ID, snap, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.AppendNote(project.ID, store.Note{Key: "cart/other", Variant: "default", Digest: "other", Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
			note := store.Note{Key: "cart/empty", Variant: "default", Digest: "mine", Verdict: "flag", Note: "fix"}
			appended := make(chan error, 1)
			go func() {
				time.Sleep(200 * time.Millisecond)
				appended <- store.AppendNote(project.ID, note)
			}()
			args := []string{"--wait", "4s", "--flagged"}
			if explicit {
				t.Setenv("VISION_SESSION_ID", "wrong-session")
				args = append(args, "--session", "successor")
			} else {
				args = append(args, "--json")
			}
			started := time.Now()
			out, waitErr := commandOutput(t, func() error { return notes(args) })
			if err := <-appended; err != nil {
				t.Fatal(err)
			}
			if waitErr != nil || time.Since(started) >= 4*time.Second {
				t.Fatalf("wait did not return after the verdict: output=%q err=%v", out, waitErr)
			}
			if explicit {
				if out != "flag cart/empty@default: mine fix\n" {
					t.Fatalf("wait text=%q", out)
				}
			} else {
				var result struct{ Notes []store.Note }
				if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Notes) != 1 || !reflect.DeepEqual(result.Notes[0], note) {
					t.Fatalf("wait JSON=%s err=%v", out, err)
				}
			}
			if values, err := store.UnreadNotesForSession(project.ID, "successor"); err != nil || len(values) != 0 {
				t.Fatalf("wait did not share unread cursor: %v %v", values, err)
			}
		})
	}
}

func TestNotesWaitTimeout(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	out, err := commandOutput(t, func() error { return notes([]string{"--wait", "40ms", "--session", "mine"}) })
	if err == nil || err.Error() != "no new verdicts after 40ms" || out != "" {
		t.Fatalf("timeout: output=%q err=%v", out, err)
	}
}

func TestNotesWaitUsesSharedCursor(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSnap(project.ID, store.Snap{Digest: "mine", Session: "mine"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendNote(project.ID, store.Note{Digest: "mine", Verdict: "ok"}); err != nil {
		t.Fatal(err)
	}
	out, err := commandOutput(t, func() error { return notes([]string{"--wait", "1s", "--session", "mine", "--flagged"}) })
	if err != nil || out != "" {
		t.Fatalf("wait did not filter the unread ok: output=%q err=%v", out, err)
	}
	if values, err := store.UnreadNotesForSession(project.ID, "mine"); err != nil || len(values) != 0 {
		t.Fatalf("filtered ok was not consumed: %v %v", values, err)
	}
}

func TestDiscard(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, variant := range []string{"default", "case=ar,vp=mobile"} {
		t.Run(variant, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			t.Setenv("VISION_SESSION_ID", "successor")
			project, err := store.Identify(".")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.JoinThread(project.ID, "successor", "shared"); err != nil {
				t.Fatal(err)
			}
			snap := store.Snap{Key: "cart/empty", Variant: variant, Digest: "sha256:aaaa", Session: "predecessor", Thread: "shared"}
			if err := store.AppendSnap(project.ID, snap, nil); err != nil {
				t.Fatal(err)
			}
			handler := server.New().Handler()
			var paths []string
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				return rec.Result(), nil
			})
			args := []string{"discard", snap.Key}
			if variant != "default" {
				args = append(args, "--as", variant, "--json")
			}
			out, err := commandOutput(t, func() error { return run(args) })
			if err != nil || !slices.Equal(paths, []string{"/health", "/api/withdraw"}) {
				t.Fatalf("discard: output=%q paths=%v err=%v", out, paths, err)
			}
			if variant == "default" {
				if out != "withdrew cart/empty@default (1 shot(s))\n" {
					t.Fatalf("discard text=%q", out)
				}
			} else {
				var result struct {
					SchemaVersion int
					Withdrawn     []string
				}
				if err := json.Unmarshal([]byte(out), &result); err != nil || result.SchemaVersion != 1 || !slices.Equal(result.Withdrawn, []string{snap.Digest}) {
					t.Fatalf("discard JSON=%s err=%v", out, err)
				}
			}
			values, err := store.Withdrawals(project.ID)
			if err != nil || len(values) != 1 || values[0].Thread != "shared" || values[0].Session != "successor" {
				t.Fatalf("discard lost session/thread: %v %v", values, err)
			}
		})
	}
}

func TestDiscardValidation(t *testing.T) {
	t.Setenv("VISION_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	for _, args := range [][]string{nil, {"bad-key"}, {"cart/empty", "--as", "bad/variant"}, {"cart/empty", "--dim", "vp=mobile"}, {"cart/empty", "extra"}} {
		if err := discard(args); err == nil {
			t.Errorf("discard %v: expected validation error", args)
		}
	}
	if err := discard([]string{"cart/empty"}); err == nil || !strings.Contains(err.Error(), "VISION_SESSION_ID") || !strings.Contains(err.Error(), "CLAUDE_CODE_SESSION_ID") {
		t.Fatalf("missing session error=%v", err)
	}
}

// Indented continuation lines keep region annotations inside their verdict's visual block.
func TestNotesIndentContinuationLines(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []store.Note{
		{Key: "checkout/cart", Variant: "default", Digest: "sha256:cccc", Verdict: "flag", Note: "Review this\n[1] question at 12,40: final copy?\n    Keep this line together"},
		{Key: "checkout/done", Variant: "default", Digest: "sha256:dddd", Verdict: "ok"},
	} {
		if err := store.AppendNote(project.ID, note); err != nil {
			t.Fatal(err)
		}
	}
	out, err := commandOutput(t, func() error { return notes(nil) })
	want := "flag checkout/cart@default: sha256:cccc Review this\n    [1] question at 12,40: final copy?\n        Keep this line together\nok checkout/done@default: sha256:dddd\n"
	if err != nil || out != want {
		t.Fatalf("notes output=%q err=%v, want %q", out, err, want)
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
	if _, err := store.JoinThread(project.ID, "mine", "a/b c"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSnap(project.ID, store.Snap{Digest: "thread", Session: "mine", Thread: "a/b c"}, nil); err != nil {
		t.Fatal(err)
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
			URL                 string
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
		if want := galleryURL + "/p/" + project.Name + "/t/a%2Fb%20c"; result.URL != want {
			t.Errorf("status url=%q, want %q", result.URL, want)
		}
	}
}

func TestStatusProjectAndGlobalURLs(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	project, err := store.Identify(".")
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"pending":0}`)), Header: make(http.Header)}, nil
	})
	for _, tc := range []struct {
		name, dir, want string
	}{
		{"project", "", galleryURL + "/p/" + project.Name},
		{"outside project", t.TempDir(), galleryURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dir != "" {
				t.Chdir(tc.dir)
			}
			out, err := commandOutput(t, func() error { return status([]string{"--json"}) })
			if err != nil {
				t.Fatal(err)
			}
			var result struct{ URL string }
			if err := json.Unmarshal([]byte(out), &result); err != nil || result.URL != tc.want {
				t.Fatalf("status JSON: %s err=%v, want url %q", out, err, tc.want)
			}
			out, err = commandOutput(t, func() error { return status(nil) })
			if err != nil || out != "vision on at "+tc.want+"\n" {
				t.Fatalf("status output=%q err=%v", out, err)
			}
		})
	}
}

func TestWaitHealthyEventuallyOK(t *testing.T) {
	var attempts atomic.Int32
	srv := healthTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected health path: %s", r.URL.Path)
		}
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	if err := waitHealthy(srv.URL+"/health", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("health attempts=%d, want 3", got)
	}
}

func TestWaitHealthyTimeout(t *testing.T) {
	srv := healthTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	timeout := 250 * time.Millisecond
	started := time.Now()
	err := waitHealthy(srv.URL+"/health", timeout)
	if err == nil || !strings.Contains(err.Error(), "within 250ms") {
		t.Fatalf("health timeout error=%v", err)
	}
	if elapsed := time.Since(started); elapsed < timeout || elapsed > time.Second {
		t.Fatalf("health timeout took %s, want at least %s and at most 1s", elapsed, timeout)
	}
}

// The sandbox forbids socket listeners, so net.Pipe keeps real HTTP requests and an httptest server usable on Go 1.26.
func healthTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener := &healthListener{connections: make(chan net.Conn), closed: make(chan struct{})}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	srv.Start()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		select {
		case listener.connections <- server:
			return client, nil
		case <-ctx.Done():
			client.Close()
			server.Close()
			return nil, ctx.Err()
		case <-listener.closed:
			client.Close()
			server.Close()
			return nil, net.ErrClosed
		}
	}}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		srv.Close()
		http.DefaultTransport = original
	})
	return srv
}

type healthListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func (l *healthListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *healthListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *healthListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4747}
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

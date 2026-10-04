package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The --server flag has to come before the subcommand, because it is a global flag: put it
// after and pinchtab rejects it, so a scoped snap fails instead of quietly capturing the
// wrong instance. Unset must add nothing at all, or a box with one agent breaks.
func TestPinchtabScopesToServer(t *testing.T) {
	t.Setenv("PINCHTAB_SERVER", "")
	if got := pinchtab("capture", "--json"); len(got) != 2 || got[0] != "capture" {
		t.Fatalf("unset server changed the args: %v", got)
	}
	t.Setenv("PINCHTAB_SERVER", "http://127.0.0.1:9999")
	got := pinchtab("capture", "--json")
	want := []string{"--server", "http://127.0.0.1:9999", "capture", "--json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The shape here is a verbatim trim of a real pinchtab capture response, so this test
// fails if pinchtab moves the fields or if someone reintroduces a guessed field name.
// A conditions block that silently parses to zeroes turns the conditions guard into a
// no-op that compares 0 against 0 and approves every mismatched diff.
func TestConditionsFromRealPinchtabResponse(t *testing.T) {
	var raw map[string]any
	body := `{"capturedAt":"2026-08-10T21:30:26.923059Z","status":"ok","title":"vision",
	  "url":"http://127.0.0.1:4747/","tabId":"77F6347DCF04763782F11997C3E80F4A",
	  "image":{"coordinateSpace":"viewport","devicePixelRatio":1,"format":"png",
	  "viewport":{"h":721,"scrollX":0,"scrollY":0,"w":1536}}}`
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	got := conditions(raw)
	if got.Width != 1536 || got.Height != 721 || got.DPR != 1 || got.URL != "http://127.0.0.1:4747/" {
		t.Fatalf("unexpected conditions: %#v", got)
	}
}

// Shape verified live: `pinchtab eval <expr> --json` answers {"result": true}. An
// unparseable answer must yield "" so the guard refuses the diff, never "light" by default,
// which would silently call a dark page light and let a scheme flip through.
func TestSchemeFrom(t *testing.T) {
	cases := map[string]string{
		`{"result": true}`:    "dark",
		`{"result": false}`:   "light",
		`{"result": null}`:    "",
		`{"error": "no tab"}`: "",
		`not json`:            "",
	}
	for body, want := range cases {
		if got := schemeFrom([]byte(body)); got != want {
			t.Errorf("schemeFrom(%s) = %q, want %q", body, got, want)
		}
	}
}

// The scheme eval must name the tab the capture came from. Riding pinchtab's current-tab
// pointer has been seen answering "tab <id> not found" for a tab that was already gone,
// which records scheme="" and forks the variant instead of diffing it. No tabId in the
// response means no --tab, because an empty one would be rejected outright.
func TestSchemeArgsAimAtTheCapturedTab(t *testing.T) {
	t.Setenv("PINCHTAB_SERVER", "")
	got := schemeArgs("77F6347DCF04763782F11997C3E80F4A")
	if len(got) < 2 || got[len(got)-2] != "--tab" || got[len(got)-1] != "77F6347DCF04763782F11997C3E80F4A" {
		t.Fatalf("scheme eval did not name the tab: %v", got)
	}
	for _, arg := range schemeArgs("") {
		if arg == "--tab" {
			t.Fatal("empty tab id still passed --tab")
		}
	}
}

func TestConditionsPermissiveShapes(t *testing.T) {
	raw := map[string]any{
		"viewport":    map[string]any{"width": float64(375), "height": float64(812)},
		"dpr":         float64(2),
		"page":        map[string]any{"url": "http://localhost:3000/checkout"},
		"colorScheme": "dark",
	}
	got := conditions(raw)
	if got.Width != 375 || got.Height != 812 || got.DPR != 2 || got.URL == "" || got.Scheme != "dark" {
		t.Fatalf("unexpected conditions: %#v", got)
	}
}

// fakePinchtab stands in for the CLI: it prints $VISION_TEST_CAPTURE, copies doc to the -o
// path the way pinchtab writes its image, and records its arguments in the returned file.
func fakePinchtab(t *testing.T, doc image.Image) string {
	t.Helper()
	dir := t.TempDir()
	docPath, argsPath := filepath.Join(dir, "doc.png"), filepath.Join(dir, "args")
	f, err := os.Create(docPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, doc); err != nil {
		t.Fatal(err)
	}
	f.Close()
	script := "#!/bin/sh\necho \"$@\" >> " + argsPath + "\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && cp " + docPath + " \"$2\"; shift; done\nprintf '%s\\n' \"$VISION_TEST_CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pinchtab"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsPath
}

// A document image whose every pixel encodes its own position, so a crop that lands on the
// wrong rows or columns cannot pass by accident.
func positional(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 7, A: 255})
		}
	}
	return img
}

func TestTakeRefusesBlankTab(t *testing.T) {
	fakePinchtab(t, positional(4, 3))
	for _, body := range []string{
		`{"url":""}`,
		`{"url":"about:blank"}`,
		`{"page":{"url":"about:blank"}}`,
		`{}`,
		`{"url":"http://localhost:3000/cart","colorScheme":"light","image":{"viewport":{"w":4,"h":3}}}`,
	} {
		t.Setenv("VISION_TEST_CAPTURE", body)
		shot, err := Take(context.Background())
		if strings.Contains(body, "localhost") {
			if err != nil || shot.Conditions.URL != "http://localhost:3000/cart" {
				t.Fatalf("valid page rejected: %#v %v", shot, err)
			}
			continue
		}
		for _, want := range []string{"current tab is blank", "capture nothing", "PINCHTAB_SERVER", "navigate first"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("Take(%s) = %v, want %q", body, err, want)
			}
		}
	}
}

func TestCaptureErrorWedgeGuidance(t *testing.T) {
	for _, tc := range []struct {
		err    error
		output string
		wedged bool
	}{
		{errors.New("signal: killed"), "", true},
		{context.DeadlineExceeded, "", true},
		{errors.New("exit status 1"), "capture: context deadline exceeded", true},
		{errors.New("exit status 1"), "signal: killed", true},
		{errors.New("executable not found"), "", false},
	} {
		err := captureError(tc.err, []byte(tc.output))
		if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.output) {
			t.Errorf("lost capture cause: %v", err)
		}
		for _, hint := range []string{"browser instance looks wedged, not the page", `pt release && eval "$(pt claim)"`, "navigate again"} {
			if strings.Contains(err.Error(), hint) != tc.wedged {
				t.Errorf("wedged=%v: unexpected hint %q in %v", tc.wedged, hint, err)
			}
		}
	}
	// Exercise the real shell-out error path too, without touching a browser instance.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pinchtab"), []byte("#!/bin/sh\necho 'context deadline exceeded' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := Take(context.Background()); err == nil || !strings.Contains(err.Error(), "browser instance looks wedged") {
		t.Fatalf("Take lost wedge guidance: %v", err)
	}
}

// Take must ask for a whole-document render and cut the scrolled viewport out of it. A
// plain capture in a browser window smaller than the emulated viewport came back with the
// overhang unpainted white (pinchtab picks the window at random), and those shots were
// approved as baselines before anyone noticed.
func TestTakeCutsTheViewportOutOfTheDocument(t *testing.T) {
	argsPath := fakePinchtab(t, positional(40, 100))
	t.Setenv("VISION_TEST_CAPTURE", `{"url":"http://localhost:3000/cart","colorScheme":"light",
	  "image":{"coordinateSpace":"document","devicePixelRatio":1,"viewport":{"w":30,"h":20,"scrollX":5,"scrollY":50}}}`)
	shot, err := Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "capture --json --beyond-viewport") {
		t.Fatalf("capture did not ask for a whole-document render: %s", args)
	}
	img, err := png.Decode(bytes.NewReader(shot.PNG))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 30 || b.Dy() != 20 {
		t.Fatalf("shot is %v, want the 30x20 viewport", b)
	}
	if r, g, _, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA(); r>>8 != 5 || g>>8 != 50 {
		t.Fatalf("shot starts at document (%d,%d), want the scroll offset (5,50)", r>>8, g>>8)
	}
}

func TestCropToViewport(t *testing.T) {
	encode := func(img image.Image) []byte {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	vp := func(w, h, x, y float64) map[string]any {
		return map[string]any{"w": w, "h": h, "scrollX": x, "scrollY": y}
	}

	// Device pixels: a 2x capture of a 10x8 viewport scrolled to (3,4) is the 20x16 block at (6,8).
	out, err := cropToViewport(encode(positional(40, 40)), vp(10, 8, 3, 4), 2)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	if b := img.Bounds(); b.Dx() != 20 || b.Dy() != 16 {
		t.Fatalf("2x crop is %v, want 20x16", b)
	}
	if r, g, _, _ := img.At(img.Bounds().Min.X, img.Bounds().Min.Y).RGBA(); r>>8 != 6 || g>>8 != 8 {
		t.Fatalf("2x crop starts at (%d,%d), want (6,8)", r>>8, g>>8)
	}

	// An image that already is the viewport passes through byte for byte.
	exact := encode(positional(10, 8))
	if out, err := cropToViewport(exact, vp(10, 8, 0, 0), 1); err != nil || !bytes.Equal(out, exact) {
		t.Fatalf("exact viewport was re-encoded or refused: %v", err)
	}

	// A capture smaller than the viewport it claims is the half-painted shot: refuse it.
	if _, err := cropToViewport(encode(positional(1280, 576)), vp(1440, 700, 0, 0), 1); err == nil || !strings.Contains(err.Error(), "does not show the whole viewport") {
		t.Fatalf("short capture was not refused: %v", err)
	}
	// No viewport in the response means nothing to cut to.
	if _, err := cropToViewport(exact, map[string]any{}, 1); err == nil || !strings.Contains(err.Error(), "no viewport size") {
		t.Fatalf("missing viewport was not refused: %v", err)
	}
}

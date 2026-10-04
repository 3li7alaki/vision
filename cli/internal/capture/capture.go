package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"vision/internal/store"
)

type Result struct {
	PNG        []byte
	Conditions store.Conditions
	Raw        map[string]any
}

// pinchtab prefixes an argument list with the server this process was told to talk to.
//
// PinchTab's unit of isolation is the browser instance, and the only way to name one is the
// global --server flag: `capture` has no --tab, so it always acts on its server's current
// tab. Without this, two agents working at once share one browser and one shot lands in the
// other's page, which looks like a real regression rather than a mixup. PinchTab has no
// environment variable of its own for this, so vision reads one and passes the flag.
//
// Unset means the default server, which is the right default for the only agent on a box.
func pinchtab(args ...string) []string {
	if server := os.Getenv("PINCHTAB_SERVER"); server != "" {
		return append([]string{"--server", server}, args...)
	}
	return args
}

func Take(ctx context.Context) (Result, error) {
	f, err := os.CreateTemp("", "vision-*.png")
	if err != nil {
		return Result{}, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	// --beyond-viewport makes Chrome render the page itself instead of copying the window's
	// compositor. PinchTab launches every browser at a random window size (1280x720 up to
	// 2560x1440, internal/bridge/runtime/init.go in pinchtab 0.15.2) and opens new tabs at
	// 1280x720, so an emulated 1440x700 viewport is wider than the real window and a plain
	// capture comes back with the overhang unpainted white. Puppeteer captures this way by
	// default (captureBeyondViewport) for the same reason. The image is the whole document,
	// so it is cropped back to the viewport below.
	cmd := exec.CommandContext(ctx, "pinchtab", pinchtab("capture", "--json", "--beyond-viewport", "-o", path, "--format", "png")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{}, captureError(err, out)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return Result{}, fmt.Errorf("decode pinchtab response: %w", err)
	}
	c := conditions(raw)
	// A fresh or accidentally shared instance can successfully capture its empty tab. That
	// is not evidence of the page the caller meant to show, so refuse it before it reaches
	// the queue. Redirecting a PNG into stdin cannot supply the browser state either.
	if c.URL == "" || c.URL == "about:blank" {
		return Result{}, fmt.Errorf("the current tab is blank, so the snap would capture nothing; point PINCHTAB_SERVER at your own instance and navigate first")
	}
	pngData, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	if pngData, err = cropToViewport(pngData, object(object(raw, "image"), "viewport"), c.DPR); err != nil {
		return Result{}, err
	}
	if c.Scheme == "" {
		c.Scheme = scheme(ctx, text(raw["tabId"]))
	}
	return Result{PNG: pngData, Conditions: c, Raw: raw}, nil
}

// cropToViewport cuts the viewport the caller was looking at out of a whole-document capture:
// {scrollX, scrollY, w, h} in CSS pixels, scaled by the device pixel ratio. A shot stays a
// viewport shot ("the top 700 px"), never a full page, so baselines keep their meaning.
//
// An image that does not cover that rectangle is refused rather than padded or scaled: it
// means the capture and the reported viewport disagree, and a picture of the wrong region
// queued as evidence is worse than no picture.
func cropToViewport(data []byte, viewport map[string]any, dpr float64) ([]byte, error) {
	if dpr <= 0 {
		dpr = 1
	}
	scaled := func(key string) int { return int(math.Round(number(viewport[key]) * dpr)) }
	origin := image.Pt(scaled("scrollX"), scaled("scrollY"))
	rect := image.Rectangle{Min: origin, Max: origin.Add(image.Pt(scaled("w"), scaled("h")))}
	if rect.Dx() <= 0 || rect.Dy() <= 0 {
		return nil, fmt.Errorf("pinchtab capture reported no viewport size, so the shot cannot be cut to what the page showed; this happens under load, snap again")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode capture: %w", err)
	}
	if !rect.In(img.Bounds()) {
		return nil, fmt.Errorf("capture is %dx%d but the viewport covers %v at device pixel ratio %g; the capture does not show the whole viewport, so it was not queued", img.Bounds().Dx(), img.Bounds().Dy(), rect, dpr)
	}
	if rect == img.Bounds() {
		return data, nil
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("decode capture: %T cannot be cropped", img)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, sub.SubImage(rect)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func captureError(err error, out []byte) error {
	err = fmt.Errorf("pinchtab capture: %w: %s", err, strings.TrimSpace(string(out)))
	// CommandContext kills the child on timeout, while PinchTab can report its own deadline
	// in output. Both point at an unresponsive instance; retrying or editing the page does
	// not recover it. Keep the original error so callers can still inspect the cause.
	if strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "context deadline exceeded") {
		return fmt.Errorf("the browser instance looks wedged, not the page; restart that instance (with pt on PATH: `pt release && eval \"$(pt claim)\"`), then navigate again: %w", err)
	}
	return err
}

// scheme reads the page's color scheme, which the capture response does not carry. This is
// the one shell-out beyond the capture itself, and it stays inside invariant 1 because it
// observes a media query: it does not navigate, click, seed, or log in.
//
// A failure here returns the empty string rather than failing the snap, because losing the
// picture is worse than losing one condition. That degrades safely: an empty scheme differs
// from a recorded one, so the guard refuses the diff instead of rendering a fake one.
//
// It aims at the tab the capture actually came from, because pinchtab's current-tab pointer
// goes stale: an eval with no --tab has been seen answering "tab <id> not found" for a tab
// that no longer exists, which turned a good capture into scheme=unknown and forked the
// variant into a new card instead of a diff.
func scheme(ctx context.Context, tabID string) string {
	cmd := exec.CommandContext(ctx, "pinchtab", schemeArgs(tabID)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return schemeFrom(out)
}

func schemeArgs(tabID string) []string {
	args := []string{"eval", "matchMedia('(prefers-color-scheme: dark)').matches", "--json"}
	if tabID != "" {
		args = append(args, "--tab", tabID)
	}
	return pinchtab(args...)
}

func schemeFrom(out []byte) string {
	var resp struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return ""
	}
	dark, ok := resp.Result.(bool)
	if !ok {
		return ""
	}
	if dark {
		return "dark"
	}
	return "light"
}

// Field names verified live against pinchtab 127.0.0.1:9867 on 2026-08-10: the real
// response nests the viewport under image as {"image":{"viewport":{"w":1536,"h":721},
// "devicePixelRatio":1},"url":"..."}. The looser aliases below are kept as fallbacks in
// case a later pinchtab flattens it.
//
// The capture response carries no color scheme at all, so Scheme is filled by a separate
// eval below rather than read from here.
func conditions(raw map[string]any) store.Conditions {
	image := object(raw, "image")
	viewport := object(image, "viewport")
	flat := object(raw, "viewport")
	page := object(raw, "page")
	return store.Conditions{
		Width:  integer(first(viewport["w"], viewport["width"], flat["w"], flat["width"], raw["width"], raw["viewportWidth"])),
		Height: integer(first(viewport["h"], viewport["height"], flat["h"], flat["height"], raw["height"], raw["viewportHeight"])),
		DPR:    number(first(image["devicePixelRatio"], flat["devicePixelRatio"], raw["devicePixelRatio"], raw["dpr"])),
		URL:    text(first(raw["url"], page["url"])),
		Scheme: text(first(raw["colorScheme"], raw["scheme"])),
	}
}

func object(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func first(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func integer(v any) int { return int(number(v)) }

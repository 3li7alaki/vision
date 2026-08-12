package capture

import (
	"context"
	"encoding/json"
	"fmt"
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
	cmd := exec.CommandContext(ctx, "pinchtab", pinchtab("capture", "--json", "-o", path, "--format", "png")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{}, fmt.Errorf("pinchtab capture: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return Result{}, fmt.Errorf("decode pinchtab response: %w", err)
	}
	pngData, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	c := conditions(raw)
	if c.Scheme == "" {
		c.Scheme = scheme(ctx, text(raw["tabId"]))
	}
	return Result{PNG: pngData, Conditions: c, Raw: raw}, nil
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

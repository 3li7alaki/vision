package capture

import (
	"encoding/json"
	"testing"
)

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

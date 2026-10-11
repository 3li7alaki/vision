package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"vision/internal/diff"
	"vision/internal/gallery"
	"vision/internal/store"
)

func TestGalleryRoutes(t *testing.T) {
	handler := New().Handler()
	for _, path := range []string{"/", "/p/x", "/p/x/b/feat/a", "/p/x/t/abc", "/p/odd%20project/t/a%2Fb%20c"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), gallery.HTML) {
			t.Errorf("GET %s: status=%d, want embedded gallery", path, rec.Code)
		}
	}
	for _, path := range []string{"/nope", "/p/x/nope", "/p/x/t/abc/extra"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status=%d, want 404", path, rec.Code)
		}
	}
}

func TestProjectIcon(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "vision")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>`
	if err := os.WriteFile(filepath.Join(dir, "shop.svg"), []byte(svg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "private.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := `{"projects":{"shop":{"logo":"shop.svg"},"unsupported":{"logo":"private.txt"}}}`
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	id, noIcon, unsupported := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for project, name := range map[string]string{id: "shop", noIcon: "no-icon-" + t.Name(), unsupported: "unsupported"} {
		if err := store.AppendSnap(project, store.Snap{Project: name, TS: time.Now(), Digest: project}, nil); err != nil {
			t.Fatal(err)
		}
	}
	handler := New().Handler()
	for _, tc := range []struct {
		id     string
		status int
	}{
		{id, http.StatusOK}, {"bad-id", http.StatusNotFound}, {noIcon, http.StatusNotFound},
		{strings.Repeat("d", 64), http.StatusNotFound}, {unsupported, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/media/icon/"+tc.id, nil))
		if rec.Code != tc.status {
			t.Errorf("icon %s: status=%d, want %d", tc.id, rec.Code, tc.status)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "max-age=300" || rec.Header().Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" {
			t.Errorf("icon headers: %v", rec.Header())
		}
		if tc.status == http.StatusOK && (rec.Header().Get("Content-Type") != "image/svg+xml" || rec.Body.String() != svg) {
			t.Errorf("svg response: type=%q body=%q", rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

func TestQueueItemThreadAndTimestamp(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i, snap := range []store.Snap{
		{Session: "successor", Thread: "predecessor"}, {Session: "legacy"}, {},
	} {
		snap.Project, snap.Key, snap.Variant, snap.TS = "shop", "cart/empty", "default", now
		snap.Digest = "sha256:" + strings.Repeat(string(rune('b'+i)), 64)
		if err := store.AppendSnap(id, snap, []byte(snap.Digest)); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/queue", nil))
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || rec.Code != http.StatusOK || len(items) != 3 {
		t.Fatalf("queue: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	for i, item := range items {
		if item["ts"] != now.Format(time.RFC3339) {
			t.Errorf("item %d ts=%v", i, item["ts"])
		}
		want := []string{"predecessor", "legacy", ""}[i]
		thread, present := item["thread"]
		if (want == "" && present) || (want != "" && thread != want) {
			t.Errorf("item %d thread=%v, want %q", i, thread, want)
		}
	}
}

func TestSnapDiffSurvivesHeightAndURLDrift(t *testing.T) {
	encode := func(height int, changed bool) []byte {
		im := image.NewRGBA(image.Rect(0, 0, 2, height))
		if changed {
			im.SetRGBA(0, 0, color.RGBA{R: 1, A: 255})
		}
		var b bytes.Buffer
		if err := png.Encode(&b, im); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	for _, tc := range []struct {
		name       string
		baseHeight int
		height     int
		url        string
		want       []string
		fraction   float64
	}{
		{"height grows", 1, 2, "http://x", []string{"size 2x1 to 2x2"}, .75},
		{"height shrinks", 2, 1, "http://x", []string{"size 2x2 to 2x1"}, .75},
		{"url drifts", 2, 2, "http://y", []string{"url http://x to http://y"}, .25},
		{"height and url drift", 1, 2, "http://y", []string{"size 2x1 to 2x2", "url http://x to http://y"}, .75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			basePNG := encode(tc.baseHeight, false)
			base := store.Snap{SchemaVersion: 1, TS: time.Now(), Project: "shop", Key: "checkout/cart", Variant: "default", Digest: diff.Digest(basePNG), Conditions: store.Conditions{Width: 2, Height: tc.baseHeight, DPR: 1, URL: "http://x", Scheme: "dark"}}
			if err := store.AppendSnap(id, base, basePNG); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendNote(id, store.Note{SchemaVersion: 1, TS: time.Now(), Key: base.Key, Variant: base.Variant, Digest: base.Digest, Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
			if err := store.CopyBaseline(id, base.Key, base.Variant, base.Digest); err != nil {
				t.Fatal(err)
			}
			conditions := base.Conditions
			conditions.Height, conditions.URL = tc.height, tc.url
			body, err := json.Marshal(SnapRequest{Project: store.Project{ID: id, Name: "shop"}, Key: base.Key, Variant: base.Variant, Capture: conditions, PNG: base64.StdEncoding.EncodeToString(encode(tc.height, true))})
			if err != nil {
				t.Fatal(err)
			}
			handler := New().Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snap", bytes.NewReader(body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("snap: status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Queued bool `json:"queued"`
				Item   Item `json:"item"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			items, err := Queue()
			if err != nil || len(items) != 1 {
				t.Fatalf("queue: got %d items err=%v, want 1", len(items), err)
			}
			if !response.Queued {
				t.Fatal("changed shot was not queued")
			}
			for _, item := range []Item{response.Item, items[0]} {
				if item.Status != "changed" || item.DiffURL == "" || item.Incomparable != "" || !slices.Equal(item.Changes, tc.want) || item.ChangedFraction != tc.fraction {
					t.Fatalf("comparison: got %+v, want changes %v and fraction %v", item, tc.want, tc.fraction)
				}
			}
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, response.Item.DiffURL, nil))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("diff media: status=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
			}
			im, err := png.Decode(rec.Body)
			if err != nil {
				t.Fatal(err)
			}
			if im.Bounds() != image.Rect(0, 0, 2, 2) {
				t.Fatalf("diff media bounds: got %v, want 2x2", im.Bounds())
			}
		})
	}
}

// A project id reaches store paths, so malformed ids must fail before any state is written.
func TestSnapRejectsInvalidProjectID(t *testing.T) {
	for _, id := range []string{"../x", "not-a-hash", "", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		t.Run(id, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("VISION_STATE_HOME", state)
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(SnapRequest{Project: store.Project{ID: id}, Key: "orders/list", Variant: "default", PNG: base64.StdEncoding.EncodeToString(pngData.Bytes()), Elements: []store.Element{{Role: "button", Name: "Order", W: 1, H: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snap", bytes.NewReader(body)))
			var response struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusBadRequest || response.Error != "invalid project id" {
				t.Errorf("snap: status=%d body=%s, want 400 invalid project id", rec.Code, rec.Body.String())
			}
			if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
				t.Fatalf("rejected snap wrote state: entries=%v err=%v", entries, err)
			}
		})
	}
}

// Shared pixels save disk, but each project, key and variant still needs its own human review.
func TestQueueKeepsIdenticalReviewsSeparate(t *testing.T) {
	for _, tc := range []struct {
		name, key, variant string
		otherProject       bool
	}{
		{"keys", "orders/theme", "default", false},
		{"variants", "orders/list", "dark", false},
		{"projects", "orders/list", "default", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id, otherID := strings.Repeat("a", 64), strings.Repeat("a", 64)
			if tc.otherProject {
				otherID = strings.Repeat("b", 64)
			}
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			first := store.Snap{SchemaVersion: 1, TS: time.Now(), Key: "orders/list", Variant: "default", Digest: diff.Digest(pngData.Bytes()), Session: "mine"}
			second := first
			second.Key, second.Variant = tc.key, tc.variant
			add := func(project string, snap store.Snap) {
				t.Helper()
				if err := store.AppendSnap(project, snap, pngData.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			check := func(want int) []Item {
				t.Helper()
				items, err := Queue()
				if err != nil || len(items) != want {
					t.Fatalf("queue: got %d items err=%v, want %d", len(items), err, want)
				}
				if n, err := PendingCount(); err != nil || n != want {
					t.Fatalf("total: pending=%d err=%v, want %d", n, err, want)
				}
				counts := map[string]int{id: want}
				if tc.otherProject {
					counts[id], counts[otherID] = want-1, 1
				}
				for project, count := range counts {
					if n, err := PendingCountFor(project); err != nil || n != count {
						t.Fatalf("project %s: pending=%d err=%v, want %d", project, n, err, count)
					}
					if n, err := PendingCountForSession(project, "mine"); err != nil || n != count {
						t.Fatalf("session in %s: pending=%d err=%v, want %d", project, n, err, count)
					}
				}
				return items
			}
			add(id, first)
			add(id, first)
			add(otherID, second)
			check(2)
			if err := store.AppendNote(id, store.Note{SchemaVersion: 1, TS: time.Now(), Key: first.Key, Variant: first.Variant, Digest: first.Digest, Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
			if err := store.CopyBaseline(id, first.Key, first.Variant, first.Digest); err != nil {
				t.Fatal(err)
			}
			for _, resnap := range []bool{false, true} {
				if resnap {
					add(id, first)
				}
				items := check(1)
				if items[0].ProjectID != otherID || items[0].Key != second.Key || items[0].Variant != second.Variant {
					t.Fatalf("remaining review: got %+v, want %s/%s@%s", items[0], otherID, second.Key, second.Variant)
				}
			}
		})
	}
}

// A later snap with shared pixels must not supply another review's baseline conditions.
func TestBaselineRecordMatchesReview(t *testing.T) {
	for _, tc := range []struct{ name, key, variant, media string }{
		{"keys", "orders/theme", "default", ""},
		{"variants", "orders/list", "dark", ""},
		{"media", "orders/list", "default", "video/mp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			base := store.Snap{Key: "orders/list", Variant: "default", Digest: "sha256:aaaa", Conditions: store.Conditions{Width: 1}}
			other := base
			other.Key, other.Variant, other.Conditions.Width = tc.key, tc.variant, 2
			other.Media = tc.media
			for _, snap := range []store.Snap{base, other} {
				if err := store.AppendSnap("p", snap, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.AppendNote("p", store.Note{Key: base.Key, Variant: base.Variant, Digest: base.Digest, Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
			if got, ok := baselineRecord("p", base.Key, base.Variant, ""); !ok || got.Key != base.Key || got.Variant != base.Variant || got.Conditions != base.Conditions {
				t.Fatalf("baseline record: got %+v ok=%v, want %+v", got, ok, base)
			}
		})
	}
}

// PendingCount is the cheap stand-in for Queue that a status line polls, so the number it
// reports has to be the number the human will actually see. A badge that says 3 over an
// empty queue is worse than no badge.
func TestPendingCountMatchesQueue(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	add := func(digest string) {
		snap := store.Snap{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: digest}
		if err := store.AppendSnap("p", snap, []byte(digest)); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		n, err := PendingCount()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(); got != 0 {
		t.Fatalf("empty state: got %d, want 0", got)
	}
	add("sha256:aaaa")
	add("sha256:bbbb")
	if got := count(); got != 1 {
		t.Fatalf("newest unjudged: got %d, want 1", got)
	}
	// A re-snap of identical content lands on the same digest, so it must not double count.
	add("sha256:aaaa")
	if got := count(); got != 1 {
		t.Fatalf("return to earlier digest: got %d, want 1", got)
	}
	if err := store.AppendNote("p", store.Note{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: "sha256:aaaa", Verdict: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 0 {
		t.Fatalf("after live review's verdict: got %d, want 0", got)
	}
}

// One record whose picture is gone, in any project, used to fail the whole queue with a
// file-not-found, so the gallery showed nothing for every repo. It is skipped instead, and
// the count agrees with what the queue shows.
func TestQueueSurvivesAMissingPicture(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	for i, id := range ids {
		digest := "sha256:" + strings.Repeat(string(rune('c'+i)), 8)
		snap := store.Snap{SchemaVersion: 1, TS: time.Now(), Project: "p", Key: "checkout/cart", Variant: "default", Digest: digest}
		if err := store.AppendSnap(id, snap, []byte(digest)); err != nil {
			t.Fatal(err)
		}
		// Give each a baseline, the path that reads the picture back.
		if err := os.MkdirAll(baseDir(t, id), 0o755); err != nil {
			t.Fatal(err)
		}
		base, _ := store.BaselinePath(id, "checkout/cart", "default")
		if err := os.WriteFile(base, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gone, _ := store.ShotPath(ids[0], "sha256:cccccccc")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	items, err := Queue()
	if err != nil {
		t.Fatalf("one missing picture failed the whole queue: %v", err)
	}
	if len(items) != 1 || items[0].ProjectID != ids[1] {
		t.Fatalf("want only the intact project's shot, got %d items", len(items))
	}
	n, err := PendingCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != len(items) {
		t.Fatalf("count %d disagrees with the queue's %d", n, len(items))
	}
}

func baseDir(t *testing.T, id string) string {
	t.Helper()
	p, err := store.BaselinePath(id, "checkout/cart", "default")
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(p)
}

// Before the fix the gallery posted the display name back as project, so the note landed
// under projects/<name>/ instead of projects/<sha256>/ and PendingCount never saw it. This
// locks that path closed: a verdict posted with the real id has to land where the queue
// reads from, and the count has to drop.
func TestVerdictLandsUnderHashedProjectID(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	snap := store.Snap{SchemaVersion: 1, TS: time.Now(), Project: "shop", Key: "checkout/cart", Variant: "default", Digest: "sha256:cccc"}
	if err := store.AppendSnap(id, snap, []byte("c")); err != nil {
		t.Fatal(err)
	}
	if n, err := PendingCount(); err != nil || n != 1 {
		t.Fatalf("before verdict: pending=%d err=%v, want 1", n, err)
	}
	body := strings.NewReader(`{"project":"` + id + `","key":"checkout/cart","variant":"default","digest":"sha256:cccc","verdict":"ok"}`)
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("verdict status: got %d, want %d, body=%s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if n, err := PendingCount(); err != nil || n != 0 {
		t.Fatalf("after verdict: pending=%d err=%v, want 0 (note must land under the hashed id)", n, err)
	}
}

// Small controls must win over their containers, and image pixels must map back to CSS pixels.
func TestResolveRegions(t *testing.T) {
	card := store.Element{Role: "group", Name: "Account", Depth: 1, W: 100, H: 100}
	button := store.Element{Role: "button", Name: "Withdraw", Depth: 2, X: 10, Y: 20, W: 20, H: 10}
	for _, tc := range []struct {
		name     string
		elements []store.Element
		region   store.Region
		dpr      float64
		want     string
	}{
		{"button over card", []store.Element{card, button}, store.Region{X: 11, Y: 21, W: 5, H: 5}, 1, `button "Withdraw"`},
		{"dpr two", []store.Element{button}, store.Region{X: 22, Y: 42, W: 10, H: 10}, 2, `button "Withdraw"`},
		{"fractional center", []store.Element{{Role: "button", Name: "Small", X: 1.2, Y: 1.2, W: .1, H: .1}}, store.Region{X: 2, Y: 2, W: 1, H: 1}, 2, `button "Small"`},
		{"empty space", []store.Element{card, button}, store.Region{X: 200, Y: 200}, 1, ""},
		{"left top included", []store.Element{button}, store.Region{X: 10, Y: 20}, 1, `button "Withdraw"`},
		{"right excluded", []store.Element{button}, store.Region{X: 30, Y: 25}, 1, ""},
		{"bottom excluded", []store.Element{button}, store.Region{X: 20, Y: 30}, 1, ""},
		{"deeper tie", []store.Element{{Role: "group", Name: "Shallow", Depth: 1, W: 100, H: 100}, {Role: "button", Name: "Deep", Depth: 3, W: 100, H: 100}}, store.Region{X: 10, Y: 10}, 1, `button "Deep"`},
		{"first equal tie", []store.Element{{Role: "button", Name: "First", Depth: 3, W: 100, H: 100}, {Role: "button", Name: "Second", Depth: 3, W: 100, H: 100}}, store.Region{X: 10, Y: 10}, 1, `button "First"`},
		{"long unicode name", []store.Element{{Role: "button", Name: strings.Repeat("界", 61), W: 100, H: 100}}, store.Region{X: 10, Y: 10}, 1, `button "` + strings.Repeat("界", 60) + `..."`},
		{"sixty runes unchanged", []store.Element{{Role: "button", Name: strings.Repeat("界", 60), W: 100, H: 100}}, store.Region{X: 10, Y: 10}, 1, `button "` + strings.Repeat("界", 60) + `"`},
		{"missing dpr", []store.Element{button}, store.Region{X: 11, Y: 21}, 0, `button "Withdraw"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.elements, tc.region, tc.dpr); got != tc.want {
				t.Fatalf("resolve = %q, want %q", got, tc.want)
			}
		})
	}
}

// Rendering keeps each human mark recognizable in plain text, including continuation lines.
func TestRenderRegions(t *testing.T) {
	regions := []store.Region{
		{Tone: "issue", Text: "too close to the edge", Element: `button "Withdraw"`},
		{Tone: "praise", Flow: true, X: 30, Y: 380, W: 270, H: 60, Text: "nice spacing"},
		{Tone: "question", X: 12, Y: 40, Text: "is this the final copy?"},
	}
	examples := "[1] issue on button \"Withdraw\": too close to the edge\n[2] praise (every step) box 30,380 270x60: nice spacing\n[3] question at 12,40: is this the final copy?"
	for _, tc := range []struct {
		name, free string
		regions    []store.Region
		want       string
	}{
		{"examples", "", regions, examples},
		{"free text first", "Review this layout", regions, "Review this layout\n" + examples},
		{"multiline mark", "", []store.Region{{Tone: "issue", Text: "first\n[2] still the first mark"}}, "[1] issue at 0,0: first\n    [2] still the first mark"},
		{"free text only", "Review this layout", nil, "Review this layout"},
		{"empty", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.free, tc.regions); got != tc.want {
				t.Fatalf("render = %q, want %q", got, tc.want)
			}
		})
	}
}

// The HTTP round trip must retain human marks and replace client names with captured elements.
func TestVerdictRegionsRoundTrip(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	var pngData bytes.Buffer
	if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	handler := New().Handler()
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		return rec
	}
	req := SnapRequest{Project: store.Project{ID: id, Name: "shop"}, Key: "checkout/cart", Variant: "default", PNG: base64.StdEncoding.EncodeToString(pngData.Bytes()), Capture: store.Conditions{DPR: 1}, Elements: []store.Element{
		{Role: "group", Name: "Account", W: 2, H: 2},
		{Role: "button", Name: "Withdraw", Depth: 2, X: 1, Y: 1, W: 1, H: 1},
	}}
	post("/api/snap", req)
	req.Capture.DPR = 2
	post("/api/snap", req)
	digest := diff.Digest(pngData.Bytes())
	rec := post("/api/verdict", VerdictRequest{Project: id, Key: req.Key, Variant: req.Variant, Digest: digest, Verdict: "flag", Regions: []store.Region{
		{X: 2, Y: 2, W: 1, H: 1, Tone: "issue", Text: "  too close to the edge  ", Element: "client lie"},
		{X: 2, Y: 2, Tone: "praise", Text: "nice spacing", Flow: true},
	}})
	notes, err := store.Notes(id)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes=%v err=%v, want one verdict", notes, err)
	}
	wantRegions := []store.Region{
		{X: 2, Y: 2, W: 1, H: 1, Tone: "issue", Text: "too close to the edge", Element: `button "Withdraw"`},
		{X: 2, Y: 2, Tone: "praise", Text: "nice spacing", Flow: true, Element: `button "Withdraw"`},
	}
	wantText := "[1] issue on button \"Withdraw\": too close to the edge\n[2] praise (every step) on button \"Withdraw\": nice spacing"
	var response store.Note
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, note := range []store.Note{notes[0], response} {
		if note.Note != wantText || !slices.Equal(note.Regions, wantRegions) {
			t.Fatalf("note=%+v, want text %q and regions %+v", note, wantText, wantRegions)
		}
	}
}

// Invalid input must be rejected before this unauthenticated endpoint writes a verdict.
func TestVerdictRegionValidation(t *testing.T) {
	valid := store.Region{Tone: "issue", Text: "edge"}
	many := make([]store.Region, 51)
	for i := range many {
		many[i] = valid
	}
	for _, tc := range []struct {
		name, digest, note string
		regions            []store.Region
		status             int
	}{
		{"bad tone", "sha256:cccc", "", []store.Region{{Tone: "bad", Text: "edge"}}, http.StatusBadRequest},
		{"empty text", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: " \n\t "}}, http.StatusBadRequest},
		{"too many regions", "sha256:cccc", "", many, http.StatusBadRequest},
		{"digest traversal", "sha256:../x", "edge", nil, http.StatusBadRequest},
		{"empty digest", "", "edge", nil, http.StatusBadRequest},
		{"nonhex digest", "sha256:gggg", "edge", nil, http.StatusBadRequest},
		{"empty flag", "sha256:cccc", " \n\t ", nil, http.StatusBadRequest},
		{"negative x", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: "edge", X: -1}}, http.StatusBadRequest},
		{"negative y", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: "edge", Y: -1}}, http.StatusBadRequest},
		{"negative width", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: "edge", W: -1}}, http.StatusBadRequest},
		{"negative height", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: "edge", H: -1}}, http.StatusBadRequest},
		{"text exceeds rune cap", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: strings.Repeat("界", 2001)}}, http.StatusBadRequest},
		{"rune cap accepts unicode", "sha256:cccc", "", []store.Region{{Tone: "issue", Text: " " + strings.Repeat("界", 2000) + " "}}, http.StatusCreated},
		{"fifty regions accepted", "sha256:cccc", "", many[:50], http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			data, err := json.Marshal(VerdictRequest{Project: id, Key: "checkout/cart", Variant: "default", Digest: tc.digest, Verdict: "flag", Note: tc.note, Regions: tc.regions})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", bytes.NewReader(data)))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if tc.status == http.StatusBadRequest {
				notes, err := store.Notes(id)
				if err != nil || len(notes) != 0 {
					t.Fatalf("rejected request wrote notes=%v err=%v", notes, err)
				}
			}
		})
	}
}

// Snapshot failures must drop optional element names without losing the human's verdict.
func TestVerdictRegionsSnapshotFallback(t *testing.T) {
	for _, mode := range []string{"missing elements", "missing snap", "unreadable elements", "unreadable index", "zero dpr"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id, digest := strings.Repeat("a", 64), "sha256:cccc"
			if mode != "missing elements" {
				if err := store.WriteElements(id, digest, []store.Element{{Role: "button", Name: "Withdraw", W: 10, H: 10}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "missing snap" {
				if err := store.AppendSnap(id, store.Snap{Digest: digest}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unreadable elements" || mode == "unreadable index" {
				path, err := store.ElementsPath(id, digest)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unreadable index" {
					dir, err := store.ProjectDir(id)
					if err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(dir, "index.jsonl")
				}
				if err := os.WriteFile(path, []byte("bad json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(VerdictRequest{Project: id, Key: "checkout/cart", Variant: "default", Digest: digest, Verdict: "flag", Note: "  Review this  ", Regions: []store.Region{{X: 1, Y: 1, Tone: "question", Text: "  final?  ", Element: "client lie"}}})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", bytes.NewReader(data)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s, want 201", rec.Code, rec.Body.String())
			}
			notes, err := store.Notes(id)
			if err != nil || len(notes) != 1 || len(notes[0].Regions) != 1 {
				t.Fatalf("notes=%v err=%v, want one mark", notes, err)
			}
			wantElement, wantText := "", "Review this\n[1] question at 1,1: final?"
			if mode == "zero dpr" {
				wantElement, wantText = `button "Withdraw"`, "Review this\n[1] question on button \"Withdraw\": final?"
			}
			if notes[0].Regions[0].Element != wantElement || notes[0].Regions[0].Text != "final?" || notes[0].Note != wantText {
				t.Fatalf("note=%+v, want element %q and text %q", notes[0], wantElement, wantText)
			}
		})
	}
}

// The status line asks "what is waiting for me here", so a shot queued in another repo
// must not appear in this one's count. Only the daemon-wide total sees both.
func TestPendingCountForScopesToOneProject(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	mine, theirs := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for id, digest := range map[string]string{mine: "sha256:1111", theirs: "sha256:2222"} {
		snap := store.Snap{SchemaVersion: 1, TS: time.Now(), Project: "shop", Key: "checkout/cart", Variant: "default", Digest: digest}
		if err := store.AppendSnap(id, snap, []byte(digest)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := PendingCountFor(mine); err != nil || n != 1 {
		t.Fatalf("this project: pending=%d err=%v, want 1 (another repo's review must not count here)", n, err)
	}
	if n, err := PendingCount(); err != nil || n != 2 {
		t.Fatalf("daemon-wide: pending=%d err=%v, want 2", n, err)
	}
}

func TestPendingCountForSession(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for _, snap := range []store.Snap{
		{Session: "other", Digest: "shared"},
		{Session: "mine", Digest: "shared"},
		{Session: "mine", Digest: "shared"},
		{Session: "mine", Digest: "mine"},
		{Session: "other", Digest: "theirs"},
		{Session: "mine", Digest: "ok"},
		{Session: "mine", Digest: "flag"},
		{Session: "", Digest: "anonymous"},
	} {
		// Give independent reviews separate keys so this fixture tests session deduplication, not supersede.
		snap.Key, snap.Variant = "session/"+snap.Digest, "default"
		if err := store.AppendSnap("p", snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, verdict := range []string{"ok", "flag"} {
		if err := store.AppendNote("p", store.Note{Key: "session/" + verdict, Variant: "default", Digest: verdict, Verdict: verdict}); err != nil {
			t.Fatal(err)
		}
	}
	for session, want := range map[string]int{"mine": 2, "other": 2, "missing": 0, "": 4} {
		if n, err := PendingCountForSession("p", session); err != nil || n != want {
			t.Errorf("session %q: pending=%d err=%v, want %d", session, n, err, want)
		}
	}
	if n, err := PendingCountFor("p"); err != nil || n != 4 {
		t.Errorf("legacy project count: pending=%d err=%v, want 4", n, err)
	}
}

// req.Project reaches filepath.Join via store.ProjectDir, so a value like ../../etc is a
// write outside the store. The 64-hex guard rejects it before any path use.
func TestVerdictRejectsInvalidProjectID(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for _, bad := range []string{"not-a-hash", "../../etc"} {
		t.Run(bad, func(t *testing.T) {
			body := strings.NewReader(`{"project":"` + bad + `","key":"checkout/cart","variant":"default","digest":"sha256:cccc","verdict":"ok"}`)
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status got %d, want %d", rec.Code, http.StatusBadRequest)
			}
			dir, err := store.ProjectDir(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Fatalf("store dir created for invalid project id: %s", dir)
			}
		})
	}
}

// The queue is where the round trip actually broke: it handed the gallery a display name,
// the gallery posted that back, and the note landed under projects/<name>/. Whatever the
// item carries for the post has to be the id the store is keyed by, never the name.
func TestQueueItemCarriesHashedProjectID(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("b", 64)
	snap := store.Snap{SchemaVersion: 1, TS: time.Now(), Project: "shop", Key: "checkout/cart", Variant: "default", Digest: "sha256:dddd"}
	if err := store.AppendSnap(id, snap, []byte("d")); err != nil {
		t.Fatal(err)
	}
	items, err := Queue()
	if err != nil || len(items) != 1 {
		t.Fatalf("queue: got %d items err=%v, want 1", len(items), err)
	}
	if items[0].ProjectID != id {
		t.Fatalf("ProjectID: got %q, want the hashed id %q", items[0].ProjectID, id)
	}
	if items[0].Project != "shop" {
		t.Fatalf("Project: got %q, want the display name shop", items[0].Project)
	}
}

func TestQueueClosesSupersededAndWithdrawn(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	old := store.Snap{Key: "cart/empty", Variant: "default", Digest: "sha256:aaaa", Session: "mine"}
	live, other := old, old
	live.Digest, other.Session = "sha256:bbbb", "other"
	for _, snap := range []store.Snap{old, live, live, other} {
		if err := store.AppendSnap(id, snap, []byte(snap.Digest)); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want, mine, theirs int) {
		t.Helper()
		items, err := Queue()
		if err != nil || len(items) != want {
			t.Fatalf("queue=%v err=%v, want %d", items, err, want)
		}
		for session, count := range map[string]int{"": want, "mine": mine, "other": theirs} {
			if n, err := PendingCountForSession(id, session); err != nil || n != count {
				t.Fatalf("session %q count=%d err=%v, want %d", session, n, err, count)
			}
		}
	}
	check(2, 1, 1)
	for i, snap := range []store.Snap{live, other} {
		if err := store.AppendWithdrawal(id, store.Withdrawal{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Session: snap.Session, Thread: snap.ThreadID()}); err != nil {
			t.Fatal(err)
		}
		check(1-i, 0, 1-i)
	}
}

func TestWithdrawPending(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	old := store.Snap{Project: "shop", Key: "cart/empty", Variant: "default", Digest: "sha256:aaaa", Session: "predecessor", Thread: "shared"}
	live, other := old, old
	live.Digest, other.Digest, other.Thread = "sha256:bbbb", "sha256:cccc", "other"
	for _, snap := range []store.Snap{old, live, live, other} {
		if err := store.AppendSnap(id, snap, []byte(snap.Digest)); err != nil {
			t.Fatal(err)
		}
	}
	s := New()
	events := make(chan Item, 2)
	s.clients[events] = struct{}{}
	body, err := json.Marshal(WithdrawRequest{Project: id, Key: old.Key, Variant: old.Variant, Session: "successor", Thread: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/withdraw", bytes.NewReader(body)))
		return rec
	}
	rec := request()
	var response struct {
		SchemaVersion int
		Withdrawn     []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || response.SchemaVersion != 1 || !slices.Equal(response.Withdrawn, []string{live.Digest}) {
		t.Fatalf("withdraw response: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	withdrawals, err := store.Withdrawals(id)
	if err != nil || len(withdrawals) != 1 || withdrawals[0].Digest != live.Digest || withdrawals[0].Session != "successor" || withdrawals[0].Thread != "shared" || withdrawals[0].SchemaVersion != 1 || withdrawals[0].TS.IsZero() {
		t.Fatalf("withdrawals=%v err=%v", withdrawals, err)
	}
	dir, _ := store.ProjectDir(id)
	data, err := os.ReadFile(filepath.Join(dir, "withdrawn.jsonl"))
	if err != nil || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("ledger=%q err=%v, want one line", data, err)
	}
	items, err := Queue()
	if err != nil || len(items) != 1 || items[0].Thread != "other" || items[0].Digest != other.Digest {
		t.Fatalf("queue after withdrawal=%v err=%v", items, err)
	}
	if n, err := PendingCountFor(id); err != nil || n != 1 {
		t.Fatalf("count after withdrawal=%d err=%v", n, err)
	}
	select {
	case event := <-events:
		if event.ProjectID != id || event.Project != "shop" || event.Thread != "shared" || event.Key != old.Key || event.Variant != old.Variant || event.Digest != live.Digest || event.Status != "withdrawn" {
			t.Fatalf("withdraw event=%+v", event)
		}
	default:
		t.Fatal("withdrawal did not publish a live event")
	}
	if len(events) != 0 {
		t.Fatal("duplicate snap published duplicate withdrawal")
	}
	if notes, err := store.Notes(id); err != nil || len(notes) != 0 {
		t.Fatalf("withdrawal changed human verdicts: %v %v", notes, err)
	}
	baseline, _ := store.BaselinePath(id, old.Key, old.Variant)
	if _, err := os.Stat(baseline); !os.IsNotExist(err) {
		t.Fatalf("withdrawal created a baseline: %v", err)
	}
	rec = request()
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "nothing pending for cart/empty@default on this thread") {
		t.Fatalf("repeat withdrawal: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWithdrawRefusesClosedOrOtherThread(t *testing.T) {
	for _, mode := range []string{"flag", "ok", "other thread", "missing"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			snap := store.Snap{Key: "cart/empty", Variant: "default", Digest: "sha256:aaaa", Session: "mine"}
			if mode == "other thread" {
				snap.Thread = "other"
			}
			if mode != "missing" {
				if err := store.AppendSnap(id, snap, []byte("shot")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "flag" || mode == "ok" {
				if err := store.AppendNote(id, store.Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: mode, Note: "reviewed"}); err != nil {
					t.Fatal(err)
				}
			}
			body, _ := json.Marshal(WithdrawRequest{Project: id, Key: snap.Key, Variant: snap.Variant, Session: "mine", Thread: "mine"})
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/withdraw", bytes.NewReader(body)))
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "nothing pending for cart/empty@default on this thread") {
				t.Fatalf("refused withdrawal: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if values, err := store.Withdrawals(id); err != nil || len(values) != 0 {
				t.Fatalf("refused withdrawal wrote ledger: %v %v", values, err)
			}
		})
	}
}

func TestWithdrawValidation(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, body := range []string{
		`{"project":"../../etc","key":"cart/empty","variant":"default","thread":"mine"}`,
		`{"project":"` + id + `","key":"invalid","variant":"default","thread":"mine"}`,
		`{"project":"` + id + `","key":"cart/empty","variant":"bad/variant","thread":"mine"}`,
		`{"project":"` + id + `","key":"cart/empty","variant":"default","thread":""}`,
		`{"project":"` + id + `","key":"cart/empty","variant":"default","thread":"mine","unknown":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("VISION_STATE_HOME", state)
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/withdraw", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
				t.Fatalf("invalid request wrote state: %v %v", entries, err)
			}
		})
	}
}

func TestSnapDoesNotRequeueClosedReview(t *testing.T) {
	for _, mode := range []string{"flag", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			data := pngData.Bytes()
			snap := store.Snap{Key: "cart/empty", Variant: "default", Digest: diff.Digest(data), Session: "mine"}
			if err := store.AppendSnap(id, snap, data); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "flag" {
				err = store.AppendNote(id, store.Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "flag", Note: "fix"})
			} else {
				err = store.AppendWithdrawal(id, store.Withdrawal{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Session: "mine", Thread: "mine"})
			}
			if err != nil {
				t.Fatal(err)
			}
			s := New()
			events := make(chan Item, 1)
			s.clients[events] = struct{}{}
			body, _ := json.Marshal(SnapRequest{Project: store.Project{ID: id}, Key: snap.Key, Variant: snap.Variant, Session: "mine", PNG: base64.StdEncoding.EncodeToString(data)})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snap", bytes.NewReader(body)))
			var response struct{ Queued bool }
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || response.Queued || len(events) != 0 {
				t.Fatalf("closed re-snap: status=%d body=%s events=%d err=%v", rec.Code, rec.Body.String(), len(events), err)
			}
		})
	}
}

func TestQueueGroupsVariantsBehindNewerKey(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("c", 64)
	now := time.Now()
	snaps := []store.Snap{
		{SchemaVersion: 1, TS: now.Add(-2 * time.Minute), Project: "shop", Key: "checkout/cart", Variant: "vp=mobile", Dims: map[string]string{"vp": "mobile"}, Meta: map[string]string{"ticket": "V-1"}, Digest: "sha256:1111"},
		{SchemaVersion: 1, TS: now.Add(-time.Minute), Project: "shop", Key: "checkout/cart", Variant: "vp=desktop", Dims: map[string]string{"vp": "desktop"}, Digest: "sha256:2222"},
		{SchemaVersion: 1, TS: now, Project: "shop", Key: "checkout/empty", Variant: "default", Digest: "sha256:3333"},
	}
	for _, snap := range snaps {
		if err := store.AppendSnap(id, snap, []byte(snap.Digest)); err != nil {
			t.Fatal(err)
		}
	}
	items, err := Queue()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("queue length: got %d, want 3", len(items))
	}
	want := []string{"checkout/empty@default", "checkout/cart@vp=desktop", "checkout/cart@vp=mobile"}
	for i, item := range items {
		if got := item.Key + "@" + item.Variant; got != want[i] {
			t.Errorf("item %d: got %q, want %q", i, got, want[i])
		}
		if item.Group != id+"/"+item.Key {
			t.Errorf("item %d group: got %q", i, item.Group)
		}
	}
	if items[1].Dims["vp"] != "desktop" || items[2].Meta["ticket"] != "V-1" {
		t.Fatalf("dimensions or metadata did not survive queue: %#v %#v", items[1].Dims, items[2].Meta)
	}
}

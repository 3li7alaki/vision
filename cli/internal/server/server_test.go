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
	if got := count(); got != 2 {
		t.Fatalf("two unjudged: got %d, want 2", got)
	}
	// A re-snap of identical content lands on the same digest, so it must not double count.
	add("sha256:aaaa")
	if got := count(); got != 2 {
		t.Fatalf("duplicate digest: got %d, want 2", got)
	}
	if err := store.AppendNote("p", store.Note{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: "sha256:aaaa", Verdict: "ok"}); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("after one verdict: got %d, want 1", got)
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
		if err := store.AppendSnap("p", snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, verdict := range []string{"ok", "flag"} {
		if err := store.AppendNote("p", store.Note{Digest: verdict, Verdict: verdict}); err != nil {
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

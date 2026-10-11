package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"vision/internal/diff"
	"vision/internal/store"
)

func fakeVideo(media string) []byte {
	if media == "video/webm" {
		return []byte("\x1a\x45\xdf\xa3\x00\x00\x00\x00")
	}
	return []byte("\x00\x00\x00\x18ftypisom\x00\x00\x00\x00")
}

// The full video review cycle must serve seekable evidence and compare content without ever pixel-diffing it.
func TestVideoSnapLifecycle(t *testing.T) {
	for _, media := range []string{"video/mp4", "video/webm"} {
		t.Run(media, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			data := fakeVideo(media)
			req := SnapRequest{Project: store.Project{ID: id, Name: "shop"}, Key: "flow/record#1", Variant: "default", Session: "mine", Media: media, PNG: base64.StdEncoding.EncodeToString(data)}
			// An existing PNG baseline cannot become a video's baseline just because the key matches.
			image := store.Snap{Key: req.Key, Variant: req.Variant, Digest: "sha256:aaaa"}
			if err := store.AppendSnap(id, image, []byte("older image")); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendNote(id, store.Note{Key: image.Key, Variant: image.Variant, Digest: image.Digest, Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
			if err := store.CopyBaseline(id, image.Key, image.Variant, image.Digest); err != nil {
				t.Fatal(err)
			}
			handler := New().Handler()
			post := func(path string, value any) *httptest.ResponseRecorder {
				t.Helper()
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				if rec.Code != http.StatusCreated {
					t.Fatalf("POST %s: status=%d body=%s", path, rec.Code, rec.Body.String())
				}
				return rec
			}
			response := func(rec *httptest.ResponseRecorder) (Item, bool) {
				t.Helper()
				var body struct {
					Queued bool `json:"queued"`
					Item   Item `json:"item"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body.Item, body.Queued
			}
			first, queued := response(post("/api/snap", req))
			if !queued || first.Status != "new" || first.Media != media || first.BaselineURL != "" || !strings.Contains(first.ShotURL, "media="+strings.ReplaceAll(media, "/", "%2F")) {
				t.Fatalf("new video: %+v queued=%v", first, queued)
			}
			items, err := Queue()
			if err != nil || len(items) != 1 || items[0].Media != media || len(items[0].Steps) != 1 || items[0].Steps[0].Media != media || items[0].Steps[0].ThumbURL != first.ShotURL {
				t.Fatalf("video queue=%+v err=%v", items, err)
			}
			if count, err := PendingCountForSession(id, "mine"); err != nil || count != 1 {
				t.Fatalf("video count=%d err=%v", count, err)
			}
			get := httptest.NewRequest(http.MethodGet, first.ShotURL, nil)
			get.Header.Set("Range", "bytes=4-7")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, get)
			if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Type") != media || rec.Header().Get("Content-Range") != fmt.Sprintf("bytes 4-7/%d", len(data)) || !bytes.Equal(rec.Body.Bytes(), data[4:8]) {
				t.Fatalf("ranged video: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.Bytes())
			}
			post("/api/verdict", VerdictRequest{Project: id, Key: req.Key, Variant: req.Variant, Digest: first.Digest, Verdict: "ok"})
			baseline, _ := store.BaselinePathMedia(id, req.Key, req.Variant, media)
			if got, err := os.ReadFile(baseline); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("video baseline=%q err=%v", got, err)
			}
			_, queued = response(post("/api/snap", req))
			if queued {
				t.Fatal("identical approved video was requeued")
			}
			if items, err := Queue(); err != nil || len(items) != 0 {
				t.Fatalf("unchanged video queue=%+v err=%v", items, err)
			}
			if count, err := PendingCountForSession(id, "mine"); err != nil || count != 0 {
				t.Fatalf("unchanged video count=%d err=%v", count, err)
			}
			// Different conditions would reject an image diff, but a video has no comparable viewport.
			req.Capture = store.Conditions{Width: 390, Height: 844, DPR: 2, Scheme: "dark"}
			req.PNG = base64.StdEncoding.EncodeToString(append(data, 1))
			rec = post("/api/snap", req)
			changed, queued := response(rec)
			if !queued || changed.Status != "changed" || changed.Media != media || changed.BaselineURL == "" || !strings.Contains(changed.BaselineURL, "media=") || changed.DiffURL != "" || changed.Incomparable != "" || changed.ChangedFraction != 0 || len(changed.Changes) != 0 {
				t.Fatalf("changed video: %+v queued=%v", changed, queued)
			}
			var fields struct{ Item map[string]json.RawMessage }
			if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"diffUrl", "incomparable", "changedFraction", "changes"} {
				if _, present := fields.Item[name]; present {
					t.Errorf("video comparison emitted %s", name)
				}
			}
			rec = httptest.NewRecorder()
			get = httptest.NewRequest(http.MethodGet, changed.BaselineURL, nil)
			get.Header.Set("Range", "bytes=0-3")
			handler.ServeHTTP(rec, get)
			if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Type") != media || !bytes.Equal(rec.Body.Bytes(), data[:4]) {
				t.Fatalf("ranged baseline: status=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.Bytes())
			}
			rec = httptest.NewRecorder()
			diffURL := strings.Replace(changed.BaselineURL, "/media/baseline/", "/media/diff/", 1)
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, diffURL, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("video diff status=%d, want 404", rec.Code)
			}
		})
	}
}

// A wrong MIME label must fail before mislabeled bytes enter either the queue or permanent ledger.
func TestSnapMediaValidation(t *testing.T) {
	for _, tc := range []struct {
		name, media string
		data        []byte
		want        int
		error       string
	}{
		{"legacy PNG", "", []byte("\x89PNG\r\n\x1a\n"), http.StatusCreated, ""},
		{"explicit PNG", "image/png", []byte("\x89PNG\r\n\x1a\n"), http.StatusCreated, ""},
		{"PNG mismatch", "", fakeVideo("video/mp4"), http.StatusBadRequest, "file is not image/png"},
		{"MP4 mismatch", "video/mp4", fakeVideo("video/webm"), http.StatusBadRequest, "file is not video/mp4"},
		{"WebM mismatch", "video/webm", fakeVideo("video/mp4"), http.StatusBadRequest, "file is not video/webm"},
		{"truncated MP4", "video/mp4", []byte("ftyp"), http.StatusBadRequest, "file is not video/mp4"},
		{"unknown media", "video/avi", fakeVideo("video/mp4"), http.StatusBadRequest, `unknown media "video/avi"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("VISION_STATE_HOME", state)
			body, err := json.Marshal(SnapRequest{Project: store.Project{ID: strings.Repeat("a", 64)}, Key: "flow/record", Variant: "default", Media: tc.media, PNG: base64.StdEncoding.EncodeToString(tc.data)})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snap", bytes.NewReader(body)))
			var result struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != tc.want || result.Error != tc.error {
				t.Fatalf("status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
			}
			if tc.want == http.StatusBadRequest {
				if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
					t.Fatalf("rejected media wrote state: %v %v", entries, err)
				}
			}
		})
	}
	for _, path := range []string{"/media/shot/p/abcd?media=video%2Favi", "/media/baseline/p/abcd?media=video%2Favi"} {
		rec := httptest.NewRecorder()
		New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unknown media GET: status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

// The request cap must report overload even for chunked bodies or excess whitespace after valid JSON.
func TestRequestBodyCap(t *testing.T) {
	const requestCap = 20 << 20
	padding := strings.Repeat(" ", requestCap)
	for _, route := range []string{"/api/snap", "/api/verdict", "/api/withdraw"} {
		for _, mode := range []string{"known length", "unknown length", "trailing whitespace"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				state := t.TempDir()
				t.Setenv("VISION_STATE_HOME", state)
				prefix, suffix := `{"note":"`, `"}`
				if mode == "trailing whitespace" {
					prefix, suffix = `{}`, ""
				}
				body := io.MultiReader(strings.NewReader(prefix), strings.NewReader(padding), strings.NewReader(suffix))
				req := httptest.NewRequest(http.MethodPost, route, body)
				if mode == "known length" {
					req.ContentLength = int64(len(prefix) + len(padding) + len(suffix))
				}
				rec := httptest.NewRecorder()
				New().Handler().ServeHTTP(rec, req)
				var result struct{ Error string }
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusRequestEntityTooLarge || result.Error != "file over the 20 MB request cap; record shorter or with --scale 0.5" {
					t.Fatalf("cap: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
				}
				if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
					t.Fatalf("oversized request wrote state: %v %v", entries, err)
				}
			})
		}
	}
}

// Video marks must preserve pause times and must not inherit element names from a stale snapshot.
func TestVerdictVideoRegions(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id, data := strings.Repeat("a", 64), fakeVideo("video/mp4")
	snap := store.Snap{Key: "flow/record", Variant: "default", Digest: diff.Digest(data), Media: "video/mp4", Conditions: store.Conditions{DPR: 1}}
	if err := store.AppendSnap(id, snap, data); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteElements(id, snap.Digest, []store.Element{{Role: "button", Name: "Stale", W: 500, H: 500}}); err != nil {
		t.Fatal(err)
	}
	seconds := 3.2
	body, err := json.Marshal(VerdictRequest{Project: id, Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "flag", Regions: []store.Region{{X: 30, Y: 40, W: 200, H: 60, Tone: "issue", Text: "text", T: &seconds, Element: "client lie"}}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", bytes.NewReader(body)))
	notes, err := store.Notes(id)
	if rec.Code != http.StatusCreated || err != nil || len(notes) != 1 {
		t.Fatalf("video mark: status=%d body=%s notes=%v err=%v", rec.Code, rec.Body.String(), notes, err)
	}
	if len(notes[0].Regions) != 1 || notes[0].Regions[0].Element != "" || notes[0].Regions[0].T == nil || *notes[0].Regions[0].T != seconds || notes[0].Note != "[1] issue 0:03.2 box 30,40 200x60: text" {
		t.Fatalf("video note=%+v", notes[0])
	}
}

// Plain-text video feedback needs tenths and minute carry so humans can find the paused frame.
func TestRenderVideoRegions(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		box     bool
		want    string
	}{
		{3.2, true, "[1] issue 0:03.2 box 30,40 200x60: text"},
		{3.2, false, "[1] issue 0:03.2 at 30,40: text"},
		{65, false, "[1] issue 1:05.0 at 30,40: text"},
		{59.96, false, "[1] issue 1:00.0 at 30,40: text"},
		{0, false, "[1] issue 0:00.0 at 30,40: text"},
	} {
		region := store.Region{X: 30, Y: 40, Tone: "issue", Text: "text", T: &tc.seconds, Element: "must not resolve video elements"}
		if tc.box {
			region.W, region.H = 200, 60
		}
		if got := render("", []store.Region{region}); got != tc.want {
			t.Errorf("render=%q, want %q", got, tc.want)
		}
	}
}

// Invalid pause times must never reach the verdict ledger, while zero remains a real timestamp.
func TestVerdictVideoTimeValidation(t *testing.T) {
	for _, timestamp := range []string{"-0.1", "1e999", "NaN", "Infinity", "0"} {
		t.Run(timestamp, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			body := fmt.Sprintf(`{"project":%q,"key":"flow/record","variant":"default","digest":"sha256:abcd","verdict":"flag","regions":[{"tone":"issue","text":"edge","t":%s}]}`, id, timestamp)
			rec := httptest.NewRecorder()
			New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/verdict", strings.NewReader(body)))
			wantStatus, wantNotes := http.StatusBadRequest, 0
			if timestamp == "0" {
				wantStatus, wantNotes = http.StatusCreated, 1
			}
			notes, err := store.Notes(id)
			if rec.Code != wantStatus || err != nil || len(notes) != wantNotes {
				t.Fatalf("time %s: status=%d body=%s notes=%v err=%v", timestamp, rec.Code, rec.Body.String(), notes, err)
			}
		})
	}
}

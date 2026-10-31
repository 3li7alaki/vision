package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vision/internal/store"
)

func TestRetired(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	now := time.Now().UTC()
	id := strings.Repeat("b", 64)
	if err := store.AppendSnap(id, store.Snap{Project: "hq", Key: "site/hero-lab-1", Variant: "mobile", Digest: "sha256:01", Session: "t1", TS: now.Add(-time.Hour)}, []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, w := range []store.Withdrawal{
		{Key: "site/hero-lab-1", Variant: "mobile", Digest: "sha256:01", Thread: "t1", Reason: "Pages deleted", TS: now.Add(-2 * time.Hour)},
		{Key: "site/hero-lab-2", Variant: "mobile", Digest: "sha256:02", Thread: "t1", Reason: "Pages deleted", TS: now.Add(-time.Minute)},
		{Key: "cart/empty", Variant: "mobile", Digest: "sha256:03", Thread: "t1", TS: now},                                               // plain discard
		{Key: "site/old", Variant: "mobile", Digest: "sha256:04", Thread: "t1", Reason: "Gone", TS: now.Add(-RetiredWindow - time.Hour)}, // outside the window
		{Key: "site/skewed", Variant: "mobile", Digest: "sha256:05", Thread: "t1", Reason: "Clock ahead", TS: now.Add(time.Hour)},        // skew: still shown
	} {
		if err := store.AppendWithdrawal(id, w); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/retired", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var items []RetiredItem
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Key != "site/skewed" || items[1].Key != "site/hero-lab-2" || items[2].Key != "site/hero-lab-1" {
		t.Fatalf("want the three reasoned withdrawals in the window, skewed one included, newest first, got %+v", items)
	}
	if items[1].Project != "hq" || items[1].ProjectID != id || items[1].Reason != "Pages deleted" || items[1].Thread != "t1" {
		t.Fatalf("item=%+v", items[1])
	}
}

func TestRetiredEmptyIsArray(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/retired", nil))
	if body := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || body != "[]" {
		t.Fatalf("status=%d body=%q", rec.Code, body)
	}
}

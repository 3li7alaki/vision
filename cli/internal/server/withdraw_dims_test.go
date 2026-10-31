package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"vision/internal/store"
)

func postWithdraw(t *testing.T, s *Server, req WithdrawRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/withdraw", bytes.NewReader(body)))
	return rec
}

func responseError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Error
}

func TestWithdrawDims(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%v", ambiguous), func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			variants := []string{"locale=x,scheme=dark,vp=desktop", "locale=y,scheme=dark,vp=desktop", "desktop"}
			if ambiguous {
				variants = append(variants, "locale=x,scheme=light,vp=desktop")
			}
			for _, variant := range variants {
				// Identical pixels in different variants still require separate reviews.
				snap := store.Snap{Key: "room/probe", Variant: variant, Digest: "sha256:aaaa", Thread: "work"}
				for range 2 {
					if err := store.AppendSnap(id, snap, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.AppendSnap(id, store.Snap{Key: "room/probe", Variant: "locale=x,scheme=light,vp=desktop", Digest: "sha256:bbbb", Thread: "other"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendWithdrawal(id, store.Withdrawal{Key: "room/old", Variant: "default", Digest: "sha256:cccc", Thread: "work"}); err != nil {
				t.Fatal(err)
			}
			dir, _ := store.ProjectDir(id)
			ledger := filepath.Join(dir, "withdrawn.jsonl")
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			s := New()
			events := make(chan Item, 4)
			s.clients[events] = struct{}{}
			rec := postWithdraw(t, s, WithdrawRequest{Project: id, Key: "room/probe", Dims: map[string]string{"locale": "x", "vp": "desktop"}, Thread: "work", Session: "successor"})
			if ambiguous {
				want := "room/probe matches several pending variants on this thread: locale=x,scheme=dark,vp=desktop, locale=x,scheme=light,vp=desktop; pass --as with one"
				if rec.Code != http.StatusConflict || responseError(t, rec) != want {
					t.Fatalf("ambiguous discard: status=%d body=%s", rec.Code, rec.Body.String())
				}
				if after, err := os.ReadFile(ledger); err != nil || !bytes.Equal(before, after) || len(events) != 0 {
					t.Fatalf("ambiguous discard changed ledger or events: %v", err)
				}
				return
			}
			if rec.Code != http.StatusCreated {
				t.Fatalf("discard: status=%d body=%s", rec.Code, rec.Body.String())
			}
			withdrawals, err := store.Withdrawals(id)
			if err != nil || len(withdrawals) != 2 || withdrawals[1].Variant != variants[0] || withdrawals[1].Thread != "work" || withdrawals[1].Session != "successor" || len(events) != 1 {
				t.Fatalf("withdrawals=%+v events=%d err=%v", withdrawals, len(events), err)
			}
			if event := <-events; event.Variant != variants[0] {
				t.Fatalf("event lost resolved variant: %+v", event)
			}
		})
	}
}

func TestReviewDimValidation(t *testing.T) {
	for _, endpoint := range []string{"withdraw", "retire"} {
		for _, tc := range []struct {
			name, variant string
			dims          map[string]string
		}{
			{"as and dims", "default", map[string]string{"locale": "x"}},
			{"unknown name", "", map[string]string{"unknown": "x"}},
			{"empty value", "", map[string]string{"locale": ""}},
			{"comma in value", "", map[string]string{"locale": "x,y"}},
			{"equals in value", "", map[string]string{"locale": "x=y"}},
			{"empty dims", "", map[string]string{}},
		} {
			t.Run(endpoint+"/"+tc.name, func(t *testing.T) {
				state := t.TempDir()
				t.Setenv("VISION_STATE_HOME", state)
				body := map[string]any{"project": strings.Repeat("a", 64), "key": "room/probe", "thread": "work", "variant": tc.variant, "dims": tc.dims}
				if endpoint == "retire" {
					delete(body, "key")
					body["pattern"], body["reason"] = "room/probe", "Page deleted"
				}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				New().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/"+endpoint, bytes.NewReader(data)))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
				}
				if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
					t.Fatalf("invalid dims wrote state: %v %v", entries, err)
				}
			})
		}
	}
}

func TestReviewMissOpenHere(t *testing.T) {
	for _, endpoint := range []string{"withdraw", "retire"} {
		for _, filter := range []string{"as", "dims"} {
			for _, count := range []int{0, 2, 12} {
				t.Run(fmt.Sprintf("%s/%s/count=%d", endpoint, filter, count), func(t *testing.T) {
					t.Setenv("VISION_STATE_HOME", t.TempDir())
					id := strings.Repeat("a", 64)
					ts := time.Now().UTC()
					var entries []string
					for i := count - 1; i >= 0; i-- {
						variant := fmt.Sprintf("locale=x,scheme=dark,vp=v%02d", i)
						snap := store.Snap{Key: "room/probe", Variant: variant, Digest: fmt.Sprintf("sha256:%04x", i), Thread: "work", TS: ts.Add(-time.Minute)}
						for range 2 {
							if err := store.AppendSnap(id, snap, nil); err != nil {
								t.Fatal(err)
							}
						}
						kind := "pending"
						if i%2 != 0 {
							kind = "flag"
							if err := store.AppendNote(id, store.Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "flag", Note: "fix", TS: ts}); err != nil {
								t.Fatal(err)
							}
						}
						entry := snap.Key + "@" + variant
						if endpoint == "withdraw" {
							entry += " (" + kind + ")"
						}
						entries = append(entries, entry)
					}
					for _, snap := range []store.Snap{
						{Key: "room/probe", Variant: "other-thread", Digest: "sha256:eeee", Thread: "other"},
						{Key: "room/unrelated", Variant: "default", Digest: "sha256:ffff", Thread: "work"},
					} {
						if err := store.AppendSnap(id, snap, nil); err != nil {
							t.Fatal(err)
						}
					}
					variant := "locale=x,vp=desktop"
					var dims map[string]string
					if filter == "dims" {
						variant, dims = "", map[string]string{"vp": "desktop", "locale": "x"}
					}
					var rec *httptest.ResponseRecorder
					pattern, action := "room/probe", "pending"
					if endpoint == "withdraw" {
						rec = postWithdraw(t, New(), WithdrawRequest{Project: id, Key: pattern, Variant: variant, Dims: dims, Thread: "work"})
					} else {
						pattern = "room/prob*"
						rec = postRetire(t, New(), RetireRequest{Project: id, Pattern: pattern, Variant: variant, Dims: dims, Thread: "work", Reason: "Page deleted"})
					}
					want := "nothing " + action + " for " + pattern + "@locale=x,vp=desktop on this thread"
					if endpoint == "retire" {
						want = "nothing matches " + pattern + "@locale=x,vp=desktop on this thread"
					}
					if count != 0 {
						slices.Sort(entries)
						if count > 10 {
							entries = append(entries[:10], fmt.Sprintf("and %d more", count-10))
						}
						if endpoint == "retire" {
							want += "; snapped here: " + strings.Join(entries, ", ")
						} else {
							want += "; open here: " + strings.Join(entries, ", ")
						}
					}
					if rec.Code != http.StatusNotFound || responseError(t, rec) != want {
						t.Fatalf("miss: status=%d body=%s, want %q", rec.Code, rec.Body.String(), want)
					}
					if withdrawals, err := store.Withdrawals(id); err != nil || len(withdrawals) != 0 {
						t.Fatalf("miss wrote withdrawals: %v %v", withdrawals, err)
					}
				})
			}
		}
	}
}

func TestWithdrawDimsOpaque(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	if err := store.AppendSnap(id, store.Snap{Key: "room/probe", Variant: "desktop", Digest: "sha256:aaaa", Session: "work"}, nil); err != nil {
		t.Fatal(err)
	}
	rec := postWithdraw(t, New(), WithdrawRequest{Project: id, Key: "room/probe", Dims: map[string]string{"vp": "desktop"}, Thread: "work"})
	want := "nothing pending for room/probe@vp=desktop on this thread; open here: room/probe@desktop (pending)"
	if rec.Code != http.StatusNotFound || responseError(t, rec) != want {
		t.Fatalf("opaque variant matched dims: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRetireEveryFlagDigest(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	ts := time.Now().UTC()
	old := store.Snap{Key: "room/probe", Variant: "locale=x,scheme=dark,vp=desktop", Digest: "sha256:aaaa", Thread: "work", TS: ts.Add(-2 * time.Minute)}
	newer := old
	newer.Digest, newer.TS = "sha256:bbbb", ts.Add(-time.Minute)
	already := old
	already.Digest = "sha256:dddd"
	other := old
	other.Digest, other.Thread = "sha256:cccc", "other"
	for _, snap := range []store.Snap{already, old, newer, other} {
		if err := store.AppendSnap(id, snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, snap := range []store.Snap{already, old, newer, other} {
		if err := store.AppendNote(id, store.Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "flag", Note: "fix", TS: ts.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendWithdrawal(id, store.Withdrawal{Key: already.Key, Variant: already.Variant, Digest: already.Digest, Thread: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendWithdrawal(id, store.Withdrawal{Key: old.Key, Variant: old.Variant, Digest: old.Digest, Thread: "other"}); err != nil {
		t.Fatal(err)
	}
	dir, _ := store.ProjectDir(id)
	before := make(map[string][]byte)
	for _, name := range []string{"index.jsonl", "notes.jsonl"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	req := RetireRequest{Project: id, Pattern: "room/probe", Dims: map[string]string{"locale": "x"}, Thread: "work", Reason: "Page deleted"}
	s := New()
	rec := postRetire(t, s, req)
	var response RetireResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || len(response.Retired) != 2 {
		t.Fatalf("retire: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	for i, digest := range []string{old.Digest, newer.Digest} {
		if review := response.Retired[i]; review.Digest != digest || review.Kind != "flag" {
			t.Fatalf("wrong flag retirement: %+v", review)
		}
	}
	if flags, err := store.OpenFlags(id, "work"); err != nil || len(flags) != 0 {
		t.Fatalf("retirement left flags open: %+v %v", flags, err)
	}
	if flags, err := store.OpenFlags(id, "other"); err != nil || len(flags) != 1 || flags[0].Digest != other.Digest {
		t.Fatalf("retirement crossed threads: %+v %v", flags, err)
	}
	withdrawals, err := store.Withdrawals(id)
	if err != nil || len(withdrawals) != 4 || withdrawals[2].Thread != "work" || withdrawals[3].Thread != "work" {
		t.Fatalf("withdrawals=%+v err=%v", withdrawals, err)
	}
	for name, data := range before {
		if after, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(data, after) {
			t.Fatalf("retirement changed %s: %v", name, err)
		}
	}
	rec = postRetire(t, s, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("repeat retirement: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if after, err := store.Withdrawals(id); err != nil || len(after) != len(withdrawals) {
		t.Fatalf("repeat retirement appended withdrawals: %v %v", after, err)
	}
}

func TestRetireSupersededFlag(t *testing.T) {
	for _, approved := range []bool{true, false} {
		t.Run(fmt.Sprintf("approved=%v", approved), func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			ts := time.Now().UTC()
			old := store.Snap{Key: "room/probe", Variant: "locale=x,scheme=dark,vp=desktop", Digest: "sha256:aaaa", Thread: "work", TS: ts.Add(-3 * time.Minute)}
			if err := store.AppendSnap(id, old, nil); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendNote(id, store.Note{Key: old.Key, Variant: old.Variant, Digest: old.Digest, Verdict: "flag", Note: "fix", TS: ts.Add(-2 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			newer := old
			newer.Digest, newer.TS = "sha256:bbbb", ts.Add(-time.Minute)
			if err := store.AppendSnap(id, newer, []byte("placeholder")); err != nil {
				t.Fatal(err)
			}
			if approved {
				if err := store.AppendNote(id, store.Note{Key: newer.Key, Variant: newer.Variant, Digest: newer.Digest, Verdict: "ok", TS: ts}); err != nil {
					t.Fatal(err)
				}
				if err := store.CopyBaseline(id, newer.Key, newer.Variant, newer.Digest); err != nil {
					t.Fatal(err)
				}
			}
			if flags, err := store.OpenFlags(id, "work"); err != nil || len(flags) != 0 {
				t.Fatalf("newer snap left flags open: %+v %v", flags, err)
			}
			if err := store.AppendWithdrawal(id, store.Withdrawal{Key: "room/old", Variant: "default", Digest: "sha256:cccc", Thread: "work"}); err != nil {
				t.Fatal(err)
			}
			dir, _ := store.ProjectDir(id)
			ledger := filepath.Join(dir, "withdrawn.jsonl")
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			s := New()
			req := RetireRequest{Project: id, Pattern: old.Key, Thread: "work", Reason: "Page deleted"}
			rec := postRetire(t, s, req)
			var response RetireResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || len(response.Retired) != 1 {
				t.Fatalf("retire: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
			}
			want := RetiredReview{Key: newer.Key, Variant: newer.Variant, Digest: newer.Digest, Kind: "pending"}
			if approved {
				want.Kind = "closed"
				baseline, _ := store.BaselinePath(id, newer.Key, newer.Variant)
				if _, err := os.Stat(baseline); !os.IsNotExist(err) {
					t.Errorf("retired baseline remains: %s: %v", baseline, err)
				}
			}
			if review := response.Retired[0]; review != want {
				t.Fatalf("wrong retirement: %+v, want %+v", review, want)
			}
			if withdrawals, err := store.Withdrawals(id); err != nil || len(withdrawals) != 2 || withdrawals[1].Key != newer.Key || withdrawals[1].Variant != newer.Variant || withdrawals[1].Digest != newer.Digest || withdrawals[1].Thread != "work" || withdrawals[1].Reason != req.Reason {
				t.Fatalf("wrong withdrawals: %+v %v", withdrawals, err)
			}
			after, err := os.ReadFile(ledger)
			if err != nil || !bytes.HasPrefix(after, before) || bytes.Equal(after, before) {
				t.Fatalf("retirement did not append withdrawals: %v", err)
			}
			rec = postRetire(t, s, req)
			if rec.Code != http.StatusNotFound || responseError(t, rec) != old.Key+" is already retired on this thread" {
				t.Fatalf("repeat retirement: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if repeat, err := os.ReadFile(ledger); err != nil || !bytes.Equal(after, repeat) {
				t.Fatalf("repeat retirement changed withdrawals: %v", err)
			}
		})
	}
}

func TestRetireClosedWithdrawals(t *testing.T) {
	ts := time.Now().UTC().Add(-time.Hour)
	snap := store.Snap{Key: "room/probe", Variant: "locale=x,vp=desktop", Digest: "sha256:bbbb", Thread: "work", TS: ts}
	withdrawal := store.Withdrawal{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Thread: snap.Thread, TS: ts.Add(time.Minute)}
	for _, tc := range []struct {
		name, reason, thread string
		ts                   time.Time
		retired              bool
	}{
		{"plain withdrawal", "", "work", withdrawal.TS, false},
		{"other thread", "Page deleted", "other", withdrawal.TS, false},
		{"older retirement", "Page deleted", "work", ts.Add(-time.Minute), false},
		{"equal retirement", "Page deleted", "work", ts, true},
		{"newer retirement", "Page deleted", "work", withdrawal.TS, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			id := strings.Repeat("a", 64)
			older := snap
			older.Digest, older.TS = "sha256:aaaa", ts.Add(-2*time.Minute)
			excluded := snap
			excluded.Variant = "locale=y,vp=desktop"
			// Timestamp order, not append order, picks the snap retirement must cover.
			for _, shot := range []store.Snap{snap, older, excluded} {
				if err := store.AppendSnap(id, shot, nil); err != nil {
					t.Fatal(err)
				}
				if err := store.AppendNote(id, store.Note{Key: shot.Key, Variant: shot.Variant, Digest: shot.Digest, Verdict: "ok", TS: ts}); err != nil {
					t.Fatal(err)
				}
			}
			w := withdrawal
			w.Reason, w.Thread, w.TS = tc.reason, tc.thread, tc.ts
			if tc.retired {
				w.Digest = older.Digest
			}
			if err := store.AppendWithdrawal(id, w); err != nil {
				t.Fatal(err)
			}
			dir, _ := store.ProjectDir(id)
			ledger := filepath.Join(dir, "withdrawn.jsonl")
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			rec := postRetire(t, New(), RetireRequest{Project: id, Pattern: "room/*", Dims: map[string]string{"locale": "x"}, Thread: "work", Reason: "Page deleted"})
			if tc.retired {
				if rec.Code != http.StatusNotFound || responseError(t, rec) != "room/*@locale=x is already retired on this thread" {
					t.Fatalf("already retired: status=%d body=%s", rec.Code, rec.Body.String())
				}
				if after, err := os.ReadFile(ledger); err != nil || !bytes.Equal(before, after) {
					t.Fatalf("already retired changed ledger: %v", err)
				}
				return
			}
			var response RetireResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || len(response.Retired) != 1 {
				t.Fatalf("retire: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
			}
			if want := (RetiredReview{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Kind: "closed"}); response.Retired[0] != want {
				t.Fatalf("wrong closed retirement: %+v, want %+v", response.Retired[0], want)
			}
		})
	}
}

func TestRetireDims(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	id := strings.Repeat("a", 64)
	ts := time.Now().UTC()
	variants := []string{"locale=x,scheme=dark,vp=desktop", "locale=x,scheme=light,vp=desktop", "locale=y,scheme=dark,vp=desktop", "desktop"}
	for i, variant := range variants {
		for key := 1; key <= 2; key++ {
			snap := store.Snap{Key: fmt.Sprintf("room/probe-%d", key), Variant: variant, Digest: fmt.Sprintf("sha256:%04x", i*2+key), Thread: "work", TS: ts.Add(-time.Minute)}
			if err := store.AppendSnap(id, snap, nil); err != nil {
				t.Fatal(err)
			}
			if key == 1 {
				if err := store.AppendNote(id, store.Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "flag", Note: "fix", TS: ts}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	rec := postRetire(t, New(), RetireRequest{Project: id, Pattern: "room/probe-*", Dims: map[string]string{"locale": "x", "vp": "desktop"}, Thread: "work", Reason: "Pages deleted"})
	var response RetireResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusCreated || len(response.Retired) != 4 {
		t.Fatalf("retire: status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
	for _, review := range response.Retired {
		kind := "pending"
		if review.Key == "room/probe-1" {
			kind = "flag"
		}
		if !slices.Contains(variants[:2], review.Variant) || review.Kind != kind {
			t.Fatalf("retired wrong variant or kind: %+v", review)
		}
	}
	if flags, err := store.OpenFlags(id, "work"); err != nil || len(flags) != 2 {
		t.Fatalf("wrong remaining flags: %v %v", flags, err)
	}
	if count, err := PendingCountForSession(id, "work"); err != nil || count != 2 {
		t.Fatalf("wrong remaining pending count: %d %v", count, err)
	}
}

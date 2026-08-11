package server

import (
	"testing"
	"time"

	"vision/internal/store"
)

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

package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Pruning is the one thing here that deletes, so it gets the sharpest test: the shot the
// human has not judged yet must survive, and the record of every shot must survive whether
// its picture did or not. Element snapshots must follow the picture's retention decision.
func TestPruneKeepsPendingBaselineAndRecent(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	shot := func(n int) string {
		digest := "sha256:" + strings.Repeat(string(rune('a'+n)), 8)
		snap := Snap{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: digest}
		if err := AppendSnap("p", snap, []byte{byte(n)}); err != nil {
			t.Fatal(err)
		}
		if err := WriteElements("p", digest, []Element{{Role: "button", Name: "Withdraw", W: 120, H: 40}}); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	approve := func(digest string) {
		if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "checkout/cart", Variant: "default", Digest: digest, Verdict: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	// Four approved in order, then one nobody has looked at.
	oldest, superseded, previous, baseline := shot(0), shot(1), shot(2), shot(3)
	for _, d := range []string{oldest, superseded, previous, baseline} {
		approve(d)
	}
	pending := shot(4)
	// Age both files past the grace window, which is covered by its own test below.
	old := time.Now().Add(-2 * pruneGrace)
	for _, d := range []string{oldest, superseded, previous, baseline, pending} {
		p, _ := ShotPath("p", d)
		snapshot, _ := ElementsPath("p", d)
		for _, path := range []string{p, snapshot} {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	exists := func(digest string) bool { return HasShot("p", Snap{Digest: digest}) }
	for _, keep := range []string{pending, baseline, previous, superseded} {
		if !exists(keep) {
			t.Errorf("pruned a shot it must keep: %s", keep)
		}
		if elements, err := ReadElements("p", keep); err != nil || len(elements) != 1 {
			t.Errorf("pruned a snapshot it must keep: %s: %#v %v", keep, elements, err)
		}
	}
	if exists(oldest) {
		t.Errorf("kept a shot past the retention window: %s", oldest)
	}
	if elements, err := ReadElements("p", oldest); err != nil || elements != nil {
		t.Errorf("kept a snapshot past the retention window: %#v %v", elements, err)
	}
	// The ledger is untouched: every shot still has its record, picture or not.
	snaps, err := Snaps("p")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 5 {
		t.Fatalf("prune changed the index: got %d records, want 5", len(snaps))
	}
}

// A snap writes its picture before its index line. A Prune that lands in between sees a
// picture or snapshot with no record, and must not delete evidence nobody has reviewed yet.
func TestPruneSparesAFreshPictureWithNoRecordYet(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	if err := AppendSnap("p", Snap{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: "sha256:aaaa"}, []byte{1}); err != nil {
		t.Fatal(err)
	}
	p, _ := ShotPath("p", "sha256:bbbb")
	if err := os.WriteFile(p, []byte{2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteElements("p", "sha256:bbbb", []Element{{Name: "Fresh", W: 10, H: 10}}); err != nil {
		t.Fatal(err)
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	if !HasShot("p", Snap{Digest: "sha256:bbbb"}) {
		t.Fatal("pruned a picture written moments ago, before its record landed")
	}
	if elements, err := ReadElements("p", "sha256:bbbb"); err != nil || len(elements) != 1 {
		t.Fatalf("pruned a snapshot written moments ago, before its record landed: %#v %v", elements, err)
	}
}

// A pending review must keep shared files even after another key's approval ages out of retention.
func TestPruneKeepsSharedPendingShot(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	const shared = "sha256:aaaa"
	add := func(key, digest string, approved bool) {
		t.Helper()
		thread := "pending"
		if approved {
			thread = "approved"
		}
		if err := AppendSnap("p", Snap{Key: key, Variant: "default", Digest: digest, Thread: thread}, []byte(digest)); err != nil {
			t.Fatal(err)
		}
		if approved {
			if err := AppendNote("p", Note{Key: key, Variant: "default", Digest: digest, Verdict: "ok"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("orders/list", shared, true)
	add("orders/theme", shared, false)
	// Approvals on another thread push shared pixels outside retention without superseding the pending review.
	for i := 0; i <= KeepSuperseded; i++ {
		digest := fmt.Sprintf("sha256:%04x", i)
		for _, key := range []string{"orders/list", "orders/theme"} {
			add(key, digest, true)
		}
	}
	if err := WriteElements("p", shared, []Element{{Role: "button", Name: "Order", W: 1, H: 1}}); err != nil {
		t.Fatal(err)
	}
	shot, err := ShotPath("p", shared)
	if err != nil {
		t.Fatal(err)
	}
	elements, err := ElementsPath("p", shared)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * pruneGrace)
	for _, path := range []string{shot, elements} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	if !HasShot("p", Snap{Digest: shared}) {
		t.Fatal("pruned shared pixels for the unjudged orders/theme review")
	}
	if got, err := ReadElements("p", shared); err != nil || len(got) != 1 {
		t.Fatalf("pruned shared elements for the unjudged orders/theme review: %#v %v", got, err)
	}
}

func appendPruneShot(t *testing.T, snap Snap, verdict string) {
	t.Helper()
	if err := AppendSnap("p", snap, []byte(snap.Digest)); err != nil {
		t.Fatal(err)
	}
	if err := WriteElements("p", snap.Digest, []Element{{Name: "Page", W: 1, H: 1}}); err != nil {
		t.Fatal(err)
	}
	if verdict != "" {
		if err := AppendNote("p", Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: verdict, Note: "reviewed", TS: snap.TS}); err != nil {
			t.Fatal(err)
		}
	}
	if verdict == "ok" {
		if err := CopyBaselineMedia("p", snap.Key, snap.Variant, snap.Digest, snap.Media); err != nil {
			t.Fatal(err)
		}
	}
	shot, _ := ShotPathMedia("p", snap.Digest, snap.Media)
	elements, _ := ElementsPath("p", snap.Digest)
	old := time.Now().Add(-2 * pruneGrace)
	for _, path := range []string{shot, elements} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneRetiredPairRemovesFilm(t *testing.T) {
	for media := range mediaExtensions {
		t.Run(media, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			ts := time.Now().Add(-time.Hour)
			var retired []Snap
			for i := range 4 {
				snap := Snap{Key: "retired/page", Variant: "default", Digest: fmt.Sprintf("sha256:%04x", i), Media: media, Thread: "work", TS: ts.Add(time.Duration(i) * time.Minute)}
				verdict := "flag"
				if i == 0 {
					verdict = "ok"
				}
				appendPruneShot(t, snap, verdict)
				// Retirement must reclaim even fresh, already-recorded film.
				shot, _ := ShotPathMedia("p", snap.Digest, media)
				elements, _ := ElementsPath("p", snap.Digest)
				for _, path := range []string{shot, elements} {
					if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				retired = append(retired, snap)
			}
			var baselines []string
			for format := range mediaExtensions {
				path, _ := BaselinePathMedia("p", "retired/page", "default", format)
				if err := os.WriteFile(path, []byte("baseline"), 0o644); err != nil {
					t.Fatal(err)
				}
				baselines = append(baselines, path)
			}
			live := Snap{Key: "live/page", Variant: "default", Digest: "sha256:aaaa", TS: ts}
			appendPruneShot(t, live, "ok")
			pending := live
			pending.Digest = "sha256:bbbb"
			appendPruneShot(t, pending, "")
			if err := AppendWithdrawal("p", Withdrawal{Key: "retired/page", Variant: "default", Digest: retired[3].Digest, Thread: "work", TS: ts.Add(time.Hour), Reason: "Page deleted"}); err != nil {
				t.Fatal(err)
			}
			dir, _ := ProjectDir("p")
			ledgers := make(map[string]string)
			for _, name := range []string{"index.jsonl", "notes.jsonl", "withdrawn.jsonl"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				ledgers[name] = string(data)
			}
			for range 2 {
				if err := Prune("p"); err != nil {
					t.Fatal(err)
				}
			}
			for _, snap := range retired {
				if HasShot("p", snap) {
					t.Errorf("kept retired shot %s", snap.Digest)
				}
				if elements, err := ReadElements("p", snap.Digest); err != nil || elements != nil {
					t.Errorf("kept retired elements: %v %v", elements, err)
				}
			}
			for _, path := range append(baselines, filepath.Join(dir, "base", "retired")) {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("retired baseline or empty directory remains: %s: %v", path, err)
				}
			}
			for _, snap := range []Snap{live, pending} {
				if !HasShot("p", snap) {
					t.Errorf("pruned live shot %s", snap.Digest)
				}
			}
			baseline, _ := BaselinePath("p", live.Key, live.Variant)
			if _, err := os.Stat(baseline); err != nil {
				t.Fatalf("pruned live baseline: %v", err)
			}
			for name, before := range ledgers {
				if after, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(after) != before {
					t.Errorf("prune changed %s: %v", name, err)
				}
			}
		})
	}
}

func TestPruneRetirementTimestamps(t *testing.T) {
	ts := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name        string
		snaps       []int
		withdrawals []int
		keep        bool
	}{
		{"newer snap revives", []int{2}, []int{1}, true},
		{"equal timestamp retires", []int{1}, []int{1}, false},
		{"newest retirement wins", []int{2}, []int{3, 1}, false},
		{"newest snap across threads wins", []int{2, 0}, []int{1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			baseline := Snap{Key: "site/page", Variant: "default", Digest: "sha256:aaaa", Thread: "original", TS: ts}
			appendPruneShot(t, baseline, "ok")
			snaps := []Snap{baseline}
			for i, minute := range tc.snaps {
				snap := baseline
				snap.Digest, snap.Thread, snap.TS = fmt.Sprintf("sha256:%04x", i), fmt.Sprint(i), ts.Add(time.Duration(minute)*time.Minute)
				appendPruneShot(t, snap, "flag")
				snaps = append(snaps, snap)
			}
			for _, minute := range tc.withdrawals {
				if err := AppendWithdrawal("p", Withdrawal{Key: baseline.Key, Variant: baseline.Variant, Digest: baseline.Digest, Thread: baseline.Thread, TS: ts.Add(time.Duration(minute) * time.Minute), Reason: "Page deleted"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := Prune("p"); err != nil {
				t.Fatal(err)
			}
			for _, snap := range snaps {
				if got := HasShot("p", snap); got != tc.keep {
					t.Errorf("shot %s exists=%v, want %v", snap.Digest, got, tc.keep)
				}
			}
			path, _ := BaselinePath("p", baseline.Key, baseline.Variant)
			if _, err := os.Stat(path); (err == nil) != tc.keep || (err != nil && !errors.Is(err, os.ErrNotExist)) {
				t.Errorf("baseline: %v, want kept=%v", err, tc.keep)
			}
			dir, _ := ProjectDir("p")
			if _, err := os.Stat(filepath.Join(dir, "base")); err != nil {
				t.Errorf("pruned base directory: %v", err)
			}
		})
	}
}

func TestPruneKeepsPendingRetiredPair(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	ts := time.Now().Add(-time.Hour)
	baseline := Snap{Key: "site/page", Variant: "default", Digest: "sha256:aaaa", Thread: "A", TS: ts}
	appendPruneShot(t, baseline, "ok")
	retired := baseline
	retired.Digest = "sha256:bbbb"
	appendPruneShot(t, retired, "")
	pending := baseline
	pending.Digest, pending.Thread = "sha256:cccc", "B"
	appendPruneShot(t, pending, "")
	if err := AppendWithdrawal("p", Withdrawal{Key: retired.Key, Variant: retired.Variant, Digest: retired.Digest, Thread: "A", TS: ts.Add(time.Minute), Reason: "Page deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	for _, snap := range []Snap{baseline, retired, pending} {
		if !HasShot("p", snap) {
			t.Errorf("pruned shot while another thread is pending: %s", snap.Digest)
		}
	}
	path, _ := BaselinePath("p", baseline.Key, baseline.Variant)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pruned baseline while another thread is pending: %v", err)
	}
}

func TestPruneKeepsLiveSharedDigest(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameKey=%v", sameKey), func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			dead := Snap{Key: "site/retired", Variant: "default", Digest: "sha256:aaaa", TS: time.Now().Add(-time.Hour)}
			appendPruneShot(t, dead, "ok")
			live := dead
			if sameKey {
				live.Variant = "mobile"
			} else {
				live.Key = "site/live"
			}
			appendPruneShot(t, live, "ok")
			if err := AppendWithdrawal("p", Withdrawal{Key: dead.Key, Variant: dead.Variant, Digest: dead.Digest, TS: time.Now(), Reason: "Page deleted"}); err != nil {
				t.Fatal(err)
			}
			if err := Prune("p"); err != nil {
				t.Fatal(err)
			}
			if !HasShot("p", live) {
				t.Fatal("pruned digest shared with live pair")
			}
			if elements, err := ReadElements("p", live.Digest); err != nil || len(elements) != 1 {
				t.Fatalf("pruned shared elements: %v %v", elements, err)
			}
			for _, snap := range []Snap{dead, live} {
				path, _ := BaselinePath("p", snap.Key, snap.Variant)
				want := snap.Key == live.Key && snap.Variant == live.Variant
				if _, err := os.Stat(path); (err == nil) != want || (err != nil && !errors.Is(err, os.ErrNotExist)) {
					t.Errorf("baseline %s@%s: %v", snap.Key, snap.Variant, err)
				}
			}
		})
	}
}

func TestPruneDiscardKeepsBaseline(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	snap := Snap{Key: "site/page", Variant: "default", Digest: "sha256:aaaa", Thread: "work", TS: time.Now().Add(-time.Hour)}
	appendPruneShot(t, snap, "ok")
	discarded := snap
	discarded.Digest = "sha256:bbbb"
	appendPruneShot(t, discarded, "")
	if err := AppendWithdrawal("p", Withdrawal{Key: discarded.Key, Variant: discarded.Variant, Digest: discarded.Digest, Thread: discarded.Thread, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	for _, shot := range []Snap{snap, discarded} {
		if !HasShot("p", shot) {
			t.Errorf("pruned retained shot after plain discard: %s", shot.Digest)
		}
	}
	path, _ := BaselinePath("p", snap.Key, snap.Variant)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pruned baseline after plain discard: %v", err)
	}
}

func TestClosedSupersedesWithinThread(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	old := Snap{Key: "cart/empty", Variant: "default", Digest: "old", Session: "mine"}
	live := old
	live.Digest = "live"
	other := old
	other.Session = "other"
	otherKey := old
	otherKey.Key = "cart/full"
	otherVariant := old
	otherVariant.Variant = "dark"
	snaps := []Snap{old, live, live, other, otherKey, otherVariant}
	closed, err := Closed("p", snaps)
	if err != nil {
		t.Fatal(err)
	}
	if !closed[old.Review()] {
		t.Fatal("older digest on the same thread stayed pending")
	}
	for _, snap := range snaps[1:] {
		if closed[snap.Review()] {
			t.Errorf("closed live or independent review: %+v", snap)
		}
	}
	// Returning to earlier pixels restores that same review rather than inventing a new one.
	closed, err = Closed("p", append(snaps, old))
	if err != nil || closed[old.Review()] || !closed[live.Review()] {
		t.Fatalf("return to old digest: closed=%v err=%v", closed, err)
	}
}

func TestWithdrawalRoundTrip(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	if values, err := Withdrawals("p"); err != nil || len(values) != 0 {
		t.Fatalf("missing ledger: withdrawals=%v err=%v", values, err)
	}
	first := Withdrawal{SchemaVersion: SchemaVersion, TS: time.Now().UTC(), Key: "cart/empty", Variant: "default", Digest: "old", Session: "mine", Thread: "thread"}
	if err := AppendWithdrawal("p", first); err != nil {
		t.Fatal(err)
	}
	dir, _ := ProjectDir("p")
	path := filepath.Join(dir, "withdrawn.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Digest = "live"
	if err := AppendWithdrawal("p", second); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(after), string(before)) || strings.Count(string(after), "\n") != 2 {
		t.Fatalf("withdrawal rewrote its ledger: before=%q after=%q err=%v", before, after, err)
	}
	values, err := Withdrawals("p")
	if err != nil || !reflect.DeepEqual(values, []Withdrawal{first, second}) {
		t.Fatalf("withdrawals=%v err=%v", values, err)
	}
	if notes, err := Notes("p"); err != nil || len(notes) != 0 {
		t.Fatalf("agent withdrawal entered human notes: %v %v", notes, err)
	}
}

func TestClosedDecidedWithdrawnSuperseded(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	snaps := []Snap{
		{Key: "cart/decided", Variant: "default", Digest: "flag", Thread: "mine"},
		{Key: "cart/decided", Variant: "default", Digest: "flag", Thread: "other"},
		{Key: "cart/withdrawn", Variant: "default", Digest: "withdrawn", Thread: "mine"},
		{Key: "cart/withdrawn", Variant: "default", Digest: "withdrawn", Thread: "other"},
		{Key: "cart/superseded", Variant: "default", Digest: "old", Thread: "mine"},
		{Key: "cart/superseded", Variant: "default", Digest: "live", Thread: "mine"},
	}
	if err := AppendNote("p", Note{Key: snaps[0].Key, Variant: "default", Digest: "flag", Verdict: "flag", Note: "fix"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendWithdrawal("p", Withdrawal{Key: snaps[2].Key, Variant: "default", Digest: "withdrawn", Session: "successor", Thread: "mine"}); err != nil {
		t.Fatal(err)
	}
	closed, err := Closed("p", snaps)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, true, false, true, false} {
		if got := closed[snaps[i].Review()]; got != want {
			t.Errorf("review %d closed=%v, want %v", i, got, want)
		}
	}
}

func TestPruneReclaimsClosedReviews(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	snaps := []Snap{
		{Digest: "sha256:0000", Thread: "approved"},
		{Digest: "sha256:1111", Thread: "abandoned"},
		{Digest: "sha256:2222", Thread: "iterating"},
		{Digest: "sha256:3333", Thread: "iterating"},
		{Digest: "sha256:4444", Thread: "iterating"},
		{Digest: "sha256:5555", Thread: "iterating"},
		{Digest: "sha256:6666", Thread: "iterating"},
	}
	old := time.Now().Add(-2 * pruneGrace)
	for i := range snaps {
		snaps[i].Key, snaps[i].Variant = "cart/empty", "default"
		if err := AppendSnap("p", snaps[i], []byte(snaps[i].Digest)); err != nil {
			t.Fatal(err)
		}
		path, _ := ShotPath("p", snaps[i].Digest)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendNote("p", Note{Key: "cart/empty", Variant: "default", Digest: snaps[0].Digest, Verdict: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := CopyBaseline("p", "cart/empty", "default", snaps[0].Digest); err != nil {
		t.Fatal(err)
	}
	if err := AppendWithdrawal("p", Withdrawal{Key: "cart/empty", Variant: "default", Digest: snaps[1].Digest, Thread: "abandoned"}); err != nil {
		t.Fatal(err)
	}
	if err := Prune("p"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, false, true, true, true, true} {
		if got := HasShot("p", snaps[i]); got != want {
			t.Errorf("shot %d exists=%v, want %v", i, got, want)
		}
	}
	if values, err := Snaps("p"); err != nil || len(values) != len(snaps) {
		t.Fatalf("prune changed index: %v %v", values, err)
	}
	if values, err := Withdrawals("p"); err != nil || len(values) != 1 {
		t.Fatalf("prune changed withdrawals: %v %v", values, err)
	}
}

// Pixel-identical captures share one digest, so the first snapshot must survive later DOM changes.
func TestElementsStorage(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	const digest = "sha256:0123456789abcdef"
	path, err := ElementsPath("p", digest)
	if err != nil {
		t.Fatal(err)
	}
	shot, err := ShotPath("p", digest)
	if err != nil {
		t.Fatal(err)
	}
	if path != strings.TrimSuffix(shot, ".png")+".json" {
		t.Fatalf("snapshot path %q does not match picture %q", path, shot)
	}
	if got, err := ReadElements("p", digest); err != nil || got != nil {
		t.Fatalf("missing snapshot: %#v %v", got, err)
	}
	for _, empty := range [][]Element{nil, {}} {
		if err := WriteElements("p", digest, empty); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("empty elements created a file: %v", err)
		}
	}
	want := []Element{{Role: "button", Name: "Withdraw", Depth: 3, X: 150.25, Y: 260.5, W: 120.75, H: 40}}
	if err := WriteElements("p", digest, want); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadElements("p", digest); err != nil || !slices.Equal(got, want) {
		t.Fatalf("round trip: got %#v %v, want %#v", got, err, want)
	}
	if err := WriteElements("p", digest, []Element{{Role: "link", Name: "Changed", W: 10, H: 10}}); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadElements("p", digest); err != nil || !slices.Equal(got, want) {
		t.Fatalf("later write replaced the first DOM: %#v %v", got, err)
	}
}

// Regions are additive to schema 1, so old verdict lines must decode alongside marked verdicts.
func TestNoteRegionsBackwardCompatible(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	dir, err := ProjectDir("p")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"schemaVersion":1,"ts":"2026-10-11T10:00:00Z","key":"checkout/cart","variant":"default","digest":"sha256:old","verdict":"flag","note":"CTA under the fold"}`
	if err := os.WriteFile(filepath.Join(dir, "notes.jsonl"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := Note{SchemaVersion: 1, TS: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC), Key: "checkout/cart", Variant: "default", Digest: "sha256:old", Verdict: "flag", Note: "CTA under the fold"}
	marked := Note{SchemaVersion: 1, TS: legacy.TS, Key: legacy.Key, Variant: legacy.Variant, Digest: "sha256:new", Verdict: "flag", Note: "Move the CTA", Regions: []Region{
		{X: 300, Y: 520, W: 240, H: 80, Tone: "bad", Text: "Move this", Flow: true, Element: "button Withdraw"},
		{X: 20, Y: 40, W: 0, H: 0, Tone: "good", Text: "Keep this"},
	}}
	if err := AppendNote("p", marked); err != nil {
		t.Fatal(err)
	}
	if got, err := Notes("p"); err != nil || !reflect.DeepEqual(got, []Note{legacy, marked}) {
		t.Fatalf("notes: got %#v %v, want legacy and marked verdicts", got, err)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["regions"]; exists {
		t.Fatalf("empty regions changed the legacy JSON shape: %s", data)
	}
}

func TestParseKeyAndVariant(t *testing.T) {
	for _, key := range []string{"checkout/empty-cart", "checkout/happy-path#12"} {
		if err := ParseKey(key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	if err := ParseKey("site.v2/hero..wide#3"); err != nil {
		t.Errorf("dots inside a segment are fine: %v", err)
	}
	for _, key := range []string{"checkout", "/empty", "checkout/happy#0", "checkout/happy#x", "../x", "./x", "x/..", "x/.", "../x#2"} {
		if ParseKey(key) == nil {
			t.Errorf("accepted %s", key)
		}
	}
	if ParseVariant("mobile-dark") != nil || ParseVariant("mobile/dark") == nil {
		t.Fatal("variant validation wrong")
	}
	for _, variant := range []string{"unknown=value", "vp=mobile,vp=desktop", "vp=mobile,scheme=dark"} {
		if ParseVariant(variant) == nil {
			t.Errorf("accepted malformed encoded variant %q", variant)
		}
	}
}

func TestEncodeVariantCanonical(t *testing.T) {
	first := map[string]string{"scheme": "dark", "state": "empty", "vp": "mobile"}
	second := make(map[string]string)
	second["vp"] = "mobile"
	second["state"] = "empty"
	second["scheme"] = "dark"
	a, err := EncodeVariant(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeVariant(second)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != "scheme=dark,state=empty,vp=mobile" {
		t.Fatalf("encoded variants differ: %q %q", a, b)
	}
}

func TestVariantCodec(t *testing.T) {
	want := map[string]string{"case": "error", "locale": "ar", "role": "admin"}
	encoded, err := EncodeVariant(want)
	if err != nil {
		t.Fatal(err)
	}
	got := DecodeVariant(encoded)
	if len(got) != len(want) {
		t.Fatalf("decoded length: got %d, want %d", len(got), len(want))
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s: got %q, want %q", key, got[key], value)
		}
	}
	for _, legacy := range []string{"default", "mobile-dark"} {
		if decoded := DecodeVariant(legacy); decoded != nil {
			t.Errorf("legacy %q decoded as %#v, want nil", legacy, decoded)
		}
	}
}

func TestEncodeVariantRejectsInvalidDimensions(t *testing.T) {
	tests := map[string]map[string]string{
		"unknown key":  {"schema": "dark"},
		"empty key":    {"": "dark"},
		"empty value":  {"scheme": ""},
		"equals key":   {"sche=me": "dark"},
		"comma key":    {"sche,me": "dark"},
		"equals value": {"scheme": "da=rk"},
		"comma value":  {"scheme": "da,rk"},
	}
	for name, dims := range tests {
		t.Run(name, func(t *testing.T) {
			if encoded, err := EncodeVariant(dims); err == nil {
				t.Fatalf("encoded invalid dimensions as %q", encoded)
			}
		})
	}
}

func TestViewportClassBoundaries(t *testing.T) {
	tests := map[int]string{599: "mobile", 600: "tablet", 1023: "tablet", 1024: "desktop"}
	for width, want := range tests {
		if got := ViewportClass(width); got != want {
			t.Errorf("width %d: got %q, want %q", width, got, want)
		}
	}
}

func TestProjectIdentityDirectoryFallback(t *testing.T) {
	dir := t.TempDir()
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	project, err := Identify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if project.Root != root || project.Worktree != root || project.Name != filepath.Base(root) {
		t.Fatalf("directory identity: %+v, want root %q", project, root)
	}
	if project.Branch != "" || project.SHA != "" || project.Dirty {
		t.Fatalf("directory has Git metadata: %+v", project)
	}
	if len(project.ID) != 64 || strings.Trim(project.ID, "0123456789abcdef") != "" {
		t.Fatalf("ID is not lowercase SHA-256 hex: %q", project.ID)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, path := range []string{dir, ".", alias} {
		got, err := Identify(path)
		if err != nil || got != project {
			t.Errorf("Identify(%q) = %+v, %v; want %+v", path, got, err, project)
		}
	}
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{t.TempDir(), child} {
		got, err := Identify(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID == project.ID {
			t.Errorf("different directory %q shares identity %q", path, got.ID)
		}
	}
	if _, err := Identify(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("identified a missing directory")
	}
	// Initializing Git at the same path must enter a separate identity namespace.
	runGit(t, dir, "init")
	gitProject, err := Identify(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte("\n"+root)))
	if gitProject.ID != want || gitProject.ID == project.ID {
		t.Fatalf("Git identity = %q, want %q, distinct from %q", gitProject.ID, want, project.ID)
	}
}

func TestProjectIdentityDirectoryFallbackWithTranslatedGit(t *testing.T) {
	dir := t.TempDir()
	bin := t.TempDir()
	const script = `#!/bin/sh
if [ "$LC_ALL" = C ]; then
    printf '%s\n' 'fatal: not a git repository (or any of the parent directories): .git' >&2
else
    printf '%s\n' 'fatal: Kein Git-Repository (oder irgendeines der Elternverzeichnisse): .git' >&2
fi
exit 128
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	project, err := Identify(dir)
	if err != nil {
		t.Fatalf("directory fallback must work with translated Git: %v", err)
	}
	want := Project{ID: fmt.Sprintf("%x", sha256.Sum256([]byte("\x00"+root))), Name: filepath.Base(root), Root: root, Worktree: root}
	if project != want {
		t.Fatalf("directory identity: got %+v, want %+v", project, want)
	}
}

func TestProjectIdentityRequiresWorkingGit(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Run("missing git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := Identify(child); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("Identify with no git: got %v, want exec.ErrNotFound", err)
		}
	})
	t.Run("dubious ownership", func(t *testing.T) {
		bin := t.TempDir()
		const diagnostic = "fatal: detected dubious ownership in repository"
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' '"+diagnostic+"' >&2\nexit 128\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		_, err := Identify(child)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || strings.TrimSpace(string(exitErr.Stderr)) != diagnostic {
			t.Fatalf("Identify must preserve Git failure: got %v", err)
		}
	})
}

func TestProjectIdentitySharedByLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "x")
	runGit(t, root, "commit", "-m", "initial")
	runGit(t, root, "remote", "add", "origin", "git@github.com:Owner/Repo.git")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, root, "worktree", "add", linked, "-b", "linked")
	a, err := Identify(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Identify(linked)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Root != b.Root {
		t.Fatalf("identities differ: %#v %#v", a, b)
	}
}

func TestUnreadNotesCursor(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	for i := 0; i < 2; i++ {
		if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: string(rune('a' + i)), Verdict: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := UnreadNotes("p")
	if err != nil || len(first) != 2 {
		t.Fatalf("first: %d %v", len(first), err)
	}
	second, err := UnreadNotes("p")
	if err != nil || len(second) != 0 {
		t.Fatalf("second: %d %v", len(second), err)
	}
	if err := AppendNote("p", Note{SchemaVersion: 1, TS: time.Now(), Key: "a/b", Variant: "default", Digest: "c", Verdict: "flag", Note: "bad"}); err != nil {
		t.Fatal(err)
	}
	third, err := UnreadNotes("p")
	if err != nil || len(third) != 1 || third[0].Digest != "c" {
		t.Fatalf("third: %#v %v", third, err)
	}
}

func TestSessionNotesAndIndependentCursors(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	session := "../../untrusted/session"
	for _, snap := range []Snap{
		{Session: session, Digest: "mine"},
		{Session: "other", Digest: "theirs"},
		{Session: "other", Digest: "shared"},
		{Session: session, Digest: "shared"},
		{Session: session, Digest: "shared"},
	} {
		if err := AppendSnap("p", snap, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A matching session in another project must not pull its digest into this view.
	if err := AppendSnap("elsewhere", Snap{Session: session, Digest: "foreign"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{"mine", "theirs", "shared", "foreign"} {
		if err := AppendNote("p", Note{Digest: digest}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(read func() ([]Note, error), want ...string) {
		t.Helper()
		notes, err := read()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, note := range notes {
			got = append(got, note.Digest)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("notes: got %v, want %v", got, want)
		}
	}
	mine := func() ([]Note, error) { return UnreadNotesForSession("p", session) }
	theirs := func() ([]Note, error) { return UnreadNotesForSession("p", "other") }
	all := func() ([]Note, error) { return UnreadNotes("p") }
	check(all, "mine", "theirs", "shared", "foreign")
	dir, err := ProjectDir("p")
	if err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "cursor.json")
	// An unreadable global cursor must not affect a session read at all. Keeping these
	// bytes intact also proves that a scoped read never advances the old shared cursor.
	if err := os.WriteFile(global, []byte("invalid global cursor"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(mine, "mine", "shared")
	check(mine)
	check(theirs, "theirs", "shared")
	if b, err := os.ReadFile(global); err != nil || string(b) != "invalid global cursor" {
		t.Fatalf("session touched global cursor: %q %v", b, err)
	}
	for _, id := range []string{session, "other"} {
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
		b, err := os.ReadFile(filepath.Join(dir, "cursors", hash[:16]+".json"))
		if err != nil || string(b) != "{\"offset\":4}\n" {
			t.Fatalf("session cursor: %q %v", b, err)
		}
	}
	if err := os.WriteFile(global, []byte("{\"offset\":4}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendNote("p", Note{Digest: "shared", Verdict: "flag"}); err != nil {
		t.Fatal(err)
	}
	check(mine, "shared")
	check(all, "shared")
	check(theirs, "shared")
	check(func() ([]Note, error) { return NotesForSession("p", session) }, "mine", "shared", "shared")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

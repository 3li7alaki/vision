package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Closed media paths keep old PNG records usable and reject formats the store cannot serve honestly.
func TestMediaPathsAndValidation(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	dir, err := ProjectDir("p")
	if err != nil {
		t.Fatal(err)
	}
	for media, ext := range map[string]string{"": ".png", "image/png": ".png", "video/mp4": ".mp4", "video/webm": ".webm"} {
		t.Run(media, func(t *testing.T) {
			if got, err := MediaExtension(media); err != nil || got != ext {
				t.Fatalf("extension=%q err=%v, want %q", got, err, ext)
			}
			if err := ValidateMedia(media); err != nil {
				t.Fatal(err)
			}
			shot, err := ShotPathMedia("p", "sha256:abcd", media)
			if err != nil || shot != filepath.Join(dir, "shots", "abcd"+ext) {
				t.Fatalf("shot=%q err=%v", shot, err)
			}
			base, err := BaselinePathMedia("p", "flow/record#1", "default", media)
			if err != nil || base != filepath.Join(dir, "base", "flow", "record#1@default"+ext) {
				t.Fatalf("baseline=%q err=%v", base, err)
			}
		})
	}
	legacyShot, _ := ShotPath("p", "sha256:abcd")
	pngShot, _ := ShotPathMedia("p", "sha256:abcd", "image/png")
	legacyBase, _ := BaselinePath("p", "flow/record#1", "default")
	pngBase, _ := BaselinePathMedia("p", "flow/record#1", "default", "image/png")
	if legacyShot != pngShot || legacyBase != pngBase {
		t.Fatal("legacy paths must remain the PNG paths")
	}
	for _, media := range []string{"video/avi", "VIDEO/MP4", "../mp4"} {
		if ValidateMedia(media) == nil {
			t.Fatalf("accepted unknown media %q", media)
		}
		if _, err := ShotPathMedia("p", "sha256:abcd", media); err == nil {
			t.Fatalf("shot path guessed unknown media %q", media)
		}
		if _, err := BaselinePathMedia("p", "flow/record", "default", media); err == nil {
			t.Fatalf("baseline path guessed unknown media %q", media)
		}
		if err := AppendSnap("p", Snap{Digest: "sha256:abcd", Media: media}, nil); err == nil {
			t.Fatalf("stored unknown media %q", media)
		}
	}
}

// A video approval must copy its own immutable bytes while leaving an older PNG baseline alone.
func TestVideoStorageRoundTrip(t *testing.T) {
	t.Setenv("VISION_STATE_HOME", t.TempDir())
	snap := Snap{SchemaVersion: SchemaVersion, Key: "flow/record", Variant: "default", Digest: "sha256:abcd", Media: "video/mp4"}
	data := []byte("\x00\x00\x00\x18ftypisom\x00\x00\x00\x00")
	image := snap
	image.Media, image.Digest = "", "sha256:dcba"
	if err := AppendSnap("p", image, []byte("old PNG baseline")); err != nil {
		t.Fatal(err)
	}
	if err := CopyBaseline("p", image.Key, image.Variant, image.Digest); err != nil {
		t.Fatal(err)
	}
	if err := AppendSnap("p", snap, data); err != nil {
		t.Fatal(err)
	}
	if !HasShot("p", snap) || HasShot("p", Snap{Digest: snap.Digest}) {
		t.Fatal("HasShot must use the snap's media, not probe other extensions")
	}
	if err := AppendSnap("p", snap, []byte("must not replace immutable content")); err != nil {
		t.Fatal(err)
	}
	if err := CopyBaselineMedia("p", snap.Key, snap.Variant, snap.Digest, snap.Media); err != nil {
		t.Fatal(err)
	}
	shot, _ := ShotPathMedia("p", snap.Digest, snap.Media)
	baseline, _ := BaselinePathMedia("p", snap.Key, snap.Variant, snap.Media)
	for _, path := range []string{shot, baseline} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s: bytes=%q err=%v", path, got, err)
		}
	}
	pngBaseline, _ := BaselinePath("p", snap.Key, snap.Variant)
	if got, err := os.ReadFile(pngBaseline); err != nil || string(got) != "old PNG baseline" {
		t.Fatalf("old baseline=%q err=%v", got, err)
	}
	snaps, err := Snaps("p")
	if err != nil || len(snaps) != 3 || snaps[1].Media != snap.Media || snaps[1].SchemaVersion != 1 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
}

// Video film must obey the same retention window without ever deleting pending evidence or ledger lines.
func TestPruneVideoRetention(t *testing.T) {
	for _, media := range []string{"video/mp4", "video/webm"} {
		t.Run(media, func(t *testing.T) {
			t.Setenv("VISION_STATE_HOME", t.TempDir())
			var snaps []Snap
			old := time.Now().Add(-2 * pruneGrace)
			for i := 0; i < KeepSuperseded+3; i++ {
				snap := Snap{Key: "flow/record", Variant: "default", Digest: fmt.Sprintf("sha256:%04x", i), Media: media}
				if err := AppendSnap("p", snap, []byte{byte(i)}); err != nil {
					t.Fatal(err)
				}
				if i < KeepSuperseded+2 {
					if err := AppendNote("p", Note{Key: snap.Key, Variant: snap.Variant, Digest: snap.Digest, Verdict: "ok"}); err != nil {
						t.Fatal(err)
					}
				}
				path, _ := ShotPathMedia("p", snap.Digest, media)
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				snaps = append(snaps, snap)
			}
			if err := Prune("p"); err != nil {
				t.Fatal(err)
			}
			for i, snap := range snaps {
				if HasShot("p", snap) != (i != 0) {
					t.Fatalf("shot %d: kept=%v, want %v", i, HasShot("p", snap), i != 0)
				}
			}
			if records, err := Snaps("p"); err != nil || len(records) != len(snaps) {
				t.Fatalf("prune changed index: %v %v", records, err)
			}
			if notes, err := Notes("p"); err != nil || len(notes) != KeepSuperseded+2 {
				t.Fatalf("prune changed notes: %v %v", notes, err)
			}
		})
	}
}

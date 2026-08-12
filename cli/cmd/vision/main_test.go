package main

import (
	"bytes"
	"errors"
	"maps"
	"os/exec"
	"path/filepath"
	"testing"

	"vision/internal/store"
)

func TestMainHelp(t *testing.T) {
	bin := buildBin(t)
	for _, spelling := range []string{"-h", "--help", "help"} {
		t.Run(spelling, func(t *testing.T) {
			cmd := exec.Command(bin, spelling)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("help must exit 0: %v (stderr=%q)", err, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("help must print to stdout (stderr=%q)", stderr.String())
			}
		})
	}
}

func TestMainUnknownArgument(t *testing.T) {
	bin := buildBin(t)
	cmd := exec.Command(bin, "definitely-not-a-command")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err == nil || !errors.As(err, &exitErr) {
		t.Fatalf("an unrecognized argument must exit non-zero: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("an unrecognized argument must not write stdout: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatalf("an unrecognized argument must print a usage message to stderr")
	}
}

// A capture that could not report its scheme or its viewport is the normal failure here, not
// an exotic one: the browser this talks to drops evals and returns responses with no viewport
// under load. What must never happen is that gap turning into a missing dimension (which
// shares one baseline across both schemes) or into a confident wrong bucket.
func TestDeriveDims(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dims       map[string]string
		conditions store.Conditions
		want       map[string]string
	}{
		{
			name:       "measured",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{Scheme: "dark", Width: 390},
			want:       map[string]string{"flow": "payment", "scheme": "dark", "vp": "mobile"},
		},
		{
			name:       "explicit wins over measured",
			dims:       map[string]string{"scheme": "light", "vp": "desktop"},
			conditions: store.Conditions{Scheme: "dark", Width: 390},
			want:       map[string]string{"scheme": "light", "vp": "desktop"},
		},
		{
			name:       "browser told us nothing",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{},
			want:       map[string]string{"flow": "payment", "scheme": "unknown", "vp": "unknown"},
		},
		{
			name:       "negative width is not mobile",
			dims:       map[string]string{"flow": "payment"},
			conditions: store.Conditions{Scheme: "light", Width: -1},
			want:       map[string]string{"flow": "payment", "scheme": "light", "vp": "unknown"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deriveDims(tc.dims, tc.conditions)
			if !maps.Equal(tc.dims, tc.want) {
				t.Fatalf("deriveDims produced %v, want %v", tc.dims, tc.want)
			}
			// Whatever it produced has to name a baseline, or a flaky capture takes a
			// successful screenshot and then throws it away at the last step.
			if _, err := store.EncodeVariant(tc.dims); err != nil {
				t.Fatalf("derived dimensions must always encode: %v", err)
			}
		})
	}
}

func buildBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}
	return bin
}

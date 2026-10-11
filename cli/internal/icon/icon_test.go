package icon

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRankedPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"basename rank", []string{"apps/web/logo.svg", "public/favicon.ico", "public/favicon.svg"}, []string{"public/favicon.svg", "apps/web/logo.svg", "public/favicon.ico"}},
		{"excluded segments", []string{"node_modules/x/favicon.svg", "test/fixtures/favicon.svg", "public/logo.svg"}, []string{"public/logo.svg"}},
		{"depth and lexical", []string{"z/favicon.svg", "apps/web/favicon.svg", "a/favicon.svg"}, []string{"a/favicon.svg", "z/favicon.svg", "apps/web/favicon.svg"}},
		{"exact segment", []string{"test-app/favicon.svg", "public/mylogo.svg"}, []string{"test-app/favicon.svg"}},
		{"empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rankedPaths(tc.files); !slices.Equal(got, tc.want) {
				t.Fatalf("rankedPaths(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
	for segment := range excludedSegments {
		if got := rankedPaths([]string{"public/" + segment + "/favicon.svg"}); len(got) != 0 {
			t.Errorf("excluded segment %q survived: %v", segment, got)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "vision")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestConfigPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, logo, want string
	}{
		{"absolute", "/tmp/project-logo.svg", "/tmp/project-logo.svg"},
		{"home", "~/logos/project.svg", filepath.Join(home, "logos", "project.svg")},
		{"relative", "logos/project.svg", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir := writeConfig(t, `{"projects":{"shop":{"logo":"`+tc.logo+`"}}}`)
			want := tc.want
			if tc.name == "relative" {
				want = filepath.Join(dir, tc.logo)
			}
			got, err := Resolve("shop", []string{t.TempDir()})
			if err != nil || got != want {
				t.Fatalf("Resolve = %q, %v, want %q", got, err, want)
			}
		})
	}
}

func TestMissingAndMalformedConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if path, err := Resolve(t.Name()+"-missing", nil); path != "" || err != nil {
		t.Fatalf("missing config: path=%q err=%v", path, err)
	}
	writeConfig(t, `{"projects":`)
	if path, err := Resolve(t.Name()+"-malformed", nil); path != "" || err == nil {
		t.Fatalf("malformed config: path=%q err=%v", path, err)
	}
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "."}} {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestResolveTrackedIcon(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := gitRepo(t, map[string]string{"favicon.svg": "<svg/>"})
	// Deleted worktrees must not hide an icon in an older, surviving worktree.
	worktrees := []string{filepath.Join(t.TempDir(), "gone"), dir}
	path, err := Resolve(t.Name(), worktrees)
	if err != nil || path != filepath.Join(dir, "favicon.svg") {
		t.Fatalf("tracked favicon: path=%q err=%v", path, err)
	}
	writeConfig(t, `{"projects":{"`+t.Name()+`":{"logo":"custom.svg"}}}`)
	path, err = Resolve(t.Name(), worktrees)
	if err != nil || path != filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "vision", "custom.svg") {
		t.Fatalf("config must override cached discovery: path=%q err=%v", path, err)
	}
	writeConfig(t, `{"projects":{"`+t.Name()+`":{"logo":"updated.svg"}}}`)
	path, err = Resolve(t.Name(), worktrees)
	if err != nil || filepath.Base(path) != "updated.svg" {
		t.Fatalf("config edit must take effect immediately: path=%q err=%v", path, err)
	}
	writeConfig(t, `{"projects":`)
	path, err = Resolve(t.Name(), worktrees)
	if err == nil || path != filepath.Join(dir, "favicon.svg") {
		t.Fatalf("malformed config must preserve discovery: path=%q err=%v", path, err)
	}
}

func TestDiscoveryOrderAndSize(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newest := gitRepo(t, map[string]string{
		"favicon.svg": strings.Repeat("x", (1<<20)+1),
		"icon.svg":    "<svg/>", "node_modules/x/favicon.svg": "<svg/>",
	})
	older := gitRepo(t, map[string]string{"favicon.svg": "<svg/>"})
	path, err := Resolve(t.Name(), []string{newest, older})
	if err != nil || path != filepath.Join(newest, "icon.svg") {
		t.Fatalf("newest valid icon must win: path=%q err=%v", path, err)
	}
}

func TestDiscoveryCache(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := gitRepo(t, map[string]string{"icon.svg": "<svg/>"})
	first, err := Resolve(t.Name(), []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "favicon.svg"), []byte("<svg/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", "favicon.svg").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if path, err := Resolve(t.Name(), []string{dir}); err != nil || path != first {
		t.Fatalf("discovery must stay cached: path=%q err=%v", path, err)
	}
	discoveryCache.Lock()
	entry := discoveryCache.entries[t.Name()]
	entry.expires = time.Now().Add(-time.Second)
	discoveryCache.entries[t.Name()] = entry
	discoveryCache.Unlock()
	if path, err := Resolve(t.Name(), []string{dir}); err != nil || path != filepath.Join(dir, "favicon.svg") {
		t.Fatalf("expired cache must rediscover: path=%q err=%v", path, err)
	}
}

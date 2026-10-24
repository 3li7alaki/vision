package store

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestIdentifyOutsideGitRepository(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", parent)
	dir := filepath.Join(parent, "design-studio")
	other := filepath.Join(parent, "other")
	for _, d := range []string{dir, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	a, err := Identify(dir)
	if err != nil {
		t.Fatalf("a plain directory must still be a project: %v", err)
	}
	if a.Root != dir || a.Worktree != dir || a.Name != "design-studio" {
		t.Fatalf("root, worktree and name must come from the directory: %#v", a)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.ID) {
		t.Fatalf("id must be 64 hex characters like a git project's: %q", a.ID)
	}
	if a.Branch != "" || a.SHA != "" || a.Dirty {
		t.Fatalf("no git, so no branch, sha or dirty state: %#v", a)
	}

	b, err := Identify(dir)
	if err != nil || b.ID != a.ID {
		t.Fatalf("identity must be stable: %#v %v", b, err)
	}
	c, err := Identify(other)
	if err != nil || c.ID == a.ID {
		t.Fatalf("two directories must be two projects: %#v %v", c, err)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	d, err := Identify(link)
	if err != nil || d.ID != a.ID || d.Root != dir {
		t.Fatalf("a symlink to the directory is the same project: %#v %v", d, err)
	}
}

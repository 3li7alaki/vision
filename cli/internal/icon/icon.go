package icon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Discovery is cached for ten minutes so gallery refreshes do not repeatedly run Git.
// Config stays uncached because a hand-edited logo must take effect without a restart.
const discoveryTTL = 10 * time.Minute

var discoveryCache = struct {
	sync.Mutex
	entries map[string]cacheEntry
}{entries: make(map[string]cacheEntry)}

type cacheEntry struct {
	path    string
	expires time.Time
}

var basenameRank = map[string]int{
	"favicon.svg": 0, "icon.svg": 1, "favicon.png": 2,
	"apple-touch-icon.png": 3, "icon.png": 4, "logo.svg": 5,
	"logo.png": 6, "favicon.ico": 7,
}

var excludedSegments = map[string]bool{
	"node_modules": true, "vendor": true, "test": true, "tests": true,
	"__tests__": true, "fixtures": true, "testdata": true,
	".storybook": true, "dist": true,
}

// Resolve returns a discovered icon alongside any config error so callers can log the
// error while still showing the project's logo. Worktrees must be newest first.
func Resolve(projectName string, worktrees []string) (string, error) {
	configured, configErr := configuredLogo(projectName)
	if configured != "" {
		return configured, configErr
	}
	discoveryCache.Lock()
	entry, ok := discoveryCache.entries[projectName]
	discoveryCache.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.path, configErr
	}
	path, err := discover(worktrees)
	if err == nil {
		discoveryCache.Lock()
		discoveryCache.entries[projectName] = cacheEntry{path: path, expires: time.Now().Add(discoveryTTL)}
		discoveryCache.Unlock()
	}
	return path, errors.Join(configErr, err)
}

func configuredLogo(projectName string) (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	path := filepath.Join(dir, "vision", "projects.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config struct {
		Projects map[string]struct {
			Logo string `json:"logo"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	logo := config.Projects[projectName].Logo
	if logo == "" {
		return "", nil
	}
	if strings.HasPrefix(logo, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, logo[2:]), nil
	}
	if !filepath.IsAbs(logo) {
		logo = filepath.Join(filepath.Dir(path), logo)
	}
	return logo, nil
}

func rankedPaths(files []string) []string {
	var candidates []string
	for _, path := range files {
		if _, ok := basenameRank[filepath.Base(path)]; !ok {
			continue
		}
		excluded := false
		for _, segment := range strings.Split(path, "/") {
			if excludedSegments[segment] {
				excluded = true
				break
			}
		}
		if !excluded {
			candidates = append(candidates, path)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if basenameRank[filepath.Base(a)] != basenameRank[filepath.Base(b)] {
			return basenameRank[filepath.Base(a)] < basenameRank[filepath.Base(b)]
		}
		if strings.Count(a, "/") != strings.Count(b, "/") {
			return strings.Count(a, "/") < strings.Count(b, "/")
		}
		return a < b
	})
	return candidates
}

func discover(worktrees []string) (string, error) {
	var discoveryErr error
	for _, worktree := range worktrees {
		info, err := os.Stat(worktree)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
			continue
		}
		if err != nil {
			discoveryErr = errors.Join(discoveryErr, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// NUL delimiters preserve tracked filenames containing whitespace or newlines.
		b, err := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "-z").Output()
		cancel()
		if err != nil {
			discoveryErr = errors.Join(discoveryErr, fmt.Errorf("icon discovery in %s: %w", worktree, err))
			continue
		}
		for _, candidate := range rankedPaths(strings.Split(string(b), "\x00")) {
			path := filepath.Join(worktree, candidate)
			info, err := os.Stat(path)
			if err == nil && info.Mode().IsRegular() && info.Size() <= 1<<20 {
				return path, nil
			}
		}
	}
	return "", discoveryErr
}

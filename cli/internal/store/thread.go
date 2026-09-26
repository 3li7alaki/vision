package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ThreadID preserves the identity recorded at capture time. Looking up old sessions here
// would move immutable snaps between threads whenever an agent joined a different one.
// Before threads existed, the session itself was the identity, so old ledgers need no rewrite.
func (s Snap) ThreadID() string {
	if s.Thread != "" {
		return s.Thread
	}
	return s.Session
}

func identityFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:16] + ".json"
}

func validateThread(thread string) error {
	if thread == "" || len(thread) > 200 || !utf8.ValidString(thread) || strings.ContainsAny(thread, "\u2028\u2029") || strings.ContainsFunc(thread, unicode.IsControl) {
		return errors.New("thread must be non-empty, at most 200 bytes, and contain no control characters or newlines")
	}
	return nil
}

// ThreadFor reads exactly one mapping. Missing mappings preserve the old session identity;
// unreadable or corrupt mappings must fail rather than silently send a reader to a fresh
// cursor and hide the predecessor's verdicts. This is deliberately not a chain traversal.
func ThreadFor(project, session string) (string, error) {
	if session == "" {
		return "", nil
	}
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(dir, "sessions", identityFile(session)))
	if errors.Is(err, os.ErrNotExist) {
		return session, nil
	}
	if err != nil {
		return "", err
	}
	var mapping struct {
		Thread string `json:"thread"`
	}
	if err := json.Unmarshal(b, &mapping); err != nil {
		return "", fmt.Errorf("session thread: %w", err)
	}
	if err := validateThread(mapping.Thread); err != nil {
		return "", err
	}
	return mapping.Thread, nil
}

// ActiveThreadFor resolves a notes or status reader and keeps its mapping alive. ThreadFor
// stays read-only so merely inspecting identities cannot extend their retention; failed
// touches are harmless because the ledger and open flags survive expired session state.
func ActiveThreadFor(project, session string) (string, error) {
	thread, err := ThreadFor(project, session)
	if err == nil && session != "" {
		dir, dirErr := ProjectDir(project)
		if dirErr == nil {
			now := time.Now()
			_ = os.Chtimes(filepath.Join(dir, "sessions", identityFile(session)), now, now)
		}
	}
	return thread, err
}

// JoinThread stores the thread carried in the handoff literally, so a later handoff never
// depends on a predecessor's mapping or confuses a thread with a similarly named session.
// Temp file plus rename keeps a concurrent snap from reading a half-written mapping.
func JoinThread(project, session, thread string) (string, error) {
	if session == "" {
		return "", errors.New("session must not be empty")
	}
	if err := validateThread(thread); err != nil {
		return "", err
	}
	current, err := ThreadFor(project, session)
	if err != nil {
		return "", err
	}
	if current == thread {
		return current, nil
	}
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".thread-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	err = json.NewEncoder(tmp).Encode(struct {
		Thread string `json:"thread"`
	}{thread})
	if err == nil {
		err = tmp.Sync()
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, identityFile(session))); err != nil {
		return "", err
	}
	return thread, nil
}

// OpenFlags is processing state derived from the ledger, never from a read cursor. A
// cursor delivers a flag once, but compaction or a timed-out agent can lose that context.
// Like Kafka committing after processing rather than after reading, a flag is done when
// the UI is fixed and re-snapped, not when it was printed. Only another snap of that same
// key and variant on this thread closes it; the human then judges the new shot as usual.
func OpenFlags(project, thread string) ([]Note, error) {
	snaps, err := Snaps(project)
	if err != nil {
		return nil, err
	}
	notes, err := Notes(project)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]bool)
	latestSnap := make(map[[2]string]time.Time)
	for _, snap := range snaps {
		if snap.ThreadID() != thread {
			continue
		}
		digests[snap.Digest] = true
		pair := [2]string{snap.Key, snap.Variant}
		if ts, ok := latestSnap[pair]; !ok || snap.TS.After(ts) {
			latestSnap[pair] = snap.TS
		}
	}
	latestFlag := make(map[[2]string]int)
	for i, note := range notes {
		pair := [2]string{note.Key, note.Variant}
		if note.Verdict != "flag" || !digests[note.Digest] {
			continue
		}
		if ts, ok := latestSnap[pair]; ok && ts.After(note.TS) {
			continue
		}
		if prev, ok := latestFlag[pair]; !ok || !note.TS.Before(notes[prev].TS) {
			latestFlag[pair] = i
		}
	}
	// Preserve ledger order rather than map iteration order so repeated reads are stable.
	var out []Note
	for i, note := range notes {
		if latest, ok := latestFlag[[2]string{note.Key, note.Variant}]; ok && latest == i {
			out = append(out, note)
		}
	}
	return out, nil
}

// SessionStateMaxAge matches Claude Code's default cleanupPeriodDays. After 30 days a
// session can no longer be resumed, so its mapping is dead weight. Only disposable session
// state expires: the ledgers and open flags remain, and a missing thread cursor starts at 0.
const SessionStateMaxAge = 30 * 24 * time.Hour

// SweepSessionState visits only the two per-session state directories. An allowlist keeps
// the permanent ledgers, project-wide cursor, shots and baselines outside the sweep even
// when they are old. Keep sweeping other projects on errors so one broken path cannot
// prevent cleanup machine-wide; the daemon logs the combined error without stopping.
func SweepSessionState(now time.Time, maxAge time.Duration) (removed int, err error) {
	projects, err := ProjectIDs()
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-maxAge)
	for _, project := range projects {
		dir, dirErr := ProjectDir(project)
		if dirErr != nil {
			err = errors.Join(err, dirErr)
			continue
		}
		for _, name := range []string{"sessions", "cursors"} {
			path := filepath.Join(dir, name)
			entries, readErr := os.ReadDir(path)
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			if readErr != nil {
				err = errors.Join(err, readErr)
				continue
			}
			for _, entry := range entries {
				info, statErr := entry.Info()
				if statErr != nil {
					err = errors.Join(err, statErr)
					continue
				}
				if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
					continue
				}
				if removeErr := os.Remove(filepath.Join(path, entry.Name())); removeErr != nil {
					if !errors.Is(removeErr, os.ErrNotExist) {
						err = errors.Join(err, removeErr)
					}
				} else {
					removed++
				}
			}
		}
	}
	return removed, err
}

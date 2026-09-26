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

// JoinThread accepts a predecessor's raw session id as well as its thread. Resolve one
// hop before storing so a later handoff never depends on every predecessor's mapping.
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
	thread, err = ThreadFor(project, thread)
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

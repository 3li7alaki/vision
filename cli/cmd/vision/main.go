package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"vision/internal/base"
	"vision/internal/capture"
	"vision/internal/server"
	"vision/internal/store"
	"vision/internal/version"
)

const endpoint = "http://127.0.0.1:4747"
const galleryURL = "http://vision.test:4747"

func viewURL(project, thread string) string {
	if project == "" {
		return galleryURL
	}
	view := galleryURL + "/p/" + url.PathEscape(project)
	if thread != "" {
		view += "/t/" + url.PathEscape(thread)
	}
	return view
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vision:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch base.Command(args) {
	case "help":
		fmt.Println(usageText)
		return nil
	case "version":
		fmt.Println(version.Version)
		return nil
	}
	switch args[0] {
	case "snap":
		return snap(args[1:])
	case "add":
		return add(args[1:])
	case "discard":
		return discard(args[1:])
	case "session":
		return session(args[1:])
	case "notes":
		return notes(args[1:])
	case "on":
		return on(args[1:])
	case "off":
		return off(args[1:])
	case "status":
		return status(args[1:])
	case "_serve":
		return server.New().ListenAndServe()
	default:
		return usage()
	}
}

const usageText = "usage: vision snap <key> [--as <variant> | --dim k=v...] [--selector <css>] [--meta k=v...] [--note <text>] [--json] | vision add <key> <file> [--as <variant>] [--meta k=v...] [--note <text>] [--json] | vision discard <key> [--as <variant>] [--json] | vision notes [--unread | --since <duration> | --open (requires --session) | --wait <duration>] [--flagged] [--session <id>] [--json] | vision on | off | vision status [--session <id>] [--json] | vision session join <thread> | vision session thread [--json] | vision --version"

func usage() error {
	return errors.New(usageText)
}

// The harness already gives every shell a stable session identity. Requiring a separate
// export left ordinary agent captures anonymous, so use the harness values as fallbacks
// while keeping VISION_SESSION_ID as the explicit override for other callers.
func sessionID() string {
	for _, name := range []string{"VISION_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		if id := os.Getenv(name); id != "" {
			return id
		}
	}
	return ""
}

func sessionFlag(fs *flag.FlagSet) *string {
	var session string
	fs.Func("session", "agent session id", func(value string) error {
		if value == "" {
			return errors.New("session must not be empty")
		}
		session = value
		return nil
	})
	return &session
}

func session(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet("session "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "JSON output")
	switch args[0] {
	case "join":
		if len(args) != 2 {
			return usage()
		}
	case "thread":
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return usage()
		}
	default:
		return usage()
	}
	id := sessionID()
	if id == "" {
		return errors.New("no session id; set VISION_SESSION_ID, CLAUDE_CODE_SESSION_ID, or CODEX_THREAD_ID")
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	if args[0] == "join" {
		thread, err := store.JoinThread(project.ID, id, args[1])
		if err != nil {
			return err
		}
		fmt.Println("joined", thread)
		return nil
	}
	thread, err := store.ThreadFor(project.ID, id)
	if err != nil {
		return err
	}
	if *asJSON {
		snaps, err := store.Snaps(project.ID)
		if err != nil {
			return err
		}
		count := 0
		for _, snap := range snaps {
			if snap.ThreadID() == thread {
				count++
			}
		}
		return printJSON(map[string]any{"schemaVersion": store.SchemaVersion, "session": id, "thread": thread, "snaps": count})
	}
	fmt.Println(thread)
	return nil
}

// unknownDimension is what a derived dimension holds when the browser could not tell us its
// value. It is a real value rather than a blank, so it sorts, encodes, and names a baseline
// like any other, and can never be read as "this shot had no such dimension".
const unknownDimension = "unknown"

// deriveDims fills in the dimensions the tool can measure for itself, which is the point of
// the whole feature: the reviewer stops typing values the capture already recorded, so a
// light page can no longer be filed under scheme=dark by a slip of the hand. An explicit
// --dim always wins, because the human overriding a measurement is a deliberate act.
//
// The browser does not always answer. capture.scheme returns "" when its eval fails, and a
// capture response missing its viewport leaves Width at 0. Neither gap may be dropped: a
// shot with no scheme dimension shares a baseline with shots of both schemes, which is the
// exact collision this encoding exists to prevent. Neither may be guessed either, since
// ViewportClass(0) would confidently call a desktop shot "mobile". Unknown is recorded
// instead, so the gap is visible in the gallery and still cannot collide with a real value.
func deriveDims(dims map[string]string, conditions store.Conditions) {
	if _, ok := dims["scheme"]; !ok {
		dims["scheme"] = unknownDimension
		if conditions.Scheme != "" {
			dims["scheme"] = conditions.Scheme
		}
	}
	if _, ok := dims["vp"]; !ok {
		dims["vp"] = unknownDimension
		if conditions.Width > 0 {
			dims["vp"] = store.ViewportClass(conditions.Width)
		}
	}
}

func validateSelector(selector string) error {
	if selector == "" || len(selector) > 500 {
		return errors.New("selector must be non-empty and at most 500 bytes")
	}
	for _, r := range selector {
		if unicode.IsControl(r) {
			return errors.New("selector must not contain control characters")
		}
	}
	return nil
}

func viewportClaim(variant string, dims map[string]string) string {
	if len(dims) != 0 {
		return dims["vp"]
	}
	claim := ""
	for _, token := range strings.FieldsFunc(strings.ToLower(variant), func(r rune) bool {
		return strings.ContainsRune("-_,=@.", r)
	}) {
		switch token {
		case "mobile", "tablet", "desktop":
			// A label naming multiple classes cannot justify rejecting one viewport class.
			if claim != "" && claim != token {
				return ""
			}
			claim = token
		}
	}
	return claim
}

func checkViewportClaim(claim string, conditions store.Conditions) error {
	if claim == "" || conditions.Width <= 0 {
		return nil
	}
	actual := store.ViewportClass(conditions.Width)
	if claim == actual {
		return nil
	}
	size := map[string]string{"mobile": "390 844", "tablet": "820 1180", "desktop": "1440 900"}[claim]
	hint := "set the viewport first"
	if size != "" {
		hint += " (pinchtab set viewport " + size + ")"
	}
	return fmt.Errorf("variant says %s but the viewport is %dx%d (%s); %s, then snap again", claim, conditions.Width, conditions.Height, actual, hint)
}

type keyValueFlag map[string]string

func (values keyValueFlag) String() string { return "" }

func (values keyValueFlag) Set(value string) error {
	key, item, ok := strings.Cut(value, "=")
	if !ok || key == "" || item == "" {
		return errors.New("want k=v with a non-empty key and value")
	}
	values[key] = item
	return nil
}

func snap(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	key := args[0]
	if err := store.ParseKey(key); err != nil {
		return err
	}
	fs := flag.NewFlagSet("snap", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dims, meta := keyValueFlag{}, keyValueFlag{}
	variant, note, asJSON := fs.String("as", "default", "variant"), fs.String("note", "", "note"), fs.Bool("json", false, "JSON output")
	selector := fs.String("selector", "", "CSS selector to capture")
	fs.Var(dims, "dim", "visual dimension k=v")
	fs.Var(meta, "meta", "provenance k=v")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	asSet, selectorSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "as" {
			asSet = true
		}
		if f.Name == "selector" {
			selectorSet = true
		}
	})
	if selectorSet {
		if err := validateSelector(*selector); err != nil {
			return err
		}
	}
	if asSet && len(dims) != 0 {
		return errors.New("--as and --dim are mutually exclusive")
	}
	if len(dims) != 0 {
		if _, err := store.EncodeVariant(dims); err != nil {
			return err
		}
	} else if err := store.ParseVariant(*variant); err != nil {
		return err
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	id := sessionID()
	thread, err := store.ThreadFor(project.ID, id)
	if err != nil {
		return err
	}
	if err := healthy(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shot, err := capture.Take(ctx, *selector)
	if err != nil {
		return err
	}
	// Check before deriving dimensions because measured values are not caller claims.
	if err := checkViewportClaim(viewportClaim(*variant, dims), shot.Conditions); err != nil {
		return err
	}
	if len(dims) != 0 {
		deriveDims(dims, shot.Conditions)
		*variant, err = store.EncodeVariant(dims)
		if err != nil {
			return err
		}
	}
	req := server.SnapRequest{Project: project, Key: key, Variant: *variant, Dims: dims, Meta: meta, Note: *note, Session: id, Thread: thread, PNG: base64.StdEncoding.EncodeToString(shot.PNG), Capture: shot.Conditions, Elements: shot.Elements}
	return submitSnap(req, *asJSON)
}

// Base64 of 15 MB leaves room for JSON under the daemon's 20 MB request cap.
const maxVideoBytes = 15_000_000

func readVideo(file string) ([]byte, string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, "", err
	}
	tooLarge := errors.New("file over the 15 MB limit; record shorter or with --scale 0.5")
	if info.Size() > maxVideoBytes {
		return nil, "", tooLarge
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	// Bound the read because the recorder could still be writing after the size check.
	data, err := io.ReadAll(io.LimitReader(f, maxVideoBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxVideoBytes {
		return nil, "", tooLarge
	}
	media := ""
	switch {
	case len(data) >= 8 && string(data[4:8]) == "ftyp":
		media = "video/mp4"
	case bytes.HasPrefix(data, []byte("\x1a\x45\xdf\xa3")):
		media = "video/webm"
	default:
		// Screenshots must come from vision's capture to record conditions for the conditions guard.
		return nil, "", errors.New("vision add files recorded video (.mp4 or .webm); use vision snap for a screenshot")
	}
	ext, err := store.MediaExtension(media)
	if err != nil {
		return nil, "", err
	}
	if actual := strings.ToLower(filepath.Ext(file)); actual != ext {
		return nil, "", fmt.Errorf("file extension %q disagrees with %s bytes (want %s); check that you filed the right recording", actual, media, ext)
	}
	return data, media, nil
}

func add(args []string) error {
	if len(args) < 2 {
		return usage()
	}
	key, file := args[0], args[1]
	if err := store.ParseKey(key); err != nil {
		return err
	}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	variant, note, asJSON := fs.String("as", "default", "variant"), fs.String("note", "", "note"), fs.Bool("json", false, "JSON output")
	meta := keyValueFlag{}
	fs.Var(meta, "meta", "provenance k=v")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	if err := store.ParseVariant(*variant); err != nil {
		return err
	}
	data, media, err := readVideo(file)
	if err != nil {
		return err
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	id := sessionID()
	thread, err := store.ThreadFor(project.ID, id)
	if err != nil {
		return err
	}
	if err := healthy(); err != nil {
		return err
	}
	req := server.SnapRequest{Project: project, Key: key, Variant: *variant, Meta: meta, Note: *note, Session: id, Thread: thread, Media: media, PNG: base64.StdEncoding.EncodeToString(data), Capture: store.Conditions{}}
	return submitSnap(req, *asJSON)
}

func submitSnap(req server.SnapRequest, asJSON bool) error {
	var result map[string]any
	if err := post("/api/snap", req, &result); err != nil {
		return err
	}
	result["url"] = viewURL(req.Project.Name, req.Thread)
	if asJSON {
		return printJSON(result)
	}
	if warning, _ := result["warning"].(string); warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	queued, _ := result["queued"].(bool)
	if queued {
		fmt.Printf("queued %s@%s\n", req.Key, req.Variant)
		fmt.Println(viewURL(req.Project.Name, req.Thread))
	} else {
		fmt.Printf("archived unchanged %s@%s\n", req.Key, req.Variant)
	}
	return nil
}

func discard(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	key := args[0]
	if err := store.ParseKey(key); err != nil {
		return err
	}
	fs := flag.NewFlagSet("discard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	variant := fs.String("as", "default", "variant")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	if err := store.ParseVariant(*variant); err != nil {
		return err
	}
	id := sessionID()
	if id == "" {
		return errors.New("no session id; set VISION_SESSION_ID, CLAUDE_CODE_SESSION_ID, or CODEX_THREAD_ID")
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	thread, err := store.ThreadFor(project.ID, id)
	if err != nil {
		return err
	}
	if err := healthy(); err != nil {
		return err
	}
	var result struct {
		SchemaVersion int      `json:"schemaVersion"`
		Withdrawn     []string `json:"withdrawn"`
	}
	if err := post("/api/withdraw", server.WithdrawRequest{Project: project.ID, Key: key, Variant: *variant, Session: id, Thread: thread}, &result); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(result)
	}
	fmt.Printf("withdrew %s@%s (%d shot(s))\n", key, *variant, len(result.Withdrawn))
	return nil
}

func notes(args []string) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unread, since, flagged, asJSON := fs.Bool("unread", false, "unread notes"), fs.Duration("since", 0, "notes since duration"), fs.Bool("flagged", false, "flagged notes"), fs.Bool("json", false, "JSON output")
	open := fs.Bool("open", false, "open flags on the session's thread")
	wait := fs.Duration("wait", 0, "wait for unread verdicts on the session's thread")
	session := sessionFlag(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*unread && *since != 0) {
		return usage()
	}
	// Even --since=0 names a conflicting mode; checking only its duration loses that.
	invalid := *open && *session == ""
	waitSet, otherModeSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wait" {
			waitSet = true
		}
		if f.Name == "unread" || f.Name == "since" || f.Name == "open" {
			otherModeSet = true
		}
		if *open && (f.Name == "since" || f.Name == "unread") {
			invalid = true
		}
	})
	if waitSet {
		if *session == "" {
			*session = sessionID()
		}
		// A backgrounded wait that is forgotten must still end.
		invalid = invalid || otherModeSet || *session == "" || *wait <= 0 || *wait > 24*time.Hour
	}
	if invalid {
		return usage()
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	var values []store.Note
	if waitSet {
		values, err = waitNotes(project.ID, *session, *wait)
	} else if *open {
		thread, resolveErr := store.ActiveThreadFor(project.ID, *session)
		if resolveErr != nil {
			return resolveErr
		}
		values, err = store.OpenFlags(project.ID, thread)
	} else if *unread && *session != "" {
		values, err = store.UnreadNotesForSession(project.ID, *session)
	} else if *unread {
		values, err = store.UnreadNotes(project.ID)
	} else if *session != "" {
		values, err = store.NotesForSession(project.ID, *session)
	} else {
		values, err = store.Notes(project.ID)
	}
	if err != nil {
		return err
	}
	if *since != 0 {
		cutoff := time.Now().Add(-*since)
		filtered := values[:0]
		for _, n := range values {
			if !n.TS.Before(cutoff) {
				filtered = append(filtered, n)
			}
		}
		values = filtered
	}
	// Filtering happens after the read, never instead of it. With --unread the cursor has
	// already advanced past every note fetched above, so "--unread --flagged" marks the ok
	// verdicts read as well and simply does not print them. That is the honest behaviour for
	// a cursor that means "seen", but it does mean this pair is a one-shot view, not a query
	// you can run twice.
	if *flagged {
		filtered := values[:0]
		for _, n := range values {
			if n.Verdict == "flag" {
				filtered = append(filtered, n)
			}
		}
		values = filtered
	}
	if *asJSON {
		return printJSON(map[string]any{"schemaVersion": 1, "notes": values})
	}
	for _, n := range values {
		fmt.Printf("%s %s@%s: %s", n.Verdict, n.Key, n.Variant, n.Digest)
		if n.Note != "" {
			fmt.Printf(" %s", strings.ReplaceAll(n.Note, "\n", "\n    "))
		}
		fmt.Println()
	}
	return nil
}

func waitNotes(project, session string, duration time.Duration) ([]store.Note, error) {
	dir, err := store.ProjectDir(project)
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	lastSize := int64(-1)
	for {
		size := int64(0)
		info, err := os.Stat(filepath.Join(dir, "notes.jsonl"))
		if err == nil {
			size = info.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if size != lastSize {
			// Wait, --unread --session and the dotfiles hook deliberately share one cursor.
			// Whichever reads first delivers the verdicts, so later reads do not repeat them.
			values, err := store.UnreadNotesForSession(project, session)
			if err != nil || len(values) > 0 {
				return values, err
			}
			lastSize = size
		}
		select {
		case <-deadline.C:
			return nil, fmt.Errorf("no new verdicts after %s", duration)
		case <-poll.C:
		}
	}
}

func post(path string, input, output any) error {
	b, err := json.Marshal(input)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(endpoint+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("daemon unavailable, run `vision on`: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(output)
}

func on(args []string) error {
	asJSON, err := jsonOnly(args)
	if err != nil {
		return usage()
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	warning, err := installService(exe)
	if err != nil {
		return err
	}
	// Five seconds is a guess above this box's sub-second start; adjust it if measured startup needs more headroom.
	if err := waitHealthy(endpoint+"/health", 5*time.Second); err != nil {
		log := "~/Library/Logs/vision.log"
		if runtime.GOOS == "linux" {
			log = "journalctl --user -u vision.service"
		}
		return fmt.Errorf("%w; check %s", err, log)
	}
	if asJSON {
		out := map[string]any{"schemaVersion": 1, "running": true, "url": galleryURL}
		if warning != "" {
			out["warning"] = warning
		}
		return printJSON(out)
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, "vision:", warning)
	}
	fmt.Println("vision on at", galleryURL)
	return nil
}

func off(args []string) error {
	asJSON, err := jsonOnly(args)
	if err != nil {
		return usage()
	}
	if err := removeService(); err != nil {
		return err
	}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	if resp, err := client.Get(endpoint + "/health"); err == nil {
		resp.Body.Close()
		fmt.Fprintln(os.Stderr, "vision: something still answers on 127.0.0.1:4747 (a hand-started vision _serve?); find it with: lsof -iTCP:4747 -sTCP:LISTEN")
	}
	if asJSON {
		return printJSON(map[string]any{"schemaVersion": 1, "running": false})
	}
	fmt.Println("vision off")
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "JSON output")
	session := sessionFlag(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage()
	}
	openFlags := 0
	view := galleryURL
	project, projectErr := store.Identify(".")
	if projectErr == nil {
		view = viewURL(project.Name, "")
	}
	if *session != "" {
		if projectErr != nil {
			return projectErr
		}
		thread, err := store.ActiveThreadFor(project.ID, *session)
		if err != nil {
			return err
		}
		view = viewURL(project.Name, thread)
		flags, err := store.OpenFlags(project.ID, thread)
		if err != nil {
			return err
		}
		openFlags = len(flags)
	}
	// Short timeout because a status line polls this on a timer: a wedged daemon must cost
	// the caller a blink, not a second.
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(endpoint + "/health")
	if err != nil {
		if *asJSON {
			out := map[string]any{"schemaVersion": 1, "running": false, "pending": 0, "url": view}
			if *session != "" {
				out["session"] = *session
				out["openFlags"] = openFlags
			}
			_ = printJSON(out)
			return errors.New("daemon unavailable, run `vision on`")
		}
		fmt.Println("vision off at", view)
		return errors.New("daemon unavailable, run `vision on`")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon unhealthy: %s", resp.Status)
	}
	var health struct {
		Pending int `json:"pending"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&health)
	// A review waiting in another repo is not this repo's work. Inside a project, `pending`
	// is that project's count and the daemon-wide figure moves to `pendingAll`; outside one
	// there is nothing to scope to, so `pending` stays the daemon-wide total.
	pending, scope := health.Pending, ""
	if projectErr == nil {
		if n, err := server.PendingCountForSession(project.ID, *session); err == nil {
			pending, scope = n, project.Name
		} else if *session != "" {
			return err
		}
	}
	if *asJSON {
		out := map[string]any{"schemaVersion": 1, "running": true, "pending": pending, "pendingAll": health.Pending, "url": view}
		if *session != "" {
			out["session"] = *session
			out["openFlags"] = openFlags
		}
		if scope != "" {
			out["project"] = scope
		}
		return printJSON(out)
	}
	switch {
	case pending > 0 && scope != "":
		fmt.Printf("vision on at %s, %d pending in %s\n", view, pending, scope)
	case pending > 0:
		fmt.Printf("vision on at %s, %d pending\n", view, pending)
	default:
		fmt.Println("vision on at", view)
	}
	return nil
}

func waitHealthy(healthURL string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 300 * time.Millisecond}
	for ctx.Err() == nil {
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not become healthy within %s", timeout)
}

func healthy() error {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(endpoint + "/health")
	if err != nil {
		return fmt.Errorf("daemon unavailable, run `vision on`: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon unhealthy, run `vision on`: %s", resp.Status)
	}
	return nil
}

func jsonOnly(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if len(args) == 1 && args[0] == "--json" {
		return true, nil
	}
	return false, usage()
}

func printJSON(v any) error {
	b, err := json.Marshal(v)
	if err == nil {
		fmt.Println(string(b))
	}
	return err
}

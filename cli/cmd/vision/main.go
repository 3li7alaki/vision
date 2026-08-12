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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"vision/internal/base"
	"vision/internal/capture"
	"vision/internal/server"
	"vision/internal/store"
	"vision/internal/version"
)

const endpoint = "http://127.0.0.1:4747"

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

const usageText = "usage: vision snap <key> [--as <variant>] [--note <text>] [--json] | vision notes [--unread | --since <duration>] [--json] | vision on | off | status | vision --version"

func usage() error {
	return errors.New(usageText)
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
	variant, note, asJSON := fs.String("as", "default", "variant"), fs.String("note", "", "note"), fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	if err := store.ParseVariant(*variant); err != nil {
		return err
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	if err := healthy(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shot, err := capture.Take(ctx)
	if err != nil {
		return err
	}
	req := server.SnapRequest{Project: project, Key: key, Variant: *variant, Note: *note, Session: os.Getenv("VISION_SESSION_ID"), PNG: base64.StdEncoding.EncodeToString(shot.PNG), Capture: shot.Conditions}
	var result map[string]any
	if err := post("/api/snap", req, &result); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(result)
	}
	if warning, _ := result["warning"].(string); warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	queued, _ := result["queued"].(bool)
	if queued {
		fmt.Printf("queued %s@%s\n", key, *variant)
	} else {
		fmt.Printf("archived unchanged %s@%s\n", key, *variant)
	}
	return nil
}

func notes(args []string) error {
	fs := flag.NewFlagSet("notes", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	unread, since, asJSON := fs.Bool("unread", false, "unread notes"), fs.Duration("since", 0, "notes since duration"), fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*unread && *since != 0) {
		return usage()
	}
	project, err := store.Identify(".")
	if err != nil {
		return err
	}
	var values []store.Note
	if *unread {
		values, err = store.UnreadNotes(project.ID)
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
	if *asJSON {
		return printJSON(map[string]any{"schemaVersion": 1, "notes": values})
	}
	for _, n := range values {
		fmt.Printf("%s %s@%s: %s", n.Verdict, n.Key, n.Variant, n.Digest)
		if n.Note != "" {
			fmt.Printf(" %s", n.Note)
		}
		fmt.Println()
	}
	return nil
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
	if runtime.GOOS != "darwin" {
		return errors.New("vision on requires macOS launchd")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", "dev.vision.plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.vision</string>
<key>ProgramArguments</key><array><string>%s</string><string>_serve</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, xmlEscape(exe), xmlEscape(filepath.Join(home, "Library", "Logs", "vision.log")), xmlEscape(filepath.Join(home, "Library", "Logs", "vision.log")))
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/dev.vision").Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if asJSON {
		return printJSON(map[string]any{"schemaVersion": 1, "running": true, "url": "http://vision.test:4747"})
	}
	fmt.Println("vision on at http://vision.test:4747")
	return nil
}

func off(args []string) error {
	asJSON, err := jsonOnly(args)
	if err != nil {
		return usage()
	}
	if runtime.GOOS != "darwin" {
		return errors.New("vision off requires macOS launchd")
	}
	target := fmt.Sprintf("gui/%d/dev.vision", os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if asJSON {
		return printJSON(map[string]any{"schemaVersion": 1, "running": false})
	}
	fmt.Println("vision off")
	return nil
}

func status(args []string) error {
	asJSON, err := jsonOnly(args)
	if err != nil {
		return usage()
	}
	// Short timeout because a status line polls this on a timer: a wedged daemon must cost
	// the caller a blink, not a second.
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(endpoint + "/health")
	if err != nil {
		if asJSON {
			_ = printJSON(map[string]any{"schemaVersion": 1, "running": false, "pending": 0})
			return errors.New("daemon unavailable, run `vision on`")
		}
		fmt.Println("vision off")
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
	if project, err := store.Identify("."); err == nil {
		if n, err := server.PendingCountFor(project.ID); err == nil {
			pending, scope = n, project.Name
		}
	}
	if asJSON {
		out := map[string]any{"schemaVersion": 1, "running": true, "pending": pending, "pendingAll": health.Pending, "url": "http://vision.test:4747"}
		if scope != "" {
			out["project"] = scope
		}
		return printJSON(out)
	}
	switch {
	case pending > 0 && scope != "":
		fmt.Printf("vision on at http://vision.test:4747, %d pending in %s\n", pending, scope)
	case pending > 0:
		fmt.Printf("vision on at http://vision.test:4747, %d pending\n", pending)
	default:
		fmt.Println("vision on at http://vision.test:4747")
	}
	return nil
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
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

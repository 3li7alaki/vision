package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// installService supervises the daemon with launchd. The returned string is a
// warning to show the caller when the daemon started but will not outlive the
// session; launchd has no equivalent of systemd's lingering, so it is always "".
func installService(exe string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", "dev.vision.plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	log := filepath.Join(home, "Library", "Logs", "vision.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.vision</string>
<key>ProgramArguments</key><array><string>%s</string><string>_serve</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, xmlEscape(exe), xmlEscape(log), xmlEscape(log))
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return "", err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/dev.vision"
	launchctl("bootout", target)
	if err := waitUnloaded(target); err != nil {
		return "", err
	}
	// A bootstrap can still land on a job a recent bootout is tearing down: it succeeds, the
	// job vanishes, and kickstart reports 113 (seen once, `vision off` then `vision on`,
	// 2026-10-31). The job is gone by then, so one more bootstrap is all it takes.
	for attempt := 1; ; attempt++ {
		if out, err := launchctl("bootstrap", domain, path); err != nil {
			return "", fmt.Errorf("launchctl bootstrap: %w: %s", err, out)
		}
		// RunAtLoad was seen not to spawn after a bootout/bootstrap pair, so explicitly start the job.
		out, err := launchctl("kickstart", "-k", target)
		if err == nil {
			return "", nil
		}
		if launchctlCode(err) != serviceNotFound || attempt == 2 {
			return "", fmt.Errorf("launchctl kickstart: %w: %s", err, out)
		}
	}
}

func removeService() error {
	target := fmt.Sprintf("gui/%d/dev.vision", os.Getuid())
	if out, err := launchctl("bootout", target); err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, out)
	}
	// Bootout returns before the job is gone; an `on` right after would race its teardown.
	return waitUnloaded(target)
}

// launchctl is a variable so tests can play launchd's replies.
var launchctl = func(args ...string) (string, error) {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// serviceNotFound is launchctl's exit status for "Could not find service".
const serviceNotFound = 113

func launchctlCode(err error) int {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// waitUnloaded polls until launchd no longer knows target. Three seconds is well above the
// sub-second unloads seen on this box.
func waitUnloaded(target string) error {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := launchctl("print", target); err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launchctl bootout: timed out waiting for %s to unload", target)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

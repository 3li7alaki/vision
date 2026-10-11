package main

import (
	"context"
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
	exec.Command("launchctl", "bootout", target).Run()
	// Bootout is asynchronous, so wait for the old job to disappear before bootstrapping its replacement.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for exec.CommandContext(ctx, "launchctl", "print", target).Run() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("launchctl bootout: timed out waiting for %s to unload: %w", target, err)
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// RunAtLoad was seen not to spawn after a bootout/bootstrap pair, so explicitly start the job.
	if out, err := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return "", nil
}

func removeService() error {
	target := fmt.Sprintf("gui/%d/dev.vision", os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", target).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

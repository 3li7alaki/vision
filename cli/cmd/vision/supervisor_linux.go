package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const unitName = "vision.service"

// installService supervises the daemon with a systemd user unit. Restart=always
// mirrors launchd's KeepAlive, and default.target mirrors RunAtLoad.
func installService(exe string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, unitName), []byte(renderUnit(exe)), 0o644); err != nil {
		return "", err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return "", fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		return "", fmt.Errorf("systemctl enable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return lingerWarning(), nil
}

func removeService() error {
	if out, err := exec.Command("systemctl", "--user", "disable", "--now", unitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl disable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// lingerWarning refuses to report a durable daemon that is not one. Without
// lingering a --user unit dies with the last session, so a daemon started over
// SSH would vanish at logout having reported success.
func lingerWarning() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	out, err := exec.Command("loginctl", "show-user", name, "-p", "Linger").Output()
	if err != nil {
		// No loginctl, or no user record: cannot tell, so do not claim either way.
		return ""
	}
	if strings.TrimSpace(string(out)) == "Linger=yes" {
		return ""
	}
	return fmt.Sprintf("lingering is off, so the daemon stops when your session ends. Enable it with: sudo loginctl enable-linger %s", name)
}

//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"
)

func installService(string) (string, error) {
	return "", fmt.Errorf("vision on needs launchd or systemd, and %s has neither wired up", runtime.GOOS)
}

func removeService() error {
	return fmt.Errorf("vision off needs launchd or systemd, and %s has neither wired up", runtime.GOOS)
}

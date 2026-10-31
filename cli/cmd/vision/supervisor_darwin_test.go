package main

import (
	"strings"
	"testing"
)

type exitCode int

func (e exitCode) Error() string { return "exit status" }
func (e exitCode) ExitCode() int { return int(e) }

// fakeLaunchctl answers each verb from its queue (empty queue: success) and records calls.
func fakeLaunchctl(t *testing.T, replies map[string][]error) *[]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	calls := &[]string{}
	saved := launchctl
	t.Cleanup(func() { launchctl = saved })
	launchctl = func(args ...string) (string, error) {
		*calls = append(*calls, args[0])
		queue := replies[args[0]]
		if len(queue) == 0 {
			return "", nil
		}
		replies[args[0]] = queue[1:]
		return "", queue[0]
	}
	return calls
}

func TestInstallRebootstrapsWhenKickstartFindsNoService(t *testing.T) {
	calls := fakeLaunchctl(t, map[string][]error{"print": {exitCode(113)}, "kickstart": {exitCode(serviceNotFound)}})
	if _, err := installService("/bin/vision"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, " "); got != "bootout print bootstrap kickstart bootstrap kickstart" {
		t.Fatalf("calls: %s", got)
	}
}

func TestInstallGivesUpAfterOneRetry(t *testing.T) {
	fakeLaunchctl(t, map[string][]error{"print": {exitCode(113)}, "kickstart": {exitCode(113), exitCode(113)}})
	if _, err := installService("/bin/vision"); err == nil || !strings.Contains(err.Error(), "kickstart") {
		t.Fatalf("want kickstart error, got %v", err)
	}
}

func TestInstallDoesNotRetryOtherKickstartErrors(t *testing.T) {
	calls := fakeLaunchctl(t, map[string][]error{"print": {exitCode(113)}, "kickstart": {exitCode(5)}})
	if _, err := installService("/bin/vision"); err == nil {
		t.Fatal("want error")
	}
	if strings.Count(strings.Join(*calls, " "), "bootstrap") != 1 {
		t.Fatalf("retried a non-113 failure: %v", *calls)
	}
}

func TestRemoveWaitsForUnload(t *testing.T) {
	// print succeeds twice (still loaded), then reports the job gone.
	calls := fakeLaunchctl(t, map[string][]error{"print": {nil, nil, exitCode(113)}})
	if err := removeService(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*calls, " "); got != "bootout print print print" {
		t.Fatalf("calls: %s", got)
	}
}

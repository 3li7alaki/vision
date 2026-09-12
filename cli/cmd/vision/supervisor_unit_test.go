package main

import "strings"

import "testing"

func TestRenderUnitSupervisesLikeLaunchd(t *testing.T) {
	unit := renderUnit("/home/someone/.local/bin/vision")
	for _, want := range []string{
		"ExecStart=/home/someone/.local/bin/vision _serve",
		"Restart=always",          // launchd KeepAlive
		"WantedBy=default.target", // launchd RunAtLoad
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

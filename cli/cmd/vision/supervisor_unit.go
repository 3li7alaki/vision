package main

import "fmt"

// renderUnit builds the systemd user unit. It lives outside supervisor_linux.go
// so its test compiles and runs on any box, not only on the one platform that
// uses it.
func renderUnit(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=vision screenshot review queue
After=network.target

[Service]
ExecStart=%s _serve
Restart=always
RestartSec=1

[Install]
WantedBy=default.target
`, exe)
}

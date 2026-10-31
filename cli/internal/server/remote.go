package server

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The daemon binds loopback only. A phone reaches it through `tailscale serve`, which proxies to
// 127.0.0.1 and, for a request from a tailnet user's device, sets Tailscale-User-Login after
// stripping any copy the client sent. Tagged devices, funnel traffic and the machine itself get
// no login, so they are refused.
//
// Host alone cannot tell a proxied request from a local one: serve picks its backend by TLS SNI
// and passes the client's Host through, so a tailnet peer can send `Host: vision.test`. serve
// always overwrites X-Forwarded-Host and X-Forwarded-For, so either one marks a request remote.
// Checked against tailscale v1.104.1 ipn/ipnlocal/serve.go (getServeHandler, addProxyForwardedHeaders,
// addTailscaleIdentityHeaders).

var localHosts = map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "vision.test": true}

func remote(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return !localHosts[strings.ToLower(host)]
}

// Agents snap, withdraw and retire on this machine; only the human reviews from elsewhere.
var localOnly = map[string]bool{"/api/snap": true, "/api/withdraw": true, "/api/retire": true}

// guard wraps every route. CrossOriginProtection refuses a cross-site POST, which matters here
// because the tailnet identity rides on the connection, not a cookie: any page open on the phone
// could otherwise post a verdict as you. It also closes the same hole on 127.0.0.1. Requests with
// neither Sec-Fetch-Site nor Origin, like the CLI's, pass.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remote(r) {
			if localOnly[r.URL.Path] {
				problem(w, http.StatusForbidden, errors.New("snap and withdraw only run on this machine"))
				return
			}
			trusted := s.trustedUser()
			if trusted == "" || !strings.EqualFold(tailnetLogin(r), trusted) {
				problem(w, http.StatusForbidden, errors.New("not the trusted tailnet user"))
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
}

// tailnetLogin decodes the login serve sends MIME Q-encoded when it is not plain ASCII.
func tailnetLogin(r *http.Request) string {
	raw := r.Header.Get("Tailscale-User-Login")
	login, err := new(mime.WordDecoder).DecodeHeader(raw)
	if err != nil {
		return ""
	}
	return login
}

// trustedUserFile holds the one tailnet login allowed to review from another device. It is read on
// every remote request, so writing or deleting it takes effect without restarting the daemon.
// Missing or empty means nobody: remote review is off until the human names themselves.
func trustedUserFile() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "vision", "trusted-user")
}

func readTrustedUser() string {
	path := trustedUserFile()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

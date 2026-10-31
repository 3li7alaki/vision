package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	const tailnet = "mac.tail9795e1.ts.net:8443"
	// serve's own headers: it overwrites both forwarded headers and sets the login.
	proxied := func(login string) map[string]string {
		h := map[string]string{"X-Forwarded-Host": tailnet, "X-Forwarded-For": "100.64.0.7"}
		if login != "" {
			h["Tailscale-User-Login"] = login
		}
		return h
	}
	cases := []struct {
		name    string
		method  string
		path    string
		host    string
		headers map[string]string
		trusted string
		want    int
	}{
		{"local gallery", "GET", "/", "127.0.0.1:4747", nil, "", 200},
		{"local vision.test", "GET", "/", "vision.test:4747", nil, "", 200},
		{"local CLI snap", "POST", "/api/snap", "127.0.0.1:4747", nil, "", 200},
		{"local same-origin verdict", "POST", "/api/verdict", "vision.test:4747", map[string]string{"Sec-Fetch-Site": "same-origin"}, "", 200},
		{"cross-site verdict on loopback", "POST", "/api/verdict", "127.0.0.1:4747", map[string]string{"Sec-Fetch-Site": "cross-site"}, "", 403},
		{"remote, trusted login", "GET", "/", tailnet, proxied("ali@example.com"), "ali@example.com", 200},
		{"remote, login case differs", "GET", "/", tailnet, proxied("Ali@Example.com"), "ali@example.com", 200},
		{"remote, Q-encoded login", "GET", "/", tailnet, proxied(mime.QEncoding.Encode("utf-8", "عَلي@example.com")), "عَلي@example.com", 200},
		{"remote verdict, trusted", "POST", "/api/verdict", tailnet, proxied("ali@example.com"), "ali@example.com", 200},
		{"remote, no login (tagged node)", "GET", "/", tailnet, proxied(""), "ali@example.com", 403},
		{"remote, someone else", "GET", "/", tailnet, proxied("eve@example.com"), "ali@example.com", 403},
		{"remote, nobody trusted yet", "GET", "/", tailnet, proxied("ali@example.com"), "", 403},
		{"peer spoofs Host vision.test", "GET", "/", "vision.test:4747", proxied(""), "ali@example.com", 403},
		{"remote snap, even trusted", "POST", "/api/snap", tailnet, proxied("ali@example.com"), "ali@example.com", 403},
		{"remote withdraw, even trusted", "POST", "/api/withdraw", tailnet, proxied("ali@example.com"), "ali@example.com", 403},
		{"remote retire, even trusted", "POST", "/api/retire", tailnet, proxied("ali@example.com"), "ali@example.com", 403},
		{"cross-site verdict from the phone", "POST", "/api/verdict", tailnet,
			map[string]string{"X-Forwarded-For": "100.64.0.7", "Tailscale-User-Login": "ali@example.com", "Sec-Fetch-Site": "cross-site"}, "ali@example.com", 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New()
			s.trustedUser = func() string { return c.trusted }
			reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Host = c.host
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.guard(reached).ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestHomeScreenAssets(t *testing.T) {
	handler := New().Handler()
	for path, contentType := range map[string]string{
		"/manifest.webmanifest": "application/manifest+json",
		"/icon.svg":             "image/svg+xml",
		"/icon-180.png":         "image/png",
		"/icon-192.png":         "image/png",
		"/icon-512.png":         "image/png",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != contentType || rec.Body.Len() == 0 {
			t.Fatalf("%s: status %d, type %q, %d bytes", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
}

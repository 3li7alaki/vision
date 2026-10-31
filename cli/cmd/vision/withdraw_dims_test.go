package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"vision/internal/server"
	"vision/internal/store"
)

func TestReviewDimUsage(t *testing.T) {
	for _, command := range []string{"discard", "retire"} {
		for _, variant := range []string{"default", ""} {
			t.Run(command+"/as="+variant, func(t *testing.T) {
				args := []string{command, "room/probe", "--as", variant, "--dim", "locale=x"}
				if command == "retire" {
					args = append(args, "--reason", "Page deleted")
				}
				if err := run(args); err == nil || err.Error() != usageText {
					t.Fatalf("args=%v err=%v, want usage", args, err)
				}
			})
		}
	}
}

func TestReviewDims(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, command := range []string{"discard", "retire"} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", command, asJSON), func(t *testing.T) {
				t.Setenv("VISION_STATE_HOME", t.TempDir())
				t.Setenv("VISION_SESSION_ID", "successor")
				project, err := store.Identify(".")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.JoinThread(project.ID, "successor", "shared"); err != nil {
					t.Fatal(err)
				}
				variants := []string{"locale=x,scheme=dark,vp=desktop", "locale=y,scheme=dark,vp=desktop", "desktop"}
				want := 1
				if command == "retire" {
					variants = append(variants, "locale=x,scheme=light,vp=desktop")
					want = 2
				}
				for i, variant := range variants {
					snap := store.Snap{Key: "room/probe", Variant: variant, Digest: fmt.Sprintf("sha256:%04x", i), Session: "predecessor", Thread: "shared"}
					if err := store.AppendSnap(project.ID, snap, nil); err != nil {
						t.Fatal(err)
					}
				}
				handler := server.New().Handler()
				var request struct {
					Variant string
					Dims    map[string]string
				}
				http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path != "/health" {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						if err := json.Unmarshal(body, &request); err != nil {
							return nil, err
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
					}
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, r)
					return rec.Result(), nil
				})
				args := []string{command, "room/probe", "--dim", "vp=desktop", "--dim", "locale=x"}
				if command == "retire" {
					args = append(args, "--reason", "Page deleted")
				}
				if asJSON {
					args = append(args, "--json")
				}
				out, err := commandOutput(t, func() error { return run(args) })
				if err != nil {
					t.Fatalf("output=%q err=%v", out, err)
				}
				if request.Variant != "" || len(request.Dims) != 2 || request.Dims["locale"] != "x" || request.Dims["vp"] != "desktop" {
					t.Fatalf("request did not preserve partial dimensions: %+v", request)
				}
				if asJSON {
					if !json.Valid([]byte(out)) {
						t.Fatalf("invalid JSON output: %s", out)
					}
				} else if !strings.Contains(out, "room/probe@locale=x") {
					t.Fatalf("output lost target: %q", out)
				}
				withdrawals, err := store.Withdrawals(project.ID)
				if err != nil || len(withdrawals) != want {
					t.Fatalf("withdrawals=%v err=%v, want %d", withdrawals, err, want)
				}
				for _, withdrawal := range withdrawals {
					if dims := store.DecodeVariant(withdrawal.Variant); dims["locale"] != "x" || dims["vp"] != "desktop" || withdrawal.Thread != "shared" || withdrawal.Session != "successor" {
						t.Fatalf("wrong withdrawal: %+v", withdrawal)
					}
				}
			})
		}
	}
}

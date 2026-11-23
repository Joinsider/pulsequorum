// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package nodecli

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/version"
)

func TestVersionLine(t *testing.T) {
	line := VersionLine("pulsequorum-test")
	for _, want := range []string{"pulsequorum-test", version.Version, version.Commit, version.Date} {
		if !strings.Contains(line, want) {
			t.Errorf("VersionLine = %q, missing %q", line, want)
		}
	}
}

// freeAddr returns a loopback address on which nothing listens.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// healthServer answers /healthz with status and returns its host:port.
func healthServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHealthcheck(t *testing.T) {
	_, port, err := net.SplitHostPort(healthServer(t, http.StatusOK))
	if err != nil {
		t.Fatal(err)
	}
	unhealthyAddr := healthServer(t, http.StatusServiceUnavailable)
	closedAddr := freeAddr(t)

	tests := []struct {
		name string
		addr string
		want int
	}{
		{name: "healthy endpoint", addr: "127.0.0.1:" + port, want: 0},
		{name: "wildcard host is checked on loopback", addr: "0.0.0.0:" + port, want: 0},
		{name: "unhealthy endpoint", addr: unhealthyAddr, want: 1},
		{name: "nothing listening", addr: closedAddr, want: 1},
		{name: "invalid address", addr: "no-port-here", want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HTTP_ADDR", tc.addr)
			if got := Healthcheck(); got != tc.want {
				t.Errorf("Healthcheck() with HTTP_ADDR=%q = %d, want %d", tc.addr, got, tc.want)
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = orig
	out := <-done
	_ = r.Close()
	return out
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		level      slog.Level
		wantLine   string
		wantAbsent string
	}{
		{name: "text format", format: "text", level: slog.LevelInfo, wantLine: "msg=hello-text"},
		{name: "json format", format: "json", level: slog.LevelInfo, wantLine: `"msg":"hello-json"`},
		{name: "level filters info", format: "text", level: slog.LevelWarn, wantAbsent: "hello-text"},
		{name: "level filters info in json", format: "json", level: slog.LevelWarn, wantAbsent: "hello-json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				logger := NewLogger(config.Config{LogFormat: tc.format, LogLevel: tc.level})
				if logger == nil {
					t.Error("NewLogger returned nil")
					return
				}
				logger.Info("hello-" + tc.format)
			})
			if tc.wantLine != "" && !strings.Contains(out, tc.wantLine) {
				t.Errorf("log output %q does not contain %q", out, tc.wantLine)
			}
			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Errorf("log output %q contains %q, want it filtered by level", out, tc.wantAbsent)
			}
		})
	}
}

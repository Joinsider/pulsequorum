// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// serveRedirect runs the "public" redirect server on port 80. Each redirect
// points at a target that the SSRF protection must refuse.
func serveRedirect(log *slog.Logger) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", reply("ok"))
	mux.HandleFunc("/hit", reply("public hit"))
	mux.HandleFunc("/to-internal", redirect("http://10.99.0.10/hit"))
	mux.HandleFunc("/to-internal-name", redirect("http://internal.ssrf.test/hit"))
	mux.HandleFunc("/to-metadata", redirect("http://169.254.169.254/latest/meta-data/"))
	mux.HandleFunc("/to-loop", redirect("http://127.0.0.1:8080/api/v1/health"))
	return listenHTTP(log, "redirect", mux)
}

// serveTarget runs the internal canary on port 80. Every request is answered
// with 200 and appended to canaryPath; any entry while the protection is on is
// a failure of AS-07.
func serveTarget(log *slog.Logger, canaryPath string) error {
	// Create or truncate the file at startup so that an empty log is always readable.
	f, err := os.OpenFile(canaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create canary log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close canary log: %w", err)
	}
	c := &canary{log: log, path: canaryPath}
	return listenHTTP(log, "target", c)
}

// canary is the internal target. It writes one line per request.
type canary struct {
	log  *slog.Logger
	path string
	mu   sync.Mutex
}

func (c *canary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	line := fmt.Sprintf("%s %s from %s\n", r.Method, r.URL.RequestURI(), r.RemoteAddr)

	c.mu.Lock()
	f, err := os.OpenFile(c.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.WriteString(line)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	c.mu.Unlock()

	if err != nil {
		c.log.Error("canary log write failed", "err", err)
		http.Error(w, "canary log unavailable", http.StatusInternalServerError)
		return
	}
	c.log.Info("CANARY HIT", "method", r.Method, "path", r.URL.RequestURI(), "remote", r.RemoteAddr)
	_, _ = w.Write([]byte("internal"))
}

func reply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}
}

func redirect(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// listenHTTP serves handler on port 80 and logs every request.
func listenHTTP(log *slog.Logger, role string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              ":80",
		Handler:           logRequests(log, role, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("http listening", "role", role, "addr", srv.Addr)
	return srv.ListenAndServe()
}

func logRequests(log *slog.Logger, role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("http request", "role", role, "method", r.Method, "host", r.Host,
			"path", r.URL.RequestURI(), "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

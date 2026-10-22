// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"net/http"
	"sync/atomic"
)

// targetServer is the HTTP monitor target. It answers 200 OK while healthy
// and 503 Service Unavailable while failing. A 503 is deterministic and keeps
// the listener open, so the node sees a clean HTTP error instead of a
// connection error.
type targetServer struct {
	failing atomic.Bool
}

// SetFailing switches between 503 (true) and 200 (false).
func (t *targetServer) SetFailing(failing bool) {
	t.failing.Store(failing)
}

// ServeHTTP implements http.Handler.
func (t *targetServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if t.failing.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

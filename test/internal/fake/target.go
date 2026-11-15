// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package fake

import (
	"net/http"
	"sync"
	"sync/atomic"
)

// Target is the HTTP monitor target. It answers 200 OK while healthy and 503
// Service Unavailable while failing. A 503 is deterministic and keeps the
// listener open, so the node sees a clean HTTP error instead of a connection
// error. Every request is counted per path.
type Target struct {
	failing atomic.Bool

	mu   sync.Mutex
	hits map[string]int
}

// NewTarget returns a healthy target without recorded hits.
func NewTarget() *Target {
	return &Target{hits: make(map[string]int)}
}

// SetFailing switches between 503 (true) and 200 (false).
func (t *Target) SetFailing(failing bool) {
	t.failing.Store(failing)
}

// Failing reports whether the target currently answers 503.
func (t *Target) Failing() bool {
	return t.failing.Load()
}

// Hits returns how many requests arrived for the given URL path.
func (t *Target) Hits(path string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hits[path]
}

// TotalHits returns the number of requests for all paths.
func (t *Target) TotalHits() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	total := 0
	for _, n := range t.hits {
		total += n
	}
	return total
}

// ServeHTTP implements http.Handler.
func (t *Target) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	t.hits[r.URL.Path]++
	t.mu.Unlock()

	if t.failing.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

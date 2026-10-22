// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Pre-built header and body values keep the hot path allocation-light.
var (
	textPlainHeader = []string{"text/plain; charset=utf-8"}
	bodyOK          = []byte("ok")
	bodyDown        = []byte("down")
)

// targetFarm answers GET /t/{i} with 200 "ok", or 503 while target i is
// switched down through POST /control/{i}/down. The state of each target is
// one atomic flag, so concurrent requests and control calls never block.
type targetFarm struct {
	down    []atomic.Bool
	latency time.Duration
}

func newTargetFarm(count int, latency time.Duration) *targetFarm {
	return &targetFarm{down: make([]atomic.Bool, count), latency: latency}
}

func (f *targetFarm) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /t/{i}", f.serveTarget)
	mux.HandleFunc("POST /control/{i}/down", f.control(true))
	mux.HandleFunc("POST /control/{i}/up", f.control(false))
	return mux
}

func (f *targetFarm) index(r *http.Request) (int, bool) {
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil || i < 0 || i >= len(f.down) {
		return 0, false
	}
	return i, true
}

func (f *targetFarm) serveTarget(w http.ResponseWriter, r *http.Request) {
	i, ok := f.index(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.latency > 0 {
		t := time.NewTimer(f.latency)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
	w.Header()["Content-Type"] = textPlainHeader
	if f.down[i].Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(bodyDown)
		return
	}
	_, _ = w.Write(bodyOK)
}

func (f *targetFarm) control(down bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i, ok := f.index(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.down[i].Store(down)
		w.WriteHeader(http.StatusNoContent)
	}
}

func runTargets(ctx context.Context, args []string, stderr io.Writer, log *slog.Logger) error {
	fs := newFlagSet("targets", stderr)
	listen := fs.String("listen", ":18900", "address to listen on")
	count := fs.Int("count", 1000, "number of targets (indexes 0..count-1)")
	latency := fs.Duration("latency", 0, "fixed delay before each answer (0 = none)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *count < 1 {
		return fmt.Errorf("%w: -count must be at least 1", errUsage)
	}
	if *latency < 0 {
		return fmt.Errorf("%w: -latency must not be negative", errUsage)
	}

	farm := newTargetFarm(*count, *latency)
	srv := &http.Server{Addr: *listen, Handler: farm.handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("target farm listening", "addr", *listen, "count", *count, "latency", *latency)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package server provides the HTTP server of a voter: REST API, health endpoints,
// runtime frontend configuration and the embedded SPA.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/joinsider/uptimecluster/internal/api"
	"github.com/joinsider/uptimecluster/internal/config"
)

// Server is the HTTP server of a voter node.
type Server struct {
	cfg   config.Config
	log   *slog.Logger
	ui    http.Handler
	ready atomic.Bool
}

// New creates a server. ui serves the single-page application for all
// non-API paths.
func New(cfg config.Config, log *slog.Logger, ui http.Handler) *Server {
	return &Server{cfg: cfg, log: log, ui: ui}
}

// Handler returns the root handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleLiveness)
	mux.HandleFunc("GET /readyz", s.handleReadiness)
	mux.HandleFunc("GET /config.json", s.handleFrontendConfig)
	api.Register(mux, s.cfg, s.log)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		api.WriteProblem(w, http.StatusNotFound, "Not Found", "no API endpoint "+r.Method+" "+r.URL.Path)
	})
	mux.Handle("/", s.ui)

	return s.recoverPanics(securityHeaders(mux))
}

// Run serves HTTP on cfg.HTTPAddr until ctx is cancelled, then shuts down
// gracefully within cfg.ShutdownTimeout.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.HTTPAddr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is like Run but uses an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// WriteTimeout must be lifted per request for WebSocket streams (P1).
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	s.ready.Store(true)
	s.log.Info("http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errCh:
		s.ready.Store(false)
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	s.ready.Store(false)
	s.log.Info("shutting down http server", "timeout", s.cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic in http handler", "panic", rec, "method", r.Method, "path", r.URL.Path)
				api.WriteProblem(w, http.StatusInternalServerError, "Internal Server Error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets headers that are safe for every response. A full CSP
// follows with NFA-SEC-05 in phase 1.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

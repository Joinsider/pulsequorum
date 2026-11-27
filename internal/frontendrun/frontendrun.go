// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package frontendrun serves the SPA as a standalone frontend host (ADR 0036):
// the SPA with config.json, the security headers and a health endpoint, but
// without any API proxy. The host never forwards /api requests.
package frontendrun

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/joinsider/pulsequorum/internal/frontendcfg"
	"github.com/joinsider/pulsequorum/internal/webui"
)

const shutdownTimeout = 10 * time.Second

// healthBody is the answer of /healthz, used as container healthcheck.
var healthBody = []byte(`{"status":"ok"}` + "\n")

// Handler returns the routes of the frontend host. Every response carries the
// security headers of cfg. spa serves everything else.
func Handler(cfg frontendcfg.Config, spa http.Handler) http.Handler {
	headers := cfg.Headers()
	configJSON, configErr := cfg.ConfigJSON()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, _ *http.Request) {
		if configErr != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(configJSON)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(healthBody)
	})
	mux.Handle("/api/", http.NotFoundHandler())
	mux.Handle("/", spa)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, h := range headers {
			w.Header().Set(h.Name, h.Value)
		}
		mux.ServeHTTP(w, r)
	})
}

// Run serves the frontend host until ctx is done, then shuts down gracefully.
// It uses HTTPS if TLS files are configured.
func Run(ctx context.Context, cfg frontendcfg.Config, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           Handler(cfg, webui.Handler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			serveErr <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		serveErr <- srv.ListenAndServe()
	}()
	log.Info("serving frontend",
		"listen", cfg.Listen, "tls", cfg.TLSCert != "", "hsts", cfg.HSTS,
		"auth", cfg.Auth.Mode, "nodes", len(cfg.Nodes))

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve frontend: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down frontend", "timeout", shutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown frontend: %w", err)
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package nodecli holds the helpers that the pulsequorum and pulsequorum-probe
// binaries share: the version line, the healthcheck subcommand and the logger.
// It must not import voter-only packages, so that the probe binary stays slim (ADR 0043).
package nodecli

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/version"
)

// VersionLine returns the output of the version subcommand for binary.
func VersionLine(binary string) string {
	return fmt.Sprintf("%s %s (commit %s, built %s)", binary, version.Version, version.Commit, version.Date)
}

// NewLogger returns the structured logger configured by LOG_LEVEL and LOG_FORMAT.
func NewLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// Healthcheck probes the local liveness endpoint. It is used as container
// HEALTHCHECK because the distroless image has no shell or curl.
func Healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid HTTP_ADDR %q: %v\n", addr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz") //nolint:gosec // G704: target is the operator-configured local listen address, not user input
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

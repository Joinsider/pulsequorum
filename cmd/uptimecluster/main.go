// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command uptimecluster runs a UptimeCluster node. The role (voter or probe) is
// selected via NODE_ROLE; see docs/pflichtenheft.md, chapter 9.5.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/server"
	"github.com/joinsider/uptimecluster/internal/version"
	"github.com/joinsider/uptimecluster/internal/webui"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Printf("uptimecluster %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
			return 0
		case "healthcheck":
			return healthcheck()
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (available: version, healthcheck)\n", args[0])
			return 2
		}
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg)
	log.Info("starting uptimecluster",
		"version", version.Version, "commit", version.Commit,
		"role", cfg.Role, "node", cfg.NodeName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cfg.Role {
	case config.RoleVoter:
		if !webui.Built() {
			log.Warn("frontend is not embedded in this binary; build with `make build`")
		}
		if err := server.New(cfg, log, webui.Handler()).Run(ctx); err != nil {
			log.Error("voter stopped with error", "err", err)
			return 1
		}
	case config.RoleProbe:
		log.Warn("probe role is not implemented yet (phase 5); idling until shutdown")
		<-ctx.Done()
	}

	log.Info("stopped")
	return 0
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// healthcheck probes the local liveness endpoint. It is used as container
// HEALTHCHECK because the distroless image has no shell or curl.
func healthcheck() int {
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
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

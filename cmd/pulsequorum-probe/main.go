// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command pulsequorum-probe runs a probe node with only the probe role linked
// in (ADR 0043). It refuses NODE_ROLE=voter; voters use the pulsequorum binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/nodecli"
	"github.com/joinsider/pulsequorum/internal/proberun"
	"github.com/joinsider/pulsequorum/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println(nodecli.VersionLine("pulsequorum-probe"))
			return 0
		case "healthcheck":
			return nodecli.Healthcheck()
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (available: version, healthcheck)\n", args[0])
			return 2
		}
	}

	cfg, err := config.Load(defaultProbeRole(os.Getenv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	if cfg.Role != config.RoleProbe {
		fmt.Fprintf(os.Stderr, "pulsequorum-probe only runs NODE_ROLE=probe; use the pulsequorum binary for voters\n")
		return 2
	}
	log := nodecli.NewLogger(cfg)
	log.Info("starting pulsequorum-probe",
		"version", version.Version, "commit", version.Commit,
		"role", cfg.Role, "node", cfg.NodeName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := proberun.Run(ctx, cfg, log); err != nil {
		log.Error("probe stopped with error", "err", err)
		return 1
	}

	log.Info("stopped")
	return 0
}

// defaultProbeRole returns a getenv that reports NODE_ROLE=probe when the
// variable is unset or empty. config.Load defaults to voter, which this binary
// must not start.
func defaultProbeRole(getenv func(string) string) func(string) string {
	return func(key string) string {
		if key == "NODE_ROLE" && getenv(key) == "" {
			return string(config.RoleProbe)
		}
		return getenv(key)
	}
}

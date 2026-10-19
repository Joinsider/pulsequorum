// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command uptimecluster runs a UptimeCluster node. The role (voter or probe) is
// selected via NODE_ROLE; see docs/pflichtenheft.md, chapter 9.5.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/node"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/server"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
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
		if err := runVoter(ctx, cfg, log); err != nil {
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

// checkpointInterval is the period of the WAL checkpoint job (ADR 0013 C4).
const checkpointInterval = 60 * time.Second

func runVoter(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	state, err := sqlite.OpenState(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	metrics, err := sqlite.OpenMetrics(ctx, cfg.DataDir, sqlite.MetricsOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = metrics.Close() }()
	log.Info("storage ready", "data_dir", cfg.DataDir, "incarnation", metrics.Incarnation())

	go state.DB().RunCheckpointer(ctx, checkpointInterval, log)
	go metrics.DB().RunCheckpointer(ctx, checkpointInterval, log)

	nodeID, err := node.LoadOrCreateID(cfg.DataDir)
	if err != nil {
		return err
	}
	kek, err := secrets.LoadKEK(cfg.DataDir, cfg.MasterKey, cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	log.Info("master key loaded", "source", kek.Source, "fingerprint", kek.Fingerprint)
	if kek.Source == secrets.SourceGenerated && cfg.AuthMode == config.AuthZitadel {
		log.Warn("master key is stored next to the data; set MASTER_KEY_FILE for multi-tenant operation (ADR 0011)")
	}
	// A halted FSM stops the whole process (FA-CLU-13).
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	raftNode, err := cluster.Start(ctx, cluster.Config{
		NodeID:       nodeID,
		Advertise:    cfg.ClusterAdvertise,
		DataDir:      cfg.DataDir,
		Timeout:      cfg.RaftTimeout,
		Logger:       log,
		RaftLogLevel: cfg.LogLevel.String(),
		RaftLogJSON:  cfg.LogFormat == "json",
	}, state, stop)
	if err != nil {
		return err
	}
	defer func() {
		if err := raftNode.Shutdown(); err != nil {
			log.Warn("raft shutdown", "err", err)
		}
	}()
	log.Info("node ready", "node_id", nodeID, "advertise", cfg.ClusterAdvertise)

	keyring := secrets.NewKeyring(kek, state.ListSecretKeys)
	go bootstrapCluster(ctx, cfg, log, raftNode, state, kek, keyring, stop)
	go cluster.RunKeyRotation(ctx, raftNode, state, kek, cluster.DefaultKeyRotation, time.Hour, log)

	if !webui.Built() {
		log.Warn("frontend is not embedded in this binary; build with `make build`")
	}
	err = server.New(cfg, log, webui.Handler()).WithReadiness(raftNode.Ready).Run(ctx)
	// A cause other than plain cancellation (signal, normal shutdown) is fatal.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

// bootstrapCluster creates the default tenant and the first data key once a
// leader exists, then loads the keyring. A master key that does not match the
// cluster stops the node (ADR 0011).
func bootstrapCluster(ctx context.Context, cfg config.Config, log *slog.Logger, n *cluster.Node,
	state *sqlite.StateStore, kek *secrets.KEK, keyring *secrets.Keyring, stop context.CancelCauseFunc) {
	bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if cfg.AuthMode == config.AuthLocal {
		if err := cluster.EnsureDefaultTenant(bootCtx, n, state); err != nil && ctx.Err() == nil {
			log.Error("could not create the default tenant", "err", err)
		}
	}
	if err := cluster.EnsureSecretKey(bootCtx, n, state, kek); err != nil && ctx.Err() == nil {
		log.Error("could not create the data key", "err", err)
	}
	if err := keyring.Reload(bootCtx); err != nil {
		if errors.Is(err, secrets.ErrKEKMismatch) {
			stop(err)
			return
		}
		if ctx.Err() == nil {
			log.Error("could not load data keys", "err", err)
		}
		return
	}
	log.Info("data keys loaded", "active_key", keyring.ActiveKeyID())
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

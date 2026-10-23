// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command uptimecluster runs a UptimeCluster node. The role (voter or probe) is
// selected via NODE_ROLE; see docs/pflichtenheft.md, chapter 9.5.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	// All voters evaluate maintenance time zones with the same embedded database.
	_ "time/tzdata"

	"github.com/joinsider/uptimecluster/internal/aggregator"
	"github.com/joinsider/uptimecluster/internal/alert"
	"github.com/joinsider/uptimecluster/internal/api"
	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/checker"
	_ "github.com/joinsider/uptimecluster/internal/checker/builtin" // registers all monitor types
	"github.com/joinsider/uptimecluster/internal/checker/icmpcheck"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusterhost"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/configsecret"
	"github.com/joinsider/uptimecluster/internal/maintenance"
	"github.com/joinsider/uptimecluster/internal/membership"
	"github.com/joinsider/uptimecluster/internal/node"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/scheduler"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/server"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
	"github.com/joinsider/uptimecluster/internal/stream"
	"github.com/joinsider/uptimecluster/internal/uptime"
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

const (
	// checkpointInterval is the period of the WAL checkpoint job (ADR 0013 C4).
	checkpointInterval = 60 * time.Second
	// reconcileInterval bounds how long the scheduler may miss a change signal.
	reconcileInterval = 30 * time.Second
)

func runVoter(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// A halted FSM or a foreign master key stops the whole process (FA-CLU-13).
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

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

	// ICMP self-test (FA-CHK-31, FA-NODE-06).
	icmpMode, icmpErr := icmpcheck.DetectMode(ctx, cfg.ICMPMode)
	if icmpMode == icmpcheck.ModeOff && cfg.ICMPMode != icmpcheck.ModeOff {
		log.Warn("ICMP checks are unavailable on this node; see docs/betrieb/icmp.md", "configured", cfg.ICMPMode, "err", icmpErr)
	} else {
		log.Info("ICMP mode", "mode", icmpMode)
	}

	nodeID, err := node.LoadOrCreateID(cfg.DataDir)
	if err != nil {
		return err
	}
	joinToken, err := readJoinToken(cfg)
	if err != nil {
		return err
	}
	// Join before the master key is loaded: a joining voter takes the
	// cluster's key instead of generating its own (ADR 0011).
	prepared, err := clusterhost.Prepare(ctx, cfg.DataDir, nodeID, cfg.NodeName, cfg.ClusterAdvertise, version.Version,
		clusterhost.JoinOptions{Seeds: cfg.ClusterJoin, Token: joinToken, ExternalKEK: cfg.MasterKey != "" || cfg.MasterKeyFile != ""}, log)
	if err != nil {
		return err
	}
	kek, err := secrets.LoadKEK(cfg.DataDir, cfg.MasterKey, cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	if err := prepared.CheckKEK(kek); err != nil {
		return err
	}
	log.Info("master key loaded", "source", kek.Source, "fingerprint", kek.Fingerprint)
	if kek.Source == secrets.SourceGenerated && cfg.AuthMode == config.AuthZitadel {
		log.Warn("master key is stored next to the data; set MASTER_KEY_FILE for multi-tenant operation (ADR 0011)")
	}

	keyring := secrets.NewKeyring(kek, state.ListSecretKeys)
	host, err := clusterhost.Start(ctx, clusterhost.Options{
		NodeID: nodeID, ListenAddr: cfg.ClusterAddr, Advertise: cfg.ClusterAdvertise, DataDir: cfg.DataDir,
		Timeout: cfg.RaftTimeout, State: state, Metrics: metrics, KEK: kek, Keyring: keyring, Prepared: prepared, Log: log,
		RaftLogLevel: cfg.LogLevel.String(), RaftLogJSON: cfg.LogFormat == "json", OnFatal: stop,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := host.Close(); err != nil {
			log.Warn("raft shutdown", "err", err)
		}
	}()
	raftNode := host.Node
	log.Info("node ready", "node_id", nodeID, "advertise", cfg.ClusterAdvertise)

	// Live stream (FA-API-04): committed status changes reach the hub through
	// the bridge, which the FSM calls after each commit without blocking.
	hub := stream.NewHub(0)
	bridge := stream.NewStateBridge(hub, state, log)
	raftNode.FSM().OnStateChange(bridge.Handle)

	// Background work stops and finishes before Raft and the stores close
	// (deferred calls run in reverse order).
	var bg sync.WaitGroup
	defer func() {
		stop(nil)
		bg.Wait()
	}()

	bg.Go(func() { state.DB().RunCheckpointer(ctx, checkpointInterval, log) })
	bg.Go(func() { metrics.DB().RunCheckpointer(ctx, checkpointInterval, log) })

	var local *auth.Local
	if cfg.AuthMode == config.AuthLocal {
		local = auth.NewLocal(auth.LocalOptions{State: state, Cluster: raftNode, Keyring: keyring, Logger: log})
	}
	bg.Go(func() {
		if bootstrapCluster(ctx, cfg, log, raftNode, state, kek, keyring, local, stop) {
			bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := host.Bootstrap(bootCtx); err != nil && ctx.Err() == nil {
				log.Error("cluster PKI setup failed", "err", err)
			}
			cancel()
		}
		if local != nil {
			local.RunKeyMaintenance(ctx, raftNode.IsLeader, time.Hour)
		}
	})
	bg.Go(func() { host.Run(ctx) })
	bg.Go(func() { cluster.RunKeyRotation(ctx, raftNode, state, kek, cluster.DefaultKeyRotation, time.Hour, log) })
	bg.Go(func() { bridge.Run(ctx) })

	heartbeats, err := scheduler.NewHeartbeatWriter(ctx, metrics, nodeID, log)
	if err != nil {
		return err
	}
	// Stored heartbeats are pushed to the other voters without blocking the writer.
	heartbeats.SetOnStored(host.Replication.Publish)
	var policy checker.AddressPolicy
	if cfg.SSRFProtectionEnabled() {
		policy = checker.BlockPrivate{}
	}
	log.Info("SSRF protection", "enabled", policy != nil, "setting", cfg.SSRFProtection)
	checkEnv := &checker.Env{
		Dialer:    &checker.Dialer{Policy: policy},
		UserAgent: "UptimeCluster/" + version.Version,
		ICMPMode:  icmpMode,
	}
	sender := alert.NewSender(checkEnv.Dialer, log)
	dispatcher := alert.NewDispatcher(alert.DispatcherOptions{
		State: state, Cluster: raftNode, Keyring: keyring, Sender: sender,
		IsLeader: raftNode.IsLeader, MaxAttempts: cfg.AlertMaxAttempts, Logger: log,
	})
	maint := maintenance.NewController(maintenance.ControllerOptions{
		State: state, Cluster: raftNode, IsLeader: raftNode.IsLeader, Logger: log,
	})
	agg := aggregator.New(aggregator.Options{
		State: state, Metrics: metrics, Cluster: raftNode, NodeID: nodeID,
		Maintenance: maint.InMaintenance, Logger: log,
	})
	sink := scheduler.Sinks{heartbeats, agg, &stream.HeartbeatSink{Hub: hub, NodeID: nodeID}}
	sched := scheduler.New(scheduler.Options{Registry: checker.Default, Env: checkEnv, Sink: sink, Logger: log,
		Resolve: func(ctx context.Context, m store.Monitor) (json.RawMessage, error) {
			return configsecret.ResolveMonitor(ctx, keyring, m)
		}})
	bg.Go(func() { heartbeats.Run(ctx) })
	bg.Go(func() { agg.Run(ctx) })
	bg.Go(func() { dispatcher.Run(ctx) })
	bg.Go(func() { maint.Run(ctx) })
	bg.Go(func() {
		configsecret.RunMigration(ctx, configsecret.MigrationOptions{
			State: state, Cluster: raftNode, Keyring: keyring, Schemas: &configsecret.Schemas{Registry: checker.Default},
			IsLeader: raftNode.IsLeader, Logger: log,
		})
	})
	maintenance := uptime.NewJob(uptime.JobOptions{
		State: state, Metrics: metrics, Logger: log, Compaction: cfg.RawCompaction,
		Retention: uptime.Retention{Raw: cfg.RetentionRaw, Hourly: cfg.RetentionHourly, Daily: cfg.RetentionDaily},
		Vacuum:    func(ctx context.Context) error { return metrics.DB().IncrementalVacuum(ctx, 0) },
		// Late heartbeats of other voters are waited for before rollup and compaction (FA-REP-03).
		ReplicatedUntil: host.Replication.ReplicatedUntil,
	})
	bg.Go(func() { maintenance.Run(ctx, time.Minute) })
	bg.Go(func() { sched.Run(ctx) })
	bg.Go(func() { scheduler.Reconcile(ctx, state, raftNode.FSM().Changes(), reconcileInterval, log, sched, agg) })

	if !webui.Built() {
		log.Warn("frontend is not embedded in this binary; build with `make build`")
	}
	apiDeps := api.Deps{
		State: state, Metrics: metrics, Cluster: raftNode, Registry: checker.Default,
		CheckEnv: checkEnv, NodeID: nodeID,
		Keyring: keyring, Alerts: sender,
		Stream: hub, Tickets: auth.NewTickets(),
		ClusterAdmin: clusterAdmin{host: host},
		Replication:  host.Replication,
	}
	if local != nil {
		apiDeps.Local, apiDeps.Authenticator = local, local
	}
	err = server.New(cfg, log, webui.Handler()).WithAPI(apiDeps).WithReadiness(raftNode.Ready).Run(ctx)
	// A cause other than plain cancellation (signal, normal shutdown) is fatal.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

// bootstrapCluster creates the default tenant and the first data key once a
// leader exists, then loads the keyring. A master key that does not match the
// cluster stops the node (ADR 0011). It reports whether the keys are
// available, which everything that encrypts depends on.
func bootstrapCluster(ctx context.Context, cfg config.Config, log *slog.Logger, n *cluster.Node,
	state *sqlite.StateStore, kek *secrets.KEK, keyring *secrets.Keyring, local *auth.Local, stop context.CancelCauseFunc) bool {
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
			return false
		}
		if ctx.Err() == nil {
			log.Error("could not load data keys", "err", err)
		}
		return false
	}
	log.Info("data keys loaded", "active_key", keyring.ActiveKeyID())
	if local == nil {
		return true
	}
	if err := local.EnsureSigningKey(bootCtx); err != nil && ctx.Err() == nil {
		log.Error("could not create the access token signing key", "err", err)
	}
	// FA-AUTH-01: without any user, a one-time setup token is printed once.
	// It is the only secret ever logged; it expires after one hour and only
	// its hash is kept in memory.
	token, ok, err := local.PrepareSetup(bootCtx)
	switch {
	case err != nil && ctx.Err() == nil:
		log.Error("could not prepare the setup", "err", err)
	case ok:
		log.Warn("no user exists yet: create the first admin within 1 hour via POST /api/v1/auth/setup (or the web UI) with this one-time token",
			"setup_token", token)
	}
	return true
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

// readJoinToken returns the join token from JOIN_TOKEN or JOIN_TOKEN_FILE.
// It is a secret and never logged.
func readJoinToken(cfg config.Config) (string, error) {
	if cfg.JoinTokenFile == "" {
		return cfg.JoinToken, nil
	}
	raw, err := os.ReadFile(cfg.JoinTokenFile)
	if err != nil {
		return "", fmt.Errorf("JOIN_TOKEN_FILE: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// clusterAdmin serves the cluster administration endpoints from the cluster
// host (api.ClusterAdmin).
type clusterAdmin struct {
	host *clusterhost.Host
}

func (a clusterAdmin) CreateJoinToken(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration) (string, time.Time, error) {
	return a.host.Authority.CreateJoinToken(ctx, role, tenantID, ttl)
}

func (a clusterAdmin) Remove(ctx context.Context, nodeID string) (membership.Removal, error) {
	return a.host.Manager.Remove(ctx, nodeID)
}

func (a clusterAdmin) Members() ([]cluster.Member, uint64, error) { return a.host.Node.Members() }

func (a clusterAdmin) Leader() (address, id string) { return a.host.Node.Leader() }

func (a clusterAdmin) ID() string { return a.host.Node.ID() }

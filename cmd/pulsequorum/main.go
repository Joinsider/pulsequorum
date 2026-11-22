// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command pulsequorum runs a Pulsequorum node. The role (voter or probe) is
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
	"sync/atomic"
	"syscall"
	"time"
	// All voters evaluate maintenance time zones with the same embedded database.
	_ "time/tzdata"

	"github.com/joinsider/pulsequorum/internal/admincli"
	"github.com/joinsider/pulsequorum/internal/aggregator"
	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/api"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/authswitch"
	"github.com/joinsider/pulsequorum/internal/checker"
	_ "github.com/joinsider/pulsequorum/internal/checker/builtin" // registers all monitor types
	"github.com/joinsider/pulsequorum/internal/checker/httpcheck"
	"github.com/joinsider/pulsequorum/internal/checker/icmpcheck"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusterhost"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	"github.com/joinsider/pulsequorum/internal/degraded"
	"github.com/joinsider/pulsequorum/internal/diskstat"
	"github.com/joinsider/pulsequorum/internal/maintenance"
	"github.com/joinsider/pulsequorum/internal/membership"
	ucmetrics "github.com/joinsider/pulsequorum/internal/metrics"
	"github.com/joinsider/pulsequorum/internal/node"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/selfcheck"
	"github.com/joinsider/pulsequorum/internal/selfhealth"
	"github.com/joinsider/pulsequorum/internal/server"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
	"github.com/joinsider/pulsequorum/internal/stream"
	"github.com/joinsider/pulsequorum/internal/sysalert"
	"github.com/joinsider/pulsequorum/internal/tenantpurge"
	"github.com/joinsider/pulsequorum/internal/uptime"
	"github.com/joinsider/pulsequorum/internal/version"
	"github.com/joinsider/pulsequorum/internal/webui"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Printf("pulsequorum %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
			return 0
		case "healthcheck":
			return healthcheck()
		case "admin":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return admincli.Run(ctx, args[1:], admincli.Env{Getenv: os.Getenv, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (available: version, healthcheck, admin)\n", args[0])
			return 2
		}
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg)
	log.Info("starting pulsequorum",
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
		if err := runProbe(ctx, cfg, log); err != nil {
			log.Error("probe stopped with error", "err", err)
			return 1
		}
	}

	log.Info("stopped")
	return 0
}

const (
	// checkpointInterval is the period of the WAL checkpoint job (ADR 0013 C4).
	checkpointInterval = 60 * time.Second
	// reconcileInterval bounds how long the scheduler may miss a change signal.
	reconcileInterval = 30 * time.Second
	// keyRefreshInterval bounds how long a joined voter has no active data key
	// and how long a node encrypts with the previous key after a rotation.
	keyRefreshInterval = 10 * time.Second
)

type voterProber struct {
	icmpMode     string
	capabilities []string
	policy       checker.AddressPolicy
	dialer       *checker.Dialer
	selfProber   *selfcheck.Prober
}

type voterCreds struct {
	nodeID       string
	metricsToken string
	prepared     clusterhost.Prepared
	kek          *secrets.KEK
}

type voterRuntimeDeps struct {
	cfg            config.Config
	log            *slog.Logger
	clock          func() time.Time
	state          *sqlite.StateStore
	metrics        *sqlite.MetricsStore
	ingest         *tenantpurge.IngestFilter
	tombstones     *tenantpurge.Tombstones
	creds          voterCreds
	prober         voterProber
	keyring        *secrets.Keyring
	host           *clusterhost.Host
	hub            *stream.Hub
	local          *auth.Local
	aggPtr         *atomic.Pointer[aggregator.Aggregator]
	degradedAggPtr *atomic.Pointer[aggregator.Aggregator]
	metricsPtr     *atomic.Pointer[ucmetrics.Metrics]
	startLastSeen  int64
}

func runVoter(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	// clock is the wall clock of this node (ADR 0020). Tests replace it per
	// node to simulate clock skew; the binary always uses the system clock.
	clock := time.Now
	// A halted FSM or a foreign master key stops the whole process (FA-CLU-13).
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	state, metrics, startLastIssued, tombstones, ingest, err := openVoterStores(ctx, cfg.DataDir, log)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	defer func() { _ = metrics.Close() }()

	prober, err := initProberAndCapabilities(ctx, cfg, clock, log)
	if err != nil {
		return err
	}
	creds, err := initClusterCredentials(ctx, cfg, clock, log)
	if err != nil {
		return err
	}
	keyring := secrets.NewKeyring(creds.kek, state.ListSecretKeys)
	var aggPtr, degradedAggPtr atomic.Pointer[aggregator.Aggregator]
	var metricsPtr atomic.Pointer[ucmetrics.Metrics]
	deps := voterRuntimeDeps{
		cfg: cfg, log: log, clock: clock, state: state, metrics: metrics, ingest: ingest, tombstones: tombstones,
		creds: creds, prober: prober, keyring: keyring,
		aggPtr: &aggPtr, degradedAggPtr: &degradedAggPtr, metricsPtr: &metricsPtr, startLastSeen: startLastIssued,
	}
	host, err := startVoterHost(ctx, deps, stop)
	if err != nil {
		return err
	}
	defer func() {
		if err := host.Close(); err != nil {
			log.Warn("raft shutdown", "err", err)
		}
	}()
	deps.host = host
	raftNode := host.Node
	log.Info("node ready", "node_id", creds.nodeID, "advertise", cfg.ClusterAdvertise)

	// Live stream (FA-API-04): committed status changes reach the hub through
	// the bridge, which the FSM calls after each commit without blocking.
	hub := stream.NewHub(0)
	bridge := stream.NewStateBridge(hub, state, log)
	raftNode.FSM().OnStateChange(bridge.Handle)
	deps.hub = hub

	// Background work stops and finishes before Raft and the stores close
	// (deferred calls run in reverse order).
	var bg sync.WaitGroup
	defer func() {
		stop(nil)
		bg.Wait()
	}()

	if cfg.AuthMode == config.AuthLocal {
		deps.local = auth.NewLocal(auth.LocalOptions{State: state, Cluster: raftNode, Keyring: keyring, Logger: log, Now: clock})
	}
	startCoreBackgroundJobs(ctx, &bg, deps, bridge, stop)

	apiDeps, nodeMetrics, err := startMonitoringPipeline(ctx, &bg, deps)
	if err != nil {
		return err
	}
	if !webui.Built() {
		log.Warn("frontend is not embedded in this binary; build with `make build`")
	}
	readiness, err := configureVoterAuth(ctx, &bg, deps, nodeMetrics, &apiDeps)
	if err != nil {
		return err
	}
	err = server.New(cfg, log, webui.Handler()).WithAPI(apiDeps).WithReadiness(readiness).Run(ctx)
	// A cause other than plain cancellation (signal, normal shutdown) is fatal.
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

func openVoterStores(ctx context.Context, dataDir string, log *slog.Logger) (*sqlite.StateStore, *sqlite.MetricsStore, int64, *tenantpurge.Tombstones, *tenantpurge.IngestFilter, error) {
	state, err := sqlite.OpenState(ctx, dataDir)
	if err != nil {
		return nil, nil, 0, nil, nil, err
	}
	// Read before Raft starts so the gap job sees the time before the quorum
	// returned (ADR 0033).
	startLastIssued, err := state.LastIssuedAt(ctx)
	if err != nil {
		_ = state.Close()
		return nil, nil, 0, nil, nil, fmt.Errorf("read last command time: %w", err)
	}
	metrics, err := sqlite.OpenMetrics(ctx, dataDir, sqlite.MetricsOptions{})
	if err != nil {
		_ = state.Close()
		return nil, nil, 0, nil, nil, err
	}
	log.Info("storage ready", "data_dir", dataDir, "incarnation", metrics.Incarnation())
	// Heartbeats of deleted tenants are dropped on ingest; the purge job keeps the tombstones current (ADR 0029).
	tombstones := tenantpurge.NewTombstones()
	ingest := tenantpurge.NewIngestFilter(metrics, tombstones, log)
	return state, metrics, startLastIssued, tombstones, ingest, nil
}

func initProberAndCapabilities(ctx context.Context, cfg config.Config, clock func() time.Time, log *slog.Logger) (voterProber, error) {
	// ICMP self-test (FA-CHK-31, FA-NODE-06).
	icmpMode, icmpErr := icmpcheck.DetectMode(ctx, cfg.ICMPMode)
	if icmpMode == icmpcheck.ModeOff && cfg.ICMPMode != icmpcheck.ModeOff {
		log.Warn("ICMP checks are unavailable on this node; see docs/betrieb/icmp.md", "configured", cfg.ICMPMode, "err", icmpErr)
	} else {
		log.Info("ICMP mode", "mode", icmpMode)
	}
	// Capabilities tell the other nodes which monitors this node can run (ADR 0017).
	// http-client-cert tells the API that this binary understands client
	// certificates in HTTP monitors (ADR 0042).
	capabilities := []string{httpcheck.ClientCertCapability}
	if icmpMode != icmpcheck.ModeOff {
		capabilities = append(capabilities, icmpcheck.Capability)
	}
	// Node self-check (ADR 0022): the reference targets are probed through the
	// same SSRF-aware dialer as the checkers, so they obey the same address policy.
	var policy checker.AddressPolicy
	if cfg.SSRFProtectionEnabled() {
		policy = checker.BlockPrivate{}
	}
	log.Info("SSRF protection", "enabled", policy != nil, "setting", cfg.SSRFProtection)
	dialer := &checker.Dialer{Policy: policy}
	selfTargets, err := selfcheck.ParseTargets(cfg.SelfCheckTargets)
	if err != nil {
		return voterProber{}, fmt.Errorf("SELF_CHECK_TARGETS: %w", err)
	}
	selfProber, err := selfcheck.New(selfcheck.Options{Targets: selfTargets, Dialer: dialer, Now: clock, Logger: log})
	if err != nil {
		return voterProber{}, err
	}
	if len(selfTargets) > 0 {
		log.Info("node self-check enabled", "targets", len(selfTargets))
	}
	return voterProber{
		icmpMode:     icmpMode,
		capabilities: capabilities,
		policy:       policy,
		dialer:       dialer,
		selfProber:   selfProber,
	}, nil
}

func initClusterCredentials(ctx context.Context, cfg config.Config, clock func() time.Time, log *slog.Logger) (voterCreds, error) {
	nodeID, err := node.LoadOrCreateID(cfg.DataDir)
	if err != nil {
		return voterCreds{}, err
	}
	joinToken, err := readJoinToken(cfg)
	if err != nil {
		return voterCreds{}, err
	}
	metricsToken, err := cfg.ResolveMetricsToken()
	if err != nil {
		return voterCreds{}, err
	}
	// Join before the master key is loaded: a joining voter takes the
	// cluster's key instead of generating its own (ADR 0011).
	prepared, err := clusterhost.Prepare(ctx, cfg.DataDir, nodeID, cfg.NodeName, cfg.ClusterAdvertise, version.Version,
		clusterhost.JoinOptions{Seeds: cfg.ClusterJoin, Token: joinToken, ExternalKEK: cfg.MasterKey != "" || cfg.MasterKeyFile != "", Clock: clock}, log)
	if err != nil {
		return voterCreds{}, err
	}
	kek, err := secrets.LoadKEK(cfg.DataDir, cfg.MasterKey, cfg.MasterKeyFile)
	if err != nil {
		return voterCreds{}, fmt.Errorf("master key: %w", err)
	}
	if err := prepared.CheckKEK(kek); err != nil {
		return voterCreds{}, err
	}
	log.Info("master key loaded", "source", kek.Source, "fingerprint", kek.Fingerprint)
	if kek.Source == secrets.SourceGenerated && cfg.AuthMode == config.AuthOIDC && cfg.OIDC.Tenancy == config.TenancyMulti {
		log.Warn("master key is stored next to the data; set MASTER_KEY_FILE for multi-tenant operation (ADR 0011)")
	}
	return voterCreds{nodeID: nodeID, metricsToken: metricsToken, prepared: prepared, kek: kek}, nil
}

func startVoterHost(ctx context.Context, deps voterRuntimeDeps, stop context.CancelCauseFunc) (*clusterhost.Host, error) {
	cfg := deps.cfg
	// The aggregator needs the Raft node, which exists only after Start. Stored
	// heartbeats that arrive before it is set are not observed; the leader
	// rebuild reads them from the store (ADR 0021).
	return clusterhost.Start(ctx, clusterhost.Options{
		// The disk and WAL signals of GetStatus (ADR 0025, section 2).
		DiskFreeRatio: func() float64 { return diskFreeRatio(cfg.DataDir) },
		WALBytes:      func() (int64, int64) { return walSize(deps.state.DB()), walSize(deps.metrics.DB()) },
		NodeID:        deps.creds.nodeID, ListenAddr: cfg.ClusterAddr, Advertise: cfg.ClusterAdvertise, DataDir: cfg.DataDir,
		Timeout: cfg.RaftTimeout, State: deps.state, Metrics: deps.ingest, KEK: deps.creds.kek, Keyring: deps.keyring,
		Prepared: deps.creds.prepared, Log: deps.log,
		RaftLogLevel: cfg.LogLevel.String(), RaftLogJSON: cfg.LogFormat == "json", OnFatal: stop,
		Name: cfg.NodeName, Version: version.Version, Capabilities: deps.prober.capabilities,
		Clock:     deps.clock,
		SelfCheck: deps.prober.selfProber.Status,
		// The SSRF setting of this node, reported in GetStatus (ADR 0030).
		SSRFProtection:    cfg.SSRFProtectionEnabled(),
		ProbeAssigner:     scheduler.Assigner{Capability: scheduler.Capabilities(checker.Default)},
		ProbeKeyring:      deps.keyring,
		ProbeBufferMaxAge: cfg.ProbeBufferMaxAge,
		OnHeartbeatsStored: func(hbs []store.Heartbeat) {
			observeHeartbeats(deps.aggPtr, deps.degradedAggPtr, hbs)
		},
		OnProbeRejected: func(reason string) {
			if m := deps.metricsPtr.Load(); m != nil {
				m.ProbeRejected(reason)
			}
		},
	})
}

func diskFreeRatio(dir string) float64 {
	ratio, err := diskstat.FreeRatio(dir)
	if err != nil {
		return -1
	}
	return ratio
}

func observeHeartbeats(aggPtr, degradedAggPtr *atomic.Pointer[aggregator.Aggregator], hbs []store.Heartbeat) {
	if a := aggPtr.Load(); a != nil {
		a.Observe(hbs)
	}
	if a := degradedAggPtr.Load(); a != nil {
		a.Observe(hbs)
	}
}

func startCoreBackgroundJobs(ctx context.Context, bg *sync.WaitGroup, deps voterRuntimeDeps, bridge *stream.StateBridge, stop context.CancelCauseFunc) {
	raftNode := deps.host.Node
	bg.Go(func() { deps.state.DB().RunCheckpointer(ctx, checkpointInterval, deps.log) })
	bg.Go(func() { deps.metrics.DB().RunCheckpointer(ctx, checkpointInterval, deps.log) })
	bg.Go(func() { runBootstrapAndKeyMaintenance(ctx, deps, stop) })
	bg.Go(func() { deps.host.Run(ctx) })
	bg.Go(func() {
		cluster.RunKeyRotation(ctx, raftNode, deps.state, deps.creds.kek, cluster.DefaultKeyRotation, time.Hour, deps.log)
	})
	// The keyring loads once in bootstrapCluster; a voter that joined loads it
	// before catching up and would have no active key. Refresh also picks up
	// rotated keys on every node.
	bg.Go(func() {
		if err := deps.keyring.Refresh(ctx, keyRefreshInterval, deps.log); err != nil {
			stop(err)
		}
	})
	bg.Go(func() { cluster.RunAuditPrune(ctx, raftNode, deps.state, deps.cfg.AuditRetention, deps.log) })
	bg.Go(func() { bridge.Run(ctx) })
}

func runBootstrapAndKeyMaintenance(ctx context.Context, deps voterRuntimeDeps, stop context.CancelCauseFunc) {
	raftNode := deps.host.Node
	if bootstrapCluster(ctx, deps.log, raftNode, deps.state, deps.creds.kek, deps.keyring, deps.local, stop) {
		bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := deps.host.Bootstrap(bootCtx); err != nil && ctx.Err() == nil {
			deps.log.Error("cluster PKI setup failed", "err", err)
		}
		cancel()
	}
	if deps.local != nil {
		deps.local.RunKeyMaintenance(ctx, raftNode.IsLeader, time.Hour)
	}
}

type voterWorkers struct {
	heartbeats      *scheduler.HeartbeatWriter
	agg             *aggregator.Aggregator
	detector        *degraded.Detector
	overlay         *degraded.Overlay
	localDispatcher *alert.LocalDispatcher
	degradedAgg     *aggregator.Aggregator
	reporter        *degraded.Reporter
	gapJob          *degraded.GapJob
	quorumNotifier  *degraded.QuorumNotifier
	alertCollector  *sysalert.Collector
	systemJobs      *sysalert.Jobs
	judge           *degraded.Judge
	assessor        *selfhealth.Assessor
	dispatcher      *alert.Dispatcher
	maint           *maintenance.Controller
	sched           *scheduler.Scheduler
	nodeMetrics     *ucmetrics.Metrics
	assigner        scheduler.Assigner
}

func startMonitoringPipeline(ctx context.Context, bg *sync.WaitGroup, deps voterRuntimeDeps) (api.Deps, *ucmetrics.Metrics, error) {
	raftNode := deps.host.Node
	heartbeats, err := scheduler.NewHeartbeatWriter(ctx, deps.ingest, deps.creds.nodeID, deps.log)
	if err != nil {
		return api.Deps{}, nil, err
	}
	// Stored heartbeats are pushed to the other voters without blocking the writer.
	heartbeats.SetOnStored(deps.host.Replication.Publish)
	checkEnv := &checker.Env{
		Dialer: deps.prober.dialer, UserAgent: "Pulsequorum/" + version.Version, ICMPMode: deps.prober.icmpMode, Now: deps.clock,
	}
	sender := alert.NewSender(checkEnv.Dialer, deps.log).WithClock(deps.clock)
	// nodeMetrics is set before any dispatcher runs; the outcome hooks read it.
	var nodeMetrics *ucmetrics.Metrics
	outcomes := func(kind, outcome string) {
		if nodeMetrics != nil {
			nodeMetrics.AlertDelivered(kind, outcome)
		}
	}
	dispatcher := alert.NewDispatcher(alert.DispatcherOptions{
		State: deps.state, Cluster: raftNode, Keyring: deps.keyring, Sender: sender,
		IsLeader: raftNode.IsLeader, MaxAttempts: deps.cfg.AlertMaxAttempts, Logger: deps.log, Now: deps.clock,
		BundleWindow: deps.cfg.AlertBundleWindow, OnOutcome: outcomes,
	})
	maint := maintenance.NewController(maintenance.ControllerOptions{
		State: deps.state, Cluster: raftNode, IsLeader: raftNode.IsLeader, Logger: deps.log, Now: deps.clock,
	})
	// Responsible nodes of a monitor (ADR 0017); shared by the aggregator and the assignment syncer.
	assigner := scheduler.Assigner{Capability: scheduler.Capabilities(checker.Default)}
	// The leader decides which voters are impaired in their connectivity (ADR 0022).
	// Its deviation statistics come from the aggregator, which is created after it.
	assessor := selfhealth.New(selfhealth.Options{
		NodeID: deps.creds.nodeID, IsLeader: raftNode.IsLeader, Registry: deps.state, Cluster: raftNode,
		Self: deps.prober.selfProber.Status, Peers: deps.host.PeerSelfChecks,
		Deviation: func(sinceMs int64) map[string]aggregator.DeviationStats {
			if a := deps.aggPtr.Load(); a != nil {
				return a.Deviation(sinceMs)
			}
			return nil
		},
		FailRatio: deps.cfg.SelfHealthFailRatio, Now: deps.clock, Logger: deps.log,
	})
	registry := func(ctx context.Context) ([]store.Node, error) { return deps.state.ListNodes(ctx) }
	// Reconciliation after the quorum returned (ADR 0024, sections 6 and 7). The
	// gap job is created after the aggregator, which it re-evaluates with.
	var gapJob *degraded.GapJob
	// The aggregator evaluates only on the leader, from the heartbeats it observes (ADR 0021).
	// It waits for the gap job of the leadership: the history must be corrected first (ADR 0024).
	agg := aggregator.New(aggregator.Options{
		State: deps.state, Metrics: deps.metrics, Cluster: raftNode, NodeID: deps.creds.nodeID,
		IsLeader:    func() bool { return raftNode.IsLeader() && gapJob != nil && gapJob.Done() },
		Registry:    registry,
		Assigner:    assigner,
		Exclude:     assessor.Exclude,
		Maintenance: maint.InMaintenance, Now: deps.clock, Logger: deps.log,
	})
	deps.aggPtr.Store(agg)
	heartbeats.SetOnStored(agg.Observe)

	// Degraded mode (ADR 0024): without a leader, the reachable voter with the
	// smallest ID of the partition decides and sends the alerts itself.
	selfCheckFailed := func() bool {
		st := deps.prober.selfProber.Status()
		return st.Configured && st.Failed
	}
	detector := degraded.New(degraded.Options{
		NodeID: deps.creds.nodeID, LeaderID: deps.host.LeaderID, CaughtUp: deps.host.CaughtUp,
		Voters:          func(ctx context.Context) ([]string, error) { return listVoterIDs(ctx, deps.state) },
		Peers:           deps.host.PeerViews,
		SelfCheckFailed: selfCheckFailed,
		// 0 in the configuration disables the degraded mode; a negative value does so for the detector.
		After: disabledIfZero(deps.cfg.DegradedAfter), Now: deps.clock, Logger: deps.log,
	})
	overlay := degraded.NewOverlay(deps.state)
	localDispatcher := alert.NewLocalDispatcher(alert.LocalDispatcherOptions{
		Sender: sender, BundleWindow: deps.cfg.AlertBundleWindow, Now: deps.clock, Logger: deps.log, OnOutcome: outcomes,
		// Degraded deliveries of suspended tenants are dropped (ADR 0029).
		Suspended: func(tenantID string) bool {
			t, err := deps.state.GetTenant(ctx, tenantID)
			return err == nil && t.Suspended && t.ID != store.SystemTenantID
		},
	})
	degradedApplier := degraded.NewApplier(degraded.ApplierOptions{
		NodeID: deps.creds.nodeID, State: deps.state, Overlay: overlay, Metrics: deps.metrics, Detector: detector.State,
		SelfCheckFailed: selfCheckFailed, Keyring: deps.keyring, Dispatcher: localDispatcher,
		BundleWindow: deps.cfg.AlertBundleWindow, Now: deps.clock, Logger: deps.log,
	})
	// The degraded aggregator evaluates the same slots as the live one, but
	// only on the sender, and records the statuses in the overlay (ADR 0024, section 3).
	degradedAgg := aggregator.New(aggregator.Options{
		State: overlay, Metrics: deps.metrics, Cluster: degradedApplier, NodeID: deps.creds.nodeID,
		IsLeader: detector.IsSender,
		Registry: registry, Assigner: assigner,
		Maintenance: maint.InMaintenance, Now: deps.clock, Logger: deps.log.With("component", "degraded"),
	})
	deps.degradedAggPtr.Store(degradedAgg)
	heartbeats.SetOnStored(degradedAgg.Observe)
	reporter := degraded.NewReporter(degraded.ReporterOptions{
		NodeID: deps.creds.nodeID, Detector: detector.State, LeaderID: deps.host.LeaderID, CaughtUp: deps.host.CaughtUp,
		CommandLevel: raftNode.CommandLevel, Proposer: raftNode, Metrics: deps.metrics, Now: deps.clock, Logger: deps.log,
	})
	gapJob = degraded.NewGapJob(degraded.GapOptions{
		NodeID: deps.creds.nodeID, State: deps.state, Reevaluator: agg, Proposer: raftNode, IsLeader: raftNode.IsLeader,
		CommandLevel: raftNode.CommandLevel, Replication: deps.host.Replication.Status, Peers: deps.host.PeerViews,
		Trigger: deps.host.Replication.Trigger, Degraded: func() bool { return detector.State().Degraded },
		StartLastSeen: deps.startLastSeen,
		Now:           deps.clock, Logger: deps.log,
	})
	// The sender of a degraded period announces the lost quorum to the system channels (ADR 0025, section 5).
	quorumNotifier := degraded.NewQuorumNotifier(degraded.QuorumOptions{
		NodeID: deps.creds.nodeID, State: deps.state, Keyring: deps.keyring, Dispatcher: localDispatcher, Detector: detector.State,
		SelfCheckFailed: selfCheckFailed, Now: deps.clock, Logger: deps.log,
	})
	// The system alerts of the leader (ADR 0025, sections 3 and 4).
	alertCollector := sysalert.New(sysalert.Options{
		NodeID: deps.creds.nodeID, IsLeader: raftNode.IsLeader, State: deps.state, CommandLevel: raftNode.CommandLevel,
		Cluster: raftNode, Self: deps.host.SelfSignals, Peers: deps.host.PeerSignals, ClockOffsets: deps.host.ClockOffsets,
		Thresholds: sysalert.Thresholds{
			ReplicationLag: deps.cfg.ReplicationLagAlert, ClockSkew: deps.cfg.ClockSkewAlert,
			DiskFree: deps.cfg.DiskFreeAlert, WALSize: deps.cfg.WALSizeAlert,
			ProbeOffline: deps.cfg.ProbeOfflineAlert,
		},
		Now: deps.clock, Logger: deps.log,
	})
	systemJobs := sysalert.NewJobs(sysalert.JobOptions{
		IsLeader: raftNode.IsLeader, CommandLevel: raftNode.CommandLevel, State: deps.state, Cluster: raftNode,
		Now: deps.clock, Logger: deps.log,
	})
	judge := degraded.NewJudge(degraded.JudgeOptions{
		State: deps.state, Proposer: raftNode, IsLeader: raftNode.IsLeader, CommandLevel: raftNode.CommandLevel,
		GapDone: gapJob.Done, RetentionRaw: deps.cfg.RetentionRaw, Now: deps.clock, Logger: deps.log,
	})
	sink := scheduler.Sinks{heartbeats, &stream.HeartbeatSink{Hub: deps.hub, NodeID: deps.creds.nodeID}}
	nodeMetrics = buildNodeMetrics(deps, detector)
	deps.metricsPtr.Store(nodeMetrics)
	sched := scheduler.New(scheduler.Options{
		Registry: checker.Default, Env: checkEnv, Sink: sink, Logger: deps.log, Now: deps.clock,
		Observer: nodeMetrics,
		Resolve: func(ctx context.Context, m store.Monitor) (json.RawMessage, error) {
			return configsecret.ResolveMonitor(ctx, deps.keyring, m)
		},
	})
	launchMonitoringWorkers(ctx, bg, deps, voterWorkers{
		heartbeats: heartbeats, agg: agg, detector: detector, overlay: overlay,
		localDispatcher: localDispatcher, degradedAgg: degradedAgg, reporter: reporter,
		gapJob: gapJob, quorumNotifier: quorumNotifier, alertCollector: alertCollector,
		systemJobs: systemJobs, judge: judge, assessor: assessor, dispatcher: dispatcher,
		maint: maint, sched: sched, nodeMetrics: nodeMetrics, assigner: assigner,
	})
	return buildAPIDeps(deps, checkEnv, sender, detector), nodeMetrics, nil
}

func launchMonitoringWorkers(ctx context.Context, bg *sync.WaitGroup, deps voterRuntimeDeps, w voterWorkers) {
	raftNode := deps.host.Node
	bg.Go(func() { w.heartbeats.Run(ctx) })
	bg.Go(func() { w.agg.Run(ctx) })
	bg.Go(func() { w.detector.Run(ctx) })
	bg.Go(func() { w.localDispatcher.Run(ctx) })
	bg.Go(func() { w.degradedAgg.Run(ctx) })
	bg.Go(func() { runOverlayResetter(ctx, w.detector, w.overlay) })
	bg.Go(func() { w.reporter.Run(ctx) })
	bg.Go(func() { w.quorumNotifier.Run(ctx) })
	bg.Go(func() { w.alertCollector.Run(ctx) })
	bg.Go(func() { w.systemJobs.Run(ctx) })
	bg.Go(func() { w.gapJob.Run(ctx) })
	bg.Go(func() { w.judge.Run(ctx) })
	bg.Go(func() { deps.prober.selfProber.Run(ctx) })
	bg.Go(func() { w.assessor.Run(ctx) })
	bg.Go(func() { w.dispatcher.Run(ctx) })
	bg.Go(func() { w.maint.Run(ctx) })
	// Ends flapping periods after 30 min without a status change (FA-ALR-06, ADR 0023).
	flapping := alert.NewFlappingWatcher(alert.FlappingWatcherOptions{
		State: deps.state, Cluster: raftNode, IsLeader: raftNode.IsLeader, Now: deps.clock, Logger: deps.log,
	})
	bg.Go(func() { flapping.Run(ctx) })
	bg.Go(func() {
		configsecret.RunMigration(ctx, configsecret.MigrationOptions{
			State: deps.state, Cluster: raftNode, Keyring: deps.keyring, Schemas: &configsecret.Schemas{Registry: checker.Default},
			IsLeader: raftNode.IsLeader, Logger: deps.log,
		})
	})
	maintenanceJob := uptime.NewJob(uptime.JobOptions{
		State: deps.state, Metrics: deps.metrics, Logger: deps.log, Compaction: deps.cfg.RawCompaction,
		Retention: uptime.Retention{Raw: deps.cfg.RetentionRaw, Hourly: deps.cfg.RetentionHourly, Daily: deps.cfg.RetentionDaily},
		Vacuum:    func(ctx context.Context) error { return deps.metrics.DB().IncrementalVacuum(ctx, 0) },
		// Late heartbeats of other voters are waited for before rollup and compaction (FA-REP-03).
		ReplicatedUntil: deps.host.Replication.ReplicatedUntil,
		Now:             deps.clock,
	})
	bg.Go(func() { maintenanceJob.Run(ctx, time.Minute) })
	bg.Go(func() { w.sched.Run(ctx) })
	// Metrics of deleted tenants are purged on every node; the first cycle also requests a snapshot (ADR 0029).
	purge := tenantpurge.New(tenantpurge.Options{
		State: deps.state, Metrics: deps.metrics, Tombstones: deps.tombstones, Snapshot: raftNode.Snapshot,
		Interval: tenantpurge.DefaultInterval, Now: deps.clock, Logger: deps.log,
	})
	bg.Go(func() { purge.Run(ctx) })
	if deps.cfg.MetricsAddr != "" {
		// The endpoint is optional; if it cannot listen, the node keeps running without it.
		bg.Go(func() {
			if err := w.nodeMetrics.Serve(ctx, deps.cfg.MetricsAddr, deps.creds.metricsToken, deps.log); err != nil {
				deps.log.Error("metrics endpoint failed", "addr", deps.cfg.MetricsAddr, "err", err)
			}
		})
	}
	// The scheduler runs only the monitors assigned to this node; the aggregator
	// keeps all monitors, because it tracks results of other nodes too (ADR 0017).
	// The degraded aggregator needs the same monitors, or it never evaluates (ADR 0024).
	assigned := scheduler.AssignmentSyncer{
		Target: w.sched, NodeID: deps.creds.nodeID, Assigner: w.assigner,
		Registry: func() ([]store.Node, error) { return deps.state.ListNodes(ctx) }, Log: deps.log,
	}
	// Suspended tenants rest their checks and their aggregation; their monitors are not evaluated (ADR 0029).
	tenantsOf := func() ([]store.Tenant, error) { return deps.state.ListTenants(ctx) }
	bg.Go(func() {
		scheduler.Reconcile(ctx, deps.state, raftNode.FSM().Changes(), reconcileInterval, deps.log,
			scheduler.TenantGate{Target: assigned, Tenants: tenantsOf, Log: deps.log},
			scheduler.TenantGate{Target: w.agg, Tenants: tenantsOf, Log: deps.log},
			scheduler.TenantGate{Target: w.degradedAgg, Tenants: tenantsOf, Log: deps.log})
	})
}

func buildAPIDeps(deps voterRuntimeDeps, checkEnv *checker.Env, sender *alert.Sender, detector *degraded.Detector) api.Deps {
	tickets := auth.NewTickets()
	tickets.Now = deps.clock
	return api.Deps{
		State: deps.state, Metrics: deps.metrics, Cluster: deps.host.Node, Registry: checker.Default,
		CheckEnv: checkEnv, NodeID: deps.creds.nodeID,
		Keyring: deps.keyring, Alerts: sender,
		Degraded: detector,
		Stream:   deps.hub, Tickets: tickets,
		ClusterAdmin: clusterAdmin{host: deps.host},
		Replication:  deps.host.Replication,
		ClockOffsets: deps.host.ClockOffsets,
		Now:          deps.clock,
		SSRFPolicy:   deps.prober.policy,
		// The SSRF setting of each node in the cluster view (ADR 0030).
		SSRFProtectionByNode: deps.host.SSRFProtectionByNode,
		ProbeSessions: func() map[string]api.ProbeSessionView {
			raw := deps.host.ProbeSessionViews()
			out := make(map[string]api.ProbeSessionView, len(raw))
			for id, v := range raw {
				out[id] = api.ProbeSessionView{ConnectedVoterID: v.ConnectedVoterID, LastMessageMs: v.LastMessageMs, Buffer: v.Buffer}
			}
			return out
		},
	}
}

func buildNodeMetrics(deps voterRuntimeDeps, detector *degraded.Detector) *ucmetrics.Metrics {
	raftNode := deps.host.Node
	// Prometheus metrics (ADR 0025, section 7). The sources are read on each scrape.
	return ucmetrics.New(ucmetrics.Sources{
		RaftState: func() string { return raftNode.Status().RaftState },
		RaftIndexes: func() (applied, commit uint64, lastContact time.Duration) {
			st := raftNode.Status()
			return st.AppliedIndex, st.CommitIndex, time.Duration(st.LastContactMs) * time.Millisecond
		},
		ReplicationLag:     func() map[string]time.Duration { return collectReplicationLag(deps.host, deps.clock) },
		ReplicationDropped: func() map[string]uint64 { return collectReplicationDropped(deps.host) },
		DBSizes: func() map[string]struct{ Size, WAL int64 } {
			return map[string]struct{ Size, WAL int64 }{
				"state":   dbFileSizes(deps.state.DB()),
				"metrics": dbFileSizes(deps.metrics.DB()),
			}
		},
		DiskFree:           func() (float64, error) { return diskstat.FreeRatio(deps.cfg.DataDir) },
		OutboxPending:      func(ctx context.Context) (int, error) { return deps.state.CountPendingAlerts(ctx) },
		Degraded:           func() bool { return detector.State().Degraded },
		SystemAlertsActive: func(ctx context.Context) (map[string]int, error) { return collectActiveSystemAlerts(ctx, deps.state) },
		ClockOffsets:       func() map[string]time.Duration { return collectClockOffsets(deps.host) },
		Version:            version.Version,
	})
}

func collectReplicationLag(host *clusterhost.Host, clock func() time.Time) map[string]time.Duration {
	now := clock()
	lags := make(map[string]time.Duration)
	for _, p := range host.Replication.Status() {
		lag, ok := p.Lag(now)
		if !ok {
			lag = -time.Nanosecond // negative = the peer never caught up
		}
		lags[p.ID] = lag
	}
	return lags
}

func collectReplicationDropped(host *clusterhost.Host) map[string]uint64 {
	dropped := make(map[string]uint64)
	for _, p := range host.Replication.Status() {
		dropped[p.ID] = p.Dropped
	}
	return dropped
}

func collectActiveSystemAlerts(ctx context.Context, state *sqlite.StateStore) (map[string]int, error) {
	alerts, err := state.ListSystemAlerts(ctx, true)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(sysalert.Kinds))
	for _, kind := range sysalert.Kinds {
		counts[kind] = 0
	}
	for _, a := range alerts {
		counts[a.Kind]++
	}
	return counts, nil
}

func collectClockOffsets(host *clusterhost.Host) map[string]time.Duration {
	offsets := make(map[string]time.Duration)
	for id, ms := range host.ClockOffsets() {
		offsets[id] = time.Duration(ms) * time.Millisecond
	}
	return offsets
}

func listVoterIDs(ctx context.Context, state *sqlite.StateStore) ([]string, error) {
	nodes, err := state.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var voters []string
	for _, n := range nodes {
		if n.Role == store.NodeRoleVoter {
			voters = append(voters, n.ID)
		}
	}
	return voters, nil
}

func runOverlayResetter(ctx context.Context, detector *degraded.Detector, overlay *degraded.Overlay) {
	// Leaving the sender role drops the statuses decided locally (ADR 0024, section 3).
	for {
		select {
		case <-ctx.Done():
			return
		case <-detector.Changes():
			if !detector.IsSender() {
				overlay.Reset()
			}
		}
	}
}

func configureVoterAuth(ctx context.Context, bg *sync.WaitGroup, deps voterRuntimeDeps, nodeMetrics *ucmetrics.Metrics, apiDeps *api.Deps) (func() error, error) {
	raftNode := deps.host.Node
	readiness := raftNode.Ready
	switch {
	case deps.local != nil:
		apiDeps.Local, apiDeps.Authenticator = deps.local, deps.local
	case deps.cfg.AuthMode == config.AuthOIDC:
		keys, authn, err := newOIDC(deps.cfg, deps.log, deps.state, deps.clock, nodeMetrics, raftNode)
		if err != nil {
			return nil, fmt.Errorf("oidc: %w", err)
		}
		apiDeps.Authenticator = authn
		readiness = oidcReadiness(readiness, keys)
		bg.Go(func() { runOIDCKeys(ctx, keys, deps.log) })
		bg.Go(func() { authswitch.BindPlatformOrg(ctx, deps.cfg, deps.log, raftNode, deps.state) })
		// Local refresh sessions must not survive the switch (ADR 0032).
		bg.Go(func() {
			authswitch.RevokeLocalSessions(ctx, deps.log, raftNode, deps.state, authswitch.SessionInterval)
		})
		bg.Go(func() {
			authswitch.RunStartupHints(ctx, deps.cfg, deps.log, raftNode, deps.state, deps.clock, authswitch.StartupHintsInterval)
		})
		if deps.cfg.OIDC.AutoProvision {
			bg.Go(func() { watchProvisionPlan(ctx, deps.log, deps.state, deps.cfg.OIDC.AutoProvisionPlan) })
		}
	}
	// API tokens work in every auth mode (ADR 0031). Without an
	// authenticator every request is denied, and tokens must not change that.
	if apiDeps.Authenticator != nil {
		apiDeps.Authenticator = auth.WithAPITokens(apiDeps.Authenticator, deps.state, deps.clock)
	}
	return readiness, nil
}

// walSize returns the size of the write-ahead log of db in bytes, 0 if unknown.
func walSize(db *sqlite.DB) int64 {
	n, err := db.WALSize()
	if err != nil {
		return 0
	}
	return n
}

// dbFileSizes returns the size of the database file and of its WAL in bytes.
// A file that cannot be stat'ed counts as 0; the metric then shows the drop.
func dbFileSizes(db *sqlite.DB) struct{ Size, WAL int64 } {
	var sizes struct{ Size, WAL int64 }
	if info, err := os.Stat(db.Path()); err == nil {
		sizes.Size = info.Size()
	}
	if wal, err := db.WALSize(); err == nil {
		sizes.WAL = wal
	}
	return sizes
}

// disabledIfZero maps the configured DEGRADED_AFTER to the detector option: a
// zero duration disables the degraded mode, which the detector expresses as a
// negative value (its zero value means the default).
func disabledIfZero(after time.Duration) time.Duration {
	if after == 0 {
		return -1
	}
	return after
}

// bootstrapCluster creates the default tenant and the first data key once a
// leader exists, then loads the keyring. A master key that does not match the
// cluster stops the node (ADR 0011). It reports whether the keys are
// available, which everything that encrypts depends on.
func bootstrapCluster(ctx context.Context, log *slog.Logger, n *cluster.Node,
	state *sqlite.StateStore, kek *secrets.KEK, keyring *secrets.Keyring, local *auth.Local, stop context.CancelCauseFunc) bool {
	bootCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Every mode needs the default tenant: local users, the generic OIDC
	// profile and the platform organization of the zitadel profile (ADR 0027).
	if err := cluster.EnsureDefaultTenant(bootCtx, n, state); err != nil && ctx.Err() == nil {
		log.Error("could not create the default tenant", "err", err)
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
	// Only the leader creates keys and the setup token. A voter that just
	// joined may not have caught up yet: it would see no key and no user.
	// It loads the leader's key through the key maintenance instead.
	if !n.IsLeader() {
		if err := local.ReloadKeys(bootCtx); err != nil && ctx.Err() == nil {
			log.Warn("could not load the access token signing keys yet", "err", err)
		}
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

func (a clusterAdmin) CreateJoinToken(ctx context.Context, role pki.Role, tenantID string, ttl time.Duration, actor *pb.Actor) (string, time.Time, error) {
	return a.host.Authority.CreateJoinTokenFor(ctx, role, tenantID, ttl, actor)
}

func (a clusterAdmin) Remove(ctx context.Context, nodeID, requestedBy string) (membership.Removal, error) {
	return a.host.Manager.Remove(ctx, nodeID, requestedBy)
}

func (a clusterAdmin) Members() ([]cluster.Member, uint64, error) { return a.host.Node.Members() }

func (a clusterAdmin) Leader() (address, id string) { return a.host.Node.Leader() }

func (a clusterAdmin) ID() string { return a.host.Node.ID() }

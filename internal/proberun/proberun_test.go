// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package proberun

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusterhost"
	"github.com/joinsider/pulsequorum/internal/config"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// freeAddr returns a loopback address that was free at the time of the call.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// startVoter starts a single bootstrapped voter in-process, the way
// cmd/pulsequorum does, and returns it with its cluster address.
func startVoter(ctx context.Context, t *testing.T) (*clusterhost.Host, string) {
	t.Helper()
	dir := t.TempDir()
	addr := freeAddr(t)
	nodeID := uuid.NewString()
	log := slog.New(slog.DiscardHandler)

	prepared, err := clusterhost.Prepare(ctx, dir, nodeID, "voter", addr, "test", clusterhost.JoinOptions{}, log)
	if err != nil {
		t.Fatal(err)
	}
	kek, err := secrets.LoadKEK(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.CheckKEK(kek); err != nil {
		t.Fatal(err)
	}
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	keyring := secrets.NewKeyring(kek, state.ListSecretKeys)
	host, err := clusterhost.Start(ctx, clusterhost.Options{
		NodeID: nodeID, ListenAddr: addr, Advertise: addr, DataDir: dir, Timeout: 300 * time.Millisecond,
		State: state, Metrics: metrics,
		KEK: kek, Keyring: keyring, Prepared: prepared, Log: log, RaftLogLevel: "error",
		OnFatal: func(err error) { t.Errorf("voter halted: %v", err) },
		Name:    "voter", Version: "test",
	})
	if err != nil {
		_ = state.Close()
		_ = metrics.Close()
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	go host.Run(runCtx)
	t.Cleanup(func() {
		cancel()
		_ = host.Close()
		_ = state.Close()
		_ = metrics.Close()
	})

	if err := cluster.EnsureDefaultTenant(ctx, host.Node, state); err != nil {
		t.Fatal(err)
	}
	if err := cluster.EnsureSecretKey(ctx, host.Node, state, kek); err != nil {
		t.Fatal(err)
	}
	if err := keyring.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "voter becomes leader", 15*time.Second, host.Node.IsLeader)
	// Tenant and tenant-bound probe commands need the cluster command level
	// to reach the current version, which a single voter reaches on its own.
	eventually(t, "command level reaches the current version", 60*time.Second, func() bool {
		level, err := host.Node.CommandLevel(ctx)
		return err == nil && level >= cluster.CommandVersion
	})
	return host, addr
}

// createTenant stores a tenant through the voter's state machine.
func createTenant(ctx context.Context, t *testing.T, voter *clusterhost.Host, id string) {
	t.Helper()
	cmd := &pb.Command{
		RequestId: "tenant-" + id + "-" + uuid.NewString(),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-platform", Name: "Platform"},
		Payload: &pb.Command_CreateTenant{CreateTenant: &pb.CreateTenant{
			TenantId:   id,
			Name:       id,
			PlanId:     "default",
			LimitsJson: "{}",
			Source:     "admin",
		}},
	}
	res, err := voter.Node.Apply(ctx, cmd)
	if err != nil || res.Err != nil {
		t.Fatalf("create tenant %s: err=%v cmdErr=%v", id, err, res.Err)
	}
}

// eventually polls cond every 50 ms until it holds or timeout passes.
func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncBuffer collects the probe log of a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// probeLogger returns a logger whose output is shown only if the test fails.
func probeLogger(t *testing.T) *slog.Logger {
	t.Helper()
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("probe log:\n%s", logs.String())
		}
	})
	return slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// probeEnv builds the configuration of a probe from an environment map.
func probeEnv(t *testing.T, seed, token, name string, extra map[string]string) config.Config {
	t.Helper()
	env := map[string]string{
		"NODE_NAME":          name,
		"NODE_ROLE":          "probe",
		"DATA_DIR":           t.TempDir(),
		"HTTP_ADDR":          freeAddr(t),
		"CLUSTER_SEEDS":      seed,
		"CLUSTER_JOIN_TOKEN": token,
		"LOG_FORMAT":         "text",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// probeRun is a Run started in the background.
type probeRun struct {
	cancel context.CancelFunc
	done   chan error
}

func startProbe(ctx context.Context, t *testing.T, cfg config.Config) *probeRun {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	pr := &probeRun{cancel: cancel, done: make(chan error, 1)}
	log := probeLogger(t)
	go func() { pr.done <- Run(ctx, cfg, log) }()
	return pr
}

// waitReady polls /readyz of the probe until it answers 200.
func waitReady(t *testing.T, pr *probeRun, addr string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-pr.done:
			t.Fatalf("Run returned before the probe was ready: %v", err)
		case <-ticker.C:
		}
		resp, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe not ready on /readyz within %s (last error: %v)", timeout, err)
		}
	}
}

// stopProbe cancels the probe and checks that Run returns nil in time.
func stopProbe(t *testing.T, pr *probeRun) {
	t.Helper()
	pr.cancel()
	select {
	case err := <-pr.done:
		if err != nil {
			t.Fatalf("Run after cancel: %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s after cancel")
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voter, seed := startVoter(ctx, t)

	t.Run("public probe becomes ready and stops cleanly", func(t *testing.T) {
		token, _, err := voter.Authority.CreateJoinToken(ctx, pki.RoleProbe, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		cfg := probeEnv(t, seed, token, "probe-public", nil)
		pr := startProbe(ctx, t, cfg)
		waitReady(t, pr, cfg.HTTPAddr, 30*time.Second)
		stopProbe(t, pr)
	})

	t.Run("private probe without PROBE_ALLOW_TARGETS is refused", func(t *testing.T) {
		createTenant(ctx, t, voter, "tenant-refuse")
		actor := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
		token, _, err := voter.Authority.CreateJoinTokenFor(ctx, pki.RoleProbe, "tenant-refuse", time.Hour, actor)
		if err != nil {
			t.Fatal(err)
		}
		cfg := probeEnv(t, seed, token, "probe-private-refused", nil)

		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
		defer runCancel()
		err = Run(runCtx, cfg, probeLogger(t))
		if err == nil {
			t.Fatal("Run of a private probe without PROBE_ALLOW_TARGETS returned nil, want an error")
		}
		const want = "PROBE_ALLOW_TARGETS is required for a private probe"
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("private probe with targets and minimal detail becomes ready", func(t *testing.T) {
		createTenant(ctx, t, voter, "tenant-ready")
		actor := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
		token, _, err := voter.Authority.CreateJoinTokenFor(ctx, pki.RoleProbe, "tenant-ready", time.Hour, actor)
		if err != nil {
			t.Fatal(err)
		}
		cfg := probeEnv(t, seed, token, "probe-private", map[string]string{
			"PROBE_ALLOW_TARGETS": "10.0.0.0/8:443",
			"PROBE_RESULT_DETAIL": "minimal",
		})
		pr := startProbe(ctx, t, cfg)
		waitReady(t, pr, cfg.HTTPAddr, 30*time.Second)
		stopProbe(t, pr)
	})

	t.Run("unregistered monitor type is rejected", func(t *testing.T) {
		token, _, err := voter.Authority.CreateJoinToken(ctx, pki.RoleProbe, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		cfg := probeEnv(t, seed, token, "probe-bad-types", map[string]string{
			"PROBE_ALLOW_MONITOR_TYPES": "http,nosuchtype",
		})

		runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
		defer runCancel()
		err = Run(runCtx, cfg, probeLogger(t))
		if err == nil {
			t.Fatal("Run with an unregistered monitor type returned nil, want an error")
		}
		if !strings.Contains(err.Error(), "PROBE_ALLOW_MONITOR_TYPES") {
			t.Fatalf("Run error = %q, want it to mention PROBE_ALLOW_MONITOR_TYPES", err)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

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
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/checker/httpcheck"
	"github.com/joinsider/pulsequorum/internal/checker/icmpcheck"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/diskstat"
	ucmetrics "github.com/joinsider/pulsequorum/internal/metrics"
	"github.com/joinsider/pulsequorum/internal/node"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/probe"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
	"github.com/joinsider/pulsequorum/internal/version"
)

func runProbe(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	clock := time.Now
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	nodeID, err := node.LoadOrCreateID(cfg.DataDir)
	if err != nil {
		return err
	}
	metrics, err := sqlite.OpenMetrics(ctx, cfg.DataDir, sqlite.MetricsOptions{
		ReadConns:    1,
		CacheSizeKiB: sqlite.ProbeMetricsCacheKiB,
	})
	if err != nil {
		return err
	}
	defer func() { _ = metrics.Close() }()
	log.Info("probe storage ready", "data_dir", cfg.DataDir, "node_id", nodeID, "incarnation", metrics.Incarnation())

	icmpMode, capabilities := detectProbeCapabilities(ctx, cfg, log)
	joinToken, err := readJoinToken(cfg)
	if err != nil {
		return err
	}
	metricsToken, err := cfg.ResolveMetricsToken()
	if err != nil {
		return err
	}

	cert, voters, err := probe.Prepare(ctx, probe.PrepareOptions{
		DataDir:      cfg.DataDir,
		NodeID:       nodeID,
		Name:         cfg.NodeName,
		Seeds:        cfg.ClusterSeeds,
		Token:        joinToken,
		Region:       cfg.NodeRegion,
		Labels:       cfg.NodeLabels,
		Capabilities: capabilities,
		Version:      version.Version,
		Incarnation:  metrics.Incarnation(),
		Log:          log,
		Clock:        clock,
	})
	if err != nil {
		return err
	}

	isPrivate := false
	if leaf := cert.Leaf(); leaf != nil {
		if id, err := pki.IdentityOf(leaf); err == nil {
			isPrivate = id.TenantID != ""
		}
	}
	// ADR 0035 section 4: for private probes auto=off; for public probes
	// remote ProbeMonitor.ssrf_protection enables protection dynamically.
	policy := probe.NewPolicy(cfg.SSRFProtection == "on", isPrivate)
	checkEnv := &checker.Env{
		Dialer:    &checker.Dialer{Policy: policy},
		UserAgent: "Pulsequorum/" + version.Version,
		ICMPMode:  icmpMode,
		Now:       clock,
	}

	heartbeats, err := scheduler.NewHeartbeatWriter(ctx, metrics, nodeID, log)
	if err != nil {
		return err
	}
	ackRetain := cfg.ProbeAckRetain
	if ackRetain == 0 {
		ackRetain = time.Nanosecond
	}
	buf, err := probe.NewBuffer(ctx, probe.BufferOptions{
		Store:     metrics,
		NodeID:    nodeID,
		AckRetain: ackRetain,
		MaxAge:    cfg.ProbeBufferMaxAge,
		MaxBytes:  cfg.ProbeBufferMaxBytes,
		Now:       clock,
		Log:       log,
	})
	if err != nil {
		return err
	}

	nodeMetrics := ucmetrics.New(ucmetrics.Sources{
		DBSizes: func() map[string]struct{ Size, WAL int64 } {
			return map[string]struct{ Size, WAL int64 }{
				"metrics": dbFileSizes(metrics.DB()),
			}
		},
		DiskFree: func() (float64, error) { return diskstat.FreeRatio(cfg.DataDir) },
		Version:  version.Version,
	})

	sched := scheduler.New(scheduler.Options{
		Registry: checker.Default,
		Env:      checkEnv,
		Sink:     heartbeats,
		Logger:   log,
		Observer: nodeMetrics,
		Now:      clock,
	})

	client, err := probe.NewClient(probe.ClientOptions{
		NodeID:         nodeID,
		Name:           cfg.NodeName,
		Region:         cfg.NodeRegion,
		Labels:         cfg.NodeLabels,
		Capabilities:   capabilities,
		Version:        version.Version,
		Cert:           cert,
		Voters:         voters,
		Buffer:         buf,
		Metrics:        metrics,
		Sync:           sched,
		Policy:         policy,
		SessionTimeout: cfg.ProbeSessionTimeout,
		Clock:          clock,
		Log:            log,
	})
	if err != nil {
		return err
	}
	heartbeats.SetOnStored(client.NotifyNewHeartbeats)

	var bg sync.WaitGroup
	defer func() {
		stop(nil)
		bg.Wait()
	}()

	bg.Go(func() { metrics.DB().RunCheckpointer(ctx, checkpointInterval, log) })
	bg.Go(func() { heartbeats.Run(ctx) })
	bg.Go(func() { sched.Run(ctx) })
	bg.Go(func() { buf.RunCleanup(ctx, time.Minute) })
	if cfg.MetricsAddr != "" {
		bg.Go(func() {
			if err := nodeMetrics.Serve(ctx, cfg.MetricsAddr, metricsToken, log); err != nil {
				log.Error("probe metrics endpoint failed", "addr", cfg.MetricsAddr, "err", err)
			}
		})
	}
	bg.Go(func() {
		if err := serveProbeHTTP(ctx, cfg, client.Ready, log); err != nil && ctx.Err() == nil {
			log.Error("probe HTTP health server failed", "addr", cfg.HTTPAddr, "err", err)
			stop(err)
		}
	})

	err = client.Run(ctx)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func detectProbeCapabilities(ctx context.Context, cfg config.Config, log *slog.Logger) (string, []string) {
	icmpMode, icmpErr := icmpcheck.DetectMode(ctx, cfg.ICMPMode)
	if icmpMode == icmpcheck.ModeOff && cfg.ICMPMode != icmpcheck.ModeOff {
		log.Warn("ICMP checks are unavailable on this probe; see docs/betrieb/icmp.md", "configured", cfg.ICMPMode, "err", icmpErr)
	} else {
		log.Info("ICMP mode", "mode", icmpMode)
	}
	capabilities := []string{httpcheck.ClientCertCapability}
	if icmpMode != icmpcheck.ModeOff {
		capabilities = append(capabilities, icmpcheck.Capability)
	}
	return icmpMode, capabilities
}

func serveProbeHTTP(ctx context.Context, cfg config.Config, ready func() error, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeProbeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := ready(); err != nil {
			writeProbeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
			return
		}
		writeProbeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	log.Info("probe health endpoint listening", "addr", ln.Addr().String())

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func writeProbeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

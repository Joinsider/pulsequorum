// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package metrics exposes the Prometheus metrics of a node (FA-OBS-02, ADR 0025
// section 7). Every node has its own registry; no collector is global. The
// metrics carry no tenant or monitor labels.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Sources are the read-only views the collector takes on each scrape. A nil
// source means its metrics are not exported. Sources that return an error
// are left out of that scrape.
type Sources struct {
	// RaftState returns the Raft state, e.g. "Leader" or "Follower".
	RaftState func() string
	// RaftIndexes returns the applied and commit index and the time since the last leader contact.
	RaftIndexes func() (applied, commit uint64, lastContact time.Duration)
	// ReplicationLag maps peer to its lag; a negative value means it never caught up.
	ReplicationLag func() map[string]time.Duration
	// ReplicationDropped maps peer to the number of dropped replication messages.
	ReplicationDropped func() map[string]uint64
	// DBSizes maps the database ("state", "metrics") to its file and WAL size in bytes.
	DBSizes func() map[string]struct{ Size, WAL int64 }
	// DiskFree returns the free ratio of the data directory's file system.
	DiskFree func() (float64, error)
	// OutboxPending returns the number of undelivered notifications. It may block up to the scrape timeout.
	OutboxPending func(ctx context.Context) (int, error)
	// Degraded reports whether the node is in degraded mode (ADR 0024).
	Degraded func() bool
	// SystemAlertsActive maps the alert kind to the number of active system alerts.
	SystemAlertsActive func(ctx context.Context) (map[string]int, error)
	// ClockOffsets maps peer to its clock offset (ADR 0020).
	ClockOffsets func() map[string]time.Duration
	// Version is the build version exported in uptimecluster_build_info.
	Version string
}

// scrapeTimeout bounds each context-aware source during one scrape.
const scrapeTimeout = 5 * time.Second

// Buckets for uptimecluster_check_duration_seconds, from 10 ms to 30 s.
var checkBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Metrics holds the push-side instruments and the scrape-time collector.
// It implements CheckObserver.
type Metrics struct {
	reg *prometheus.Registry

	checks          *prometheus.CounterVec
	checkDuration   *prometheus.HistogramVec
	checksRunning   prometheus.Gauge
	alertDeliveries *prometheus.CounterVec
	jwksFailures    prometheus.Counter
	userInfoCalls   prometheus.Counter
	userInfoFails   prometheus.Counter
}

// CheckObserver receives check start and finish events from the scheduler.
type CheckObserver interface {
	CheckStarted(typ string)
	CheckFinished(typ, result string, d time.Duration)
}

// Compile-time check that Metrics implements CheckObserver.
var _ CheckObserver = (*Metrics)(nil)

// New creates the metrics of one node on a private registry.
func New(src Sources) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uptimecluster_checks_total",
			Help: "Finished checks by monitor type and result.",
		}, []string{"type", "result"}),
		checkDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uptimecluster_check_duration_seconds",
			Help:    "Duration of checks by monitor type.",
			Buckets: checkBuckets,
		}, []string{"type"}),
		checksRunning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "uptimecluster_checks_running",
			Help: "Checks currently running on this node.",
		}),
		alertDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uptimecluster_alert_deliveries_total",
			Help: "Alert deliveries by kind and outcome.",
		}, []string{"kind", "outcome"}),
		jwksFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "uptimecluster_auth_jwks_fetch_failures_total",
			Help: "Failed fetches of the identity provider's signing keys (ADR 0027).",
		}),
		userInfoCalls: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "uptimecluster_auth_userinfo_requests_total",
			Help: "Userinfo lookups at the identity provider, one per new access token (ADR 0028).",
		}),
		userInfoFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "uptimecluster_auth_userinfo_failures_total",
			Help: "Failed userinfo lookups at the identity provider (ADR 0028).",
		}),
	}
	m.reg.MustRegister(m.checks, m.checkDuration, m.checksRunning, m.alertDeliveries, m.jwksFailures, m.userInfoCalls, m.userInfoFails)
	m.reg.MustRegister(newCollector(src))
	return m
}

// CheckStarted implements CheckObserver.
func (m *Metrics) CheckStarted(string) {
	m.checksRunning.Inc()
}

// CheckFinished implements CheckObserver.
func (m *Metrics) CheckFinished(typ, result string, d time.Duration) {
	m.checksRunning.Dec()
	m.checks.WithLabelValues(typ, result).Inc()
	m.checkDuration.WithLabelValues(typ).Observe(d.Seconds())
}

// AlertDelivered counts one alert delivery. kind is the event kind and
// outcome is e.g. "sent", "failed" or "throttled".
func (m *Metrics) AlertDelivered(kind, outcome string) {
	m.alertDeliveries.WithLabelValues(kind, outcome).Inc()
}

// JWKSFetchFailed counts one failed fetch of the identity provider's keys.
func (m *Metrics) JWKSFetchFailed() {
	m.jwksFailures.Inc()
}

// UserInfoCalled counts one userinfo lookup.
func (m *Metrics) UserInfoCalled() {
	m.userInfoCalls.Inc()
}

// UserInfoFailed counts one failed userinfo lookup.
func (m *Metrics) UserInfoFailed() {
	m.userInfoFails.Inc()
}

// Handler serves the metrics of the private registry in the Prometheus text format.
// It does no authentication; Serve adds the token check.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Serve listens on addr and serves only /metrics, protected by token (see
// RequireToken). It blocks until ctx ends, then shuts down gracefully.
func (m *Metrics) Serve(ctx context.Context, addr, token string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return m.serve(ctx, ln, token, log)
}

func (m *Metrics) serve(ctx context.Context, ln net.Listener, token string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", RequireToken(token, m.Handler()))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if token == "" {
		log.Warn("metrics endpoint runs without a token; protect it at the network level", "addr", ln.Addr().String())
	}
	log.Info("metrics listening", "addr", ln.Addr().String(), "token", token != "")

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

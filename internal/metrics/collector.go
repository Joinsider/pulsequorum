// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

// raftStates are always exported, so a state change shows as a 0/1 flip.
var raftStates = []string{"Follower", "Candidate", "Leader", "Shutdown"}

// collector reads the Sources on every scrape. It is an unchecked collector:
// Describe sends nothing, so series can appear and disappear with the sources.
type collector struct {
	src Sources

	raftState, raftApplied, raftCommit, raftLastContact *prometheus.Desc
	replLag, replDropped, dbSize, dbWAL                 *prometheus.Desc
	diskFree, outboxPending, degraded                   *prometheus.Desc
	alertsActive, clockOffset, buildInfo                *prometheus.Desc
}

func newCollector(src Sources) *collector {
	return &collector{
		src:             src,
		raftState:       prometheus.NewDesc("pulsequorum_raft_state", "Raft state of this node: 1 for the current state, 0 otherwise.", []string{"state"}, nil),
		raftApplied:     prometheus.NewDesc("pulsequorum_raft_applied_index", "Last applied Raft log index.", nil, nil),
		raftCommit:      prometheus.NewDesc("pulsequorum_raft_commit_index", "Last committed Raft log index.", nil, nil),
		raftLastContact: prometheus.NewDesc("pulsequorum_raft_last_contact_seconds", "Seconds since the last contact with the leader (0 on the leader).", nil, nil),
		replLag:         prometheus.NewDesc("pulsequorum_replication_lag_seconds", "Replication lag to a peer in seconds; -1 if the peer never caught up.", []string{"peer"}, nil),
		replDropped:     prometheus.NewDesc("pulsequorum_replication_dropped_total", "Replication messages dropped for a peer.", []string{"peer"}, nil),
		dbSize:          prometheus.NewDesc("pulsequorum_db_size_bytes", "Size of a database file in bytes.", []string{"db"}, nil),
		dbWAL:           prometheus.NewDesc("pulsequorum_db_wal_bytes", "Size of a database write-ahead log in bytes.", []string{"db"}, nil),
		diskFree:        prometheus.NewDesc("pulsequorum_disk_free_ratio", "Free share of the data directory's file system.", nil, nil),
		outboxPending:   prometheus.NewDesc("pulsequorum_outbox_pending", "Notifications in the outbox that are not delivered yet.", nil, nil),
		degraded:        prometheus.NewDesc("pulsequorum_degraded", "1 if the node is in degraded mode, 0 otherwise.", nil, nil),
		alertsActive:    prometheus.NewDesc("pulsequorum_system_alerts_active", "Active system alerts by kind.", []string{"kind"}, nil),
		clockOffset:     prometheus.NewDesc("pulsequorum_clock_offset_seconds", "Clock offset to a peer in seconds.", []string{"peer"}, nil),
		buildInfo:       prometheus.NewDesc("pulsequorum_build_info", "Build information; always 1.", []string{"version"}, nil),
	}
}

// Describe implements prometheus.Collector. It sends nothing (unchecked collector).
func (*collector) Describe(chan<- *prometheus.Desc) {
	// Intentionally empty: unchecked collector whose series vary with Sources.
}

// Collect implements prometheus.Collector.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	s := c.src
	if s.RaftState != nil {
		current := s.RaftState()
		states := raftStates
		if !contains(states, current) {
			states = append(append([]string(nil), states...), current)
		}
		for _, st := range states {
			c.gauge(ch, c.raftState, boolValue(st == current), st)
		}
	}
	if s.RaftIndexes != nil {
		applied, commit, last := s.RaftIndexes()
		c.gauge(ch, c.raftApplied, float64(applied))
		c.gauge(ch, c.raftCommit, float64(commit))
		c.gauge(ch, c.raftLastContact, last.Seconds())
	}
	if s.ReplicationLag != nil {
		for peer, lag := range s.ReplicationLag() {
			v := lag.Seconds()
			if lag < 0 {
				v = -1
			}
			c.gauge(ch, c.replLag, v, peer)
		}
	}
	if s.ReplicationDropped != nil {
		for peer, n := range s.ReplicationDropped() {
			ch <- prometheus.MustNewConstMetric(c.replDropped, prometheus.CounterValue, float64(n), peer)
		}
	}
	if s.DBSizes != nil {
		for db, sz := range s.DBSizes() {
			c.gauge(ch, c.dbSize, float64(sz.Size), db)
			c.gauge(ch, c.dbWAL, float64(sz.WAL), db)
		}
	}
	if s.DiskFree != nil {
		if ratio, err := s.DiskFree(); err == nil {
			c.gauge(ch, c.diskFree, ratio)
		}
	}
	if s.OutboxPending != nil {
		ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
		n, err := s.OutboxPending(ctx)
		cancel()
		if err == nil {
			c.gauge(ch, c.outboxPending, float64(n))
		}
	}
	if s.Degraded != nil {
		c.gauge(ch, c.degraded, boolValue(s.Degraded()))
	}
	if s.SystemAlertsActive != nil {
		ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
		alerts, err := s.SystemAlertsActive(ctx)
		cancel()
		if err == nil {
			for kind, n := range alerts {
				c.gauge(ch, c.alertsActive, float64(n), kind)
			}
		}
	}
	if s.ClockOffsets != nil {
		for peer, off := range s.ClockOffsets() {
			c.gauge(ch, c.clockOffset, off.Seconds(), peer)
		}
	}
	version := s.Version
	if version == "" {
		version = "unknown"
	}
	c.gauge(ch, c.buildInfo, 1, version)
}

func (*collector) gauge(ch chan<- prometheus.Metric, d *prometheus.Desc, v float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

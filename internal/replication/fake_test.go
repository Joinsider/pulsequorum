// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// fakeMetrics is an in-memory store.Metrics. Methods not implemented here panic
// through the nil embedded interface.
type fakeMetrics struct {
	store.Metrics

	incarnation string

	mu          sync.Mutex
	hbs         map[streamKey]map[uint64]store.Heartbeat
	pos         map[streamKey]uint64
	fetchLimits []int
}

func newFakeMetrics(incarnation string) *fakeMetrics {
	return &fakeMetrics{
		incarnation: incarnation,
		hbs:         make(map[streamKey]map[uint64]store.Heartbeat),
		pos:         make(map[streamKey]uint64),
	}
}

func (m *fakeMetrics) Incarnation() string { return m.incarnation }

// InsertHeartbeats stores idempotently and advances the position of every
// touched stream over the contiguous seqs present, like the SQLite store.
func (m *fakeMetrics) InsertHeartbeats(_ context.Context, hbs []store.Heartbeat) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0
	touched := make(map[streamKey]bool)
	for _, h := range hbs {
		k := streamKey{node: h.NodeID, inc: h.Incarnation}
		if m.hbs[k] == nil {
			m.hbs[k] = make(map[uint64]store.Heartbeat)
		}
		if _, ok := m.hbs[k][h.Seq]; !ok {
			added++
		}
		m.hbs[k][h.Seq] = h
		touched[k] = true
	}
	for k := range touched {
		for {
			if _, ok := m.hbs[k][m.pos[k]+1]; !ok {
				break
			}
			m.pos[k]++
		}
	}
	return added, nil
}

func (m *fakeMetrics) LastSeq(_ context.Context, node, inc string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last uint64
	for s := range m.hbs[streamKey{node: node, inc: inc}] {
		last = max(last, s)
	}
	return last, nil
}

func (m *fakeMetrics) StreamPositions(context.Context) ([]store.StreamPosition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make(map[streamKey]bool)
	for k := range m.pos {
		keys[k] = true
	}
	for k := range m.hbs {
		keys[k] = true
	}
	out := make([]store.StreamPosition, 0, len(keys))
	for k := range keys {
		out = append(out, store.StreamPosition{NodeID: k.node, Incarnation: k.inc, Seq: m.pos[k]})
	}
	return out, nil
}

func (m *fakeMetrics) AdvanceStreamPosition(_ context.Context, p store.StreamPosition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := streamKey{node: p.NodeID, inc: p.Incarnation}
	if p.Seq > m.pos[k] {
		m.pos[k] = p.Seq
	}
	return nil
}

// HeartbeatsAfter returns heartbeats with after < seq <= until in seq order.
func (m *fakeMetrics) HeartbeatsAfter(_ context.Context, node, inc string, after, until uint64, limit int) ([]store.Heartbeat, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fetchLimits = append(m.fetchLimits, limit)
	k := streamKey{node: node, inc: inc}
	seqs := make([]uint64, 0, len(m.hbs[k]))
	for s := range m.hbs[k] {
		seqs = append(seqs, s)
	}
	slices.Sort(seqs)
	var out []store.Heartbeat
	for _, s := range seqs {
		if s <= after || s > until {
			continue
		}
		if len(out) == limit {
			return out, true, nil
		}
		out = append(out, m.hbs[k][s])
	}
	return out, false, nil
}

// seqs returns the stored seqs of a stream in ascending order.
func (m *fakeMetrics) seqs(node, inc string) []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := streamKey{node: node, inc: inc}
	out := make([]uint64, 0, len(m.hbs[k]))
	for s := range m.hbs[k] {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// position returns the stored position of a stream.
func (m *fakeMetrics) position(node, inc string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pos[streamKey{node: node, inc: inc}]
}

// fetchLimitsSeen returns the limits of all HeartbeatsAfter calls.
func (m *fakeMetrics) fetchLimitsSeen() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.fetchLimits)
}

// makeHB returns a valid heartbeat of the test tenant.
func makeHB(node, inc string, seq uint64) store.Heartbeat {
	return store.Heartbeat{
		NodeID:      node,
		Incarnation: inc,
		Seq:         seq,
		TenantID:    "t1",
		MonitorID:   "m1",
		TsMs:        1_000 + int64(seq), //nolint:gosec // G115: small test seqs
		Status:      store.CheckUp,
		LatencyMs:   12,
		Details:     json.RawMessage(`{"code":200}`),
	}
}

// makeHBs returns valid heartbeats of one stream for the given seqs.
func makeHBs(node, inc string, seqs ...uint64) []store.Heartbeat {
	out := make([]store.Heartbeat, 0, len(seqs))
	for _, s := range seqs {
		out = append(out, makeHB(node, inc, s))
	}
	return out
}

func mustInsert(t *testing.T, m *fakeMetrics, hbs []store.Heartbeat) {
	t.Helper()
	if _, err := m.InsertHeartbeats(context.Background(), hbs); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func seqsString(seqs []uint64) string { return fmt.Sprint(seqs) }

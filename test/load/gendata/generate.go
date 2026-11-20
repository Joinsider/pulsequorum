// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	// medianLatencyMs and latencySigma shape the log-normal latency of
	// successful checks: median 45 ms, 95th percentile about 80 ms.
	medianLatencyMs = 45.0
	latencySigma    = 0.35
	// maxDownStreakChecks bounds the length of one DOWN streak (1..n checks).
	maxDownStreakChecks = 10
	// defaultTimeoutSeconds applies if a monitor has no timeout configured.
	defaultTimeoutSeconds = 10
)

// Error classes of failed checks, the same values as internal/checker.
const (
	errorClassConnect = "connect"
	errorClassTimeout = "timeout"
)

// monitorGen is the generator state of one monitor: its next check and the
// DOWN streak in progress.
type monitorGen struct {
	tenantID   string
	monitorID  string
	intervalMs int64
	timeoutMs  int
	// next is the time of the next check in ms.
	next      int64
	downLeft  int
	downClass string
}

func newMonitorGen(m store.Monitor) (*monitorGen, error) {
	if m.IntervalSeconds <= 0 {
		return nil, fmt.Errorf("monitor %s has interval %d s", m.ID, m.IntervalSeconds)
	}
	timeout := m.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds
	}
	return &monitorGen{
		tenantID:   m.TenantID,
		monitorID:  m.ID,
		intervalMs: int64(m.IntervalSeconds) * 1000,
		timeoutMs:  timeout * 1000,
	}, nil
}

// outcome is the result of one simulated check.
type outcome struct {
	status     store.CheckStatus
	latencyMs  int
	errorClass string
	message    string
}

// outcome draws the result of the monitor's next check. A DOWN streak starts
// with probability downRate per UP check and lasts 1 to maxDownStreakChecks
// checks; it fails either by connection error or by timeout.
func (g *monitorGen) outcome(rng *rand.Rand, downRate float64) outcome {
	if g.downLeft == 0 && rng.Float64() < downRate {
		g.downLeft = 1 + rng.IntN(maxDownStreakChecks)
		if rng.IntN(2) == 0 {
			g.downClass = errorClassConnect
		} else {
			g.downClass = errorClassTimeout
		}
	}
	if g.downLeft > 0 {
		g.downLeft--
		if g.downClass == errorClassConnect {
			return outcome{status: store.CheckDown, errorClass: errorClassConnect,
				message: "dial tcp: connect: connection refused"}
		}
		return outcome{status: store.CheckDown, latencyMs: g.timeoutMs, errorClass: errorClassTimeout,
			message: fmt.Sprintf("request timed out after %d ms", g.timeoutMs)}
	}
	return outcome{status: store.CheckUp, latencyMs: latencyMs(rng, g.timeoutMs)}
}

// latencyMs draws a log-normal latency in ms, between 1 and maxMs-1.
func latencyMs(rng *rand.Rand, maxMs int) int {
	v := math.Exp(math.Log(medianLatencyMs) + latencySigma*rng.NormFloat64())
	return min(max(int(math.Round(v)), 1), max(maxMs-1, 1))
}

// windowBounds returns the range [from, to) of the i-th window of the given
// length (ms) in a history that starts at startMs.
func windowBounds(startMs int64, i int, length int64) (from, to int64) {
	from = startMs + int64(i)*length
	return from, from + length
}

// emitFunc receives the checks in time order.
type emitFunc func(g *monitorGen, tsMs int64, o outcome) error

// generateWindow emits, in time order, every check of the monitors with a time
// in [from, to) and advances the monitors to the first check after to. Ties
// are broken by monitor ID, so the output depends only on the inputs and rng.
func generateWindow(gens []*monitorGen, from, to int64, rng *rand.Rand, downRate float64, emit emitFunc) error {
	q := make(checkQueue, 0, len(gens))
	for _, g := range gens {
		if g.next < to {
			q = append(q, g)
		}
	}
	heap.Init(&q)
	for q.Len() > 0 {
		g := q[0]
		ts := g.next
		if ts < from {
			return fmt.Errorf("monitor %s: next check %d lies before the day start %d", g.monitorID, ts, from)
		}
		if err := emit(g, ts, g.outcome(rng, downRate)); err != nil {
			return err
		}
		g.next += g.intervalMs
		if g.next < to {
			heap.Fix(&q, 0)
		} else {
			heap.Pop(&q)
		}
	}
	return nil
}

// checkQueue orders monitors by their next check.
type checkQueue []*monitorGen

func (q checkQueue) Len() int { return len(q) }

func (q checkQueue) Less(i, j int) bool {
	if q[i].next != q[j].next {
		return q[i].next < q[j].next
	}
	return q[i].monitorID < q[j].monitorID
}

func (q checkQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *checkQueue) Push(x any) { *q = append(*q, x.(*monitorGen)) }

func (q *checkQueue) Pop() any {
	old := *q
	n := len(old)
	g := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return g
}

// heartbeatWriter stores the checks as heartbeats of this node's stream in
// batches. Seq is strictly increasing and continues after the stored seq.
type heartbeatWriter struct {
	ctx         context.Context
	metrics     store.Metrics
	nodeID      string
	incarnation string
	seq         uint64
	size        int
	batch       []store.Heartbeat
	rows        int64
}

// add queues one heartbeat and stores the batch when it is full.
func (w *heartbeatWriter) add(g *monitorGen, tsMs int64, o outcome) error {
	w.seq++
	w.batch = append(w.batch, store.Heartbeat{
		NodeID:      w.nodeID,
		Incarnation: w.incarnation,
		Seq:         w.seq,
		TenantID:    g.tenantID,
		MonitorID:   g.monitorID,
		TsMs:        tsMs,
		Status:      o.status,
		LatencyMs:   o.latencyMs,
		ErrorClass:  o.errorClass,
		Message:     o.message,
	})
	if len(w.batch) >= w.size {
		return w.flush()
	}
	return nil
}

// flush stores the queued heartbeats. Every row must be new: a duplicate seq
// would mean that the stream was not fresh.
func (w *heartbeatWriter) flush() error {
	if len(w.batch) == 0 {
		return nil
	}
	n, err := w.metrics.InsertHeartbeats(w.ctx, w.batch)
	if err != nil {
		return fmt.Errorf("insert %d heartbeats: %w", len(w.batch), err)
	}
	if n != len(w.batch) {
		return fmt.Errorf("insert: only %d of %d heartbeats were new", n, len(w.batch))
	}
	w.rows += int64(n)
	w.batch = w.batch[:0]
	return nil
}

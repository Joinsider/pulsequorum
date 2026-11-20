// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

// Batching per ADR 0004: up to 500 rows or 1 s per transaction.
const (
	batchSize     = 500
	flushInterval = time.Second
	queueSize     = 8192
)

// HeartbeatWriter turns outcomes into heartbeats of this node's stream and
// stores them in batches. seq continues after a restart and is never reused
// within an incarnation (FA-REP-01).
type HeartbeatWriter struct {
	metrics     store.Metrics
	nodeID      string
	incarnation string
	log         *slog.Logger

	seq   atomic.Uint64
	queue chan store.Heartbeat
	// onStored are called with every batch stored successfully, in
	// registration order (push to the peers, leader aggregation per ADR 0021).
	// They must not block; see SetOnStored.
	onStored []func([]store.Heartbeat)
}

var _ Sink = (*HeartbeatWriter)(nil)

// NewHeartbeatWriter continues the stream (nodeID, metrics.Incarnation()).
func NewHeartbeatWriter(ctx context.Context, metrics store.Metrics, nodeID string, log *slog.Logger) (*HeartbeatWriter, error) {
	last, err := metrics.LastSeq(ctx, nodeID, metrics.Incarnation())
	if err != nil {
		return nil, err
	}
	w := &HeartbeatWriter{metrics: metrics, nodeID: nodeID, incarnation: metrics.Incarnation(), log: log,
		queue: make(chan store.Heartbeat, queueSize)}
	w.seq.Store(last)
	return w, nil
}

// SetOnStored registers fn to receive every batch after it was stored. fn must
// not block, because the writer runs on; ADR 0021. The slice is only valid
// during the call, so fn must copy what it keeps. Several functions may be
// registered; they are called in registration order. Register before Run starts.
func (w *HeartbeatWriter) SetOnStored(fn func([]store.Heartbeat)) {
	if fn != nil {
		w.onStored = append(w.onStored, fn)
	}
}

// Record implements Sink. It blocks only if the queue is full, which applies
// backpressure to the checks instead of losing heartbeats.
func (w *HeartbeatWriter) Record(ctx context.Context, o Outcome) {
	details, err := json.Marshal(o.Result.Details)
	if err != nil || o.Result.Details == nil {
		details = []byte("{}")
	}
	hb := store.Heartbeat{
		NodeID:      w.nodeID,
		Incarnation: w.incarnation,
		Seq:         w.seq.Add(1),
		TenantID:    o.Monitor.TenantID,
		MonitorID:   o.Monitor.ID,
		TsMs:        o.Started.UnixMilli(),
		Status:      store.CheckStatus(o.Result.Status),
		LatencyMs:   o.Result.LatencyMs,
		ErrorClass:  string(o.Result.ErrorClass),
		Message:     o.Result.Message,
		Details:     details,
	}
	select {
	case w.queue <- hb:
	case <-ctx.Done():
		w.log.Warn("heartbeat dropped during shutdown", "monitor", hb.MonitorID, "seq", hb.Seq)
	}
}

// Run writes queued heartbeats until ctx ends, then flushes what is left.
func (w *HeartbeatWriter) Run(ctx context.Context) {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	batch := make([]store.Heartbeat, 0, batchSize)
	for {
		select {
		case hb := <-w.queue:
			batch = append(batch, hb)
			if len(batch) >= batchSize {
				w.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-ctx.Done():
			// Drain without blocking and give the final write a short budget.
			for {
				select {
				case hb := <-w.queue:
					batch = append(batch, hb)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				w.flush(final, batch)
				cancel()
			}
			return
		}
	}
}

// flush retries until the batch is stored or ctx ends: a lost batch would
// leave a permanent gap in this node's stream.
func (w *HeartbeatWriter) flush(ctx context.Context, batch []store.Heartbeat) {
	backoff := 100 * time.Millisecond
	for {
		_, err := w.metrics.InsertHeartbeats(ctx, batch)
		if err == nil {
			for _, fn := range w.onStored {
				fn(batch)
			}
			return
		}
		w.log.Error("storing heartbeats failed, retrying", "count", len(batch), "err", err)
		select {
		case <-ctx.Done():
			w.log.Error("heartbeats lost", "count", len(batch), "first_seq", batch[0].Seq)
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

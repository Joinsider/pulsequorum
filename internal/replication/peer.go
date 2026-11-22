// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package replication

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"github.com/joinsider/pulsequorum/internal/backoff"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// rpcTimeout bounds every anti-entropy RPC.
const rpcTimeout = 30 * time.Second

// streamKey identifies a heartbeat stream.
type streamKey struct {
	node string
	inc  string
}

// worker serves one peer: a push loop (best effort) and an anti-entropy loop
// (authoritative) share one connection. Both loops run until the worker is
// cancelled.
type worker struct {
	r    *Replicator
	peer Peer
	log  *slog.Logger

	cancel  context.CancelFunc
	trigger chan struct{} // cap 1: coalesced anti-entropy requests
	batches chan []store.Heartbeat
	dropped atomic.Uint64

	mu           sync.Mutex
	connected    bool
	cancelStream context.CancelFunc
	caughtUpAt   time.Time
	lastErr      string
}

func newWorker(r *Replicator, p Peer, cancel context.CancelFunc) *worker {
	return &worker{
		r:       r,
		peer:    p,
		log:     r.log.With("peer", p.ID),
		cancel:  cancel,
		trigger: make(chan struct{}, 1),
		batches: make(chan []store.Heartbeat, r.opts.PushBuffer),
	}
}

func (w *worker) run(ctx context.Context) {
	conn, ok := w.dial(ctx)
	if !ok {
		return
	}
	defer func() { _ = conn.Close() }()
	client := pb.NewReplicationServiceClient(conn)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w.pushLoop(ctx, client)
	}()
	go func() {
		defer wg.Done()
		w.antiEntropyLoop(ctx, client)
	}()
	wg.Wait()
}

func (w *worker) dial(ctx context.Context) (*grpc.ClientConn, bool) {
	bo := backoff.HalfJitter(w.r.opts.backoffMin, w.r.opts.backoffMax)
	for {
		conn, err := w.r.opts.Dial(w.peer.Address)
		if err == nil {
			return conn, true
		}
		w.setErr(fmt.Errorf("dial: %w", err))
		if !backoff.Sleep(ctx, bo.Wait()) {
			return nil, false
		}
	}
}

// offer queues a batch for the push stream. It never blocks. Batches are
// discarded while no stream is open. A full queue closes the stream, so that
// the push loop reconnects; the peer catches up via anti-entropy.
func (w *worker) offer(batch []store.Heartbeat) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.connected {
		w.dropped.Add(1)
		return
	}
	select {
	case w.batches <- batch:
	default:
		w.dropped.Add(1)
		w.connected = false
		if w.cancelStream != nil {
			w.cancelStream()
			w.cancelStream = nil
		}
		w.log.Debug("push buffer full, reconnecting")
	}
}

func (w *worker) connect(cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.connected = true
	w.cancelStream = cancel
}

// disconnect marks the stream closed and discards queued batches. Batches
// queued after this point are refused by offer, so the drain is complete.
func (w *worker) disconnect() {
	w.mu.Lock()
	w.connected = false
	w.cancelStream = nil
	w.mu.Unlock()
	for {
		select {
		case <-w.batches:
			w.dropped.Add(1)
		default:
			return
		}
	}
}

func (w *worker) pushLoop(ctx context.Context, client pb.ReplicationServiceClient) {
	bo := backoff.HalfJitter(w.r.opts.backoffMin, w.r.opts.backoffMax)
	for {
		healthy, err := w.pushStream(ctx, client)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			bo.Reset()
		}
		w.setErr(err)
		w.log.Debug("push stream ended", "err", err)
		if !backoff.Sleep(ctx, bo.Wait()) {
			return
		}
	}
}

// pushStream runs one push stream until it fails or the worker is cancelled.
// healthy reports whether the stream was useful enough to reset the backoff:
// it received a response or lasted at least the minimum backoff.
func (w *worker) pushStream(ctx context.Context, client pb.ReplicationServiceClient) (healthy bool, err error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.StreamHeartbeats(sctx)
	if err != nil {
		return false, fmt.Errorf("open push stream: %w", err)
	}
	opened := w.r.opts.Now()
	var acked atomic.Bool
	defer func() {
		healthy = acked.Load() || w.r.opts.Now().Sub(opened) >= w.r.opts.backoffMin
	}()

	w.connect(cancel)
	defer w.disconnect()
	// Batches may have been lost while the stream was down (ADR 0012 B6).
	w.requestAntiEntropy()

	recvErr := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				recvErr <- err
				return
			}
			acked.Store(true)
			w.clearErr()
		}
	}()

	for {
		select {
		case <-sctx.Done():
			return false, fmt.Errorf("push stream closed: %w", sctx.Err())
		case err := <-recvErr:
			return false, fmt.Errorf("push receive: %w", err)
		case batch := <-w.batches:
			req := &pb.StreamHeartbeatsRequest{Heartbeats: heartbeatsToPB(batch)}
			if err := stream.Send(req); err != nil {
				w.dropped.Add(1)
				return false, fmt.Errorf("push send: %w", err)
			}
		}
	}
}

func (w *worker) antiEntropyLoop(ctx context.Context, client pb.ReplicationServiceClient) {
	ticker := time.NewTicker(w.r.opts.AntiEntropyInterval)
	defer ticker.Stop()
	w.antiEntropy(ctx, client)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.trigger:
		}
		w.antiEntropy(ctx, client)
	}
}

// antiEntropy runs one round. caughtUpAt is set to the start of the round only
// if the whole round succeeds (FA-REP-03).
func (w *worker) antiEntropy(ctx context.Context, client pb.ReplicationServiceClient) {
	t0 := w.r.opts.Now()
	if err := w.catchUp(ctx, client); err != nil {
		if ctx.Err() == nil {
			w.setErr(fmt.Errorf("anti-entropy: %w", err))
			w.log.Warn("anti-entropy round failed", "err", err)
		}
		return
	}
	w.mu.Lock()
	w.caughtUpAt = t0
	w.lastErr = ""
	w.mu.Unlock()
}

// catchUp fetches every stream the peer knows that is ahead of the local
// position, including streams of third nodes (ADR 0012 B3). The own current
// stream is skipped: it is never behind locally.
func (w *worker) catchUp(ctx context.Context, client pb.ReplicationServiceClient) error {
	m := w.r.opts.Metrics
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	wm, err := client.GetWatermarks(rctx, &pb.GetWatermarksRequest{})
	cancel()
	if err != nil {
		return fmt.Errorf("get watermarks: %w", err)
	}
	local, err := m.StreamPositions(ctx)
	if err != nil {
		return fmt.Errorf("local positions: %w", err)
	}
	have := make(map[streamKey]uint64, len(local))
	for _, p := range local {
		have[streamKey{node: p.NodeID, inc: p.Incarnation}] = p.Seq
	}
	own := m.Incarnation()
	for _, rp := range wm.GetPositions() {
		key := streamKey{node: rp.GetNodeId(), inc: rp.GetIncarnation()}
		if key.node == "" || key.inc == "" || (key.node == w.r.opts.NodeID && key.inc == own) {
			continue
		}
		if rp.GetSeq() <= have[key] {
			continue
		}
		if err := w.fetchStream(ctx, client, key, have[key], rp.GetSeq()); err != nil {
			return err
		}
	}
	return nil
}

// fetchStream pages through (after, until] of one stream and stores each page
// before fetching the next, so memory stays bounded by one page.
func (w *worker) fetchStream(ctx context.Context, client pb.ReplicationServiceClient, key streamKey, after, until uint64) error {
	m := w.r.opts.Metrics
	for after < until {
		rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		resp, err := client.FetchRange(rctx, &pb.FetchRangeRequest{
			NodeId:      key.node,
			Incarnation: key.inc,
			AfterSeq:    after,
			Limit:       uint32(w.r.opts.PageSize), //nolint:gosec // G115: New caps PageSize at maxFetchLimit
			UntilSeq:    until,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("fetch %s/%s after %d: %w", key.node, key.inc, after, err)
		}
		hbs := heartbeatsFromPB(resp.GetHeartbeats())
		next := until
		if resp.GetMore() {
			if len(hbs) == 0 {
				return fmt.Errorf("fetch %s/%s after %d: more without heartbeats", key.node, key.inc, after)
			}
			next = hbs[len(hbs)-1].Seq
		}
		if next <= after {
			return fmt.Errorf("fetch %s/%s: no progress after seq %d", key.node, key.inc, after)
		}
		hbs = w.validPage(hbs, key, after, next)
		if len(hbs) > 0 {
			if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
				return fmt.Errorf("store page of %s/%s: %w", key.node, key.inc, err)
			}
			if w.r.opts.OnStored != nil {
				w.r.opts.OnStored(hbs)
			}
		}
		// Every seq up to next exists on the peer, and the page holds all of them (ADR 0012 B2).
		if err := m.AdvanceStreamPosition(ctx, store.StreamPosition{NodeID: key.node, Incarnation: key.inc, Seq: next}); err != nil {
			return fmt.Errorf("advance position of %s/%s: %w", key.node, key.inc, err)
		}
		after = next
	}
	return nil
}

// validPage keeps the heartbeats of a fetched page that belong to the
// requested stream and range and pass the same checks as pushed ones.
func (w *worker) validPage(hbs []store.Heartbeat, key streamKey, after, next uint64) []store.Heartbeat {
	own := w.r.opts.Metrics.Incarnation()
	valid := hbs[:0]
	for _, h := range hbs {
		reason := rejectReason(h, w.r.opts.NodeID, own)
		if reason == "" && (h.NodeID != key.node || h.Incarnation != key.inc || h.Seq <= after || h.Seq > next) {
			reason = "outside the requested range"
		}
		if reason != "" {
			w.log.Debug("dropped fetched heartbeat", "reason", reason, "node", h.NodeID, "seq", h.Seq)
			continue
		}
		valid = append(valid, h)
	}
	return valid
}

// requestAntiEntropy asks for a round soon; repeated requests coalesce.
func (w *worker) requestAntiEntropy() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

func (w *worker) setErr(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastErr = err.Error()
}

func (w *worker) clearErr() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastErr = ""
}

func (w *worker) caughtUp() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.caughtUpAt
}

func (w *worker) status() PeerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return PeerStatus{
		ID:         w.peer.ID,
		Address:    w.peer.Address,
		Connected:  w.connected,
		CaughtUpAt: w.caughtUpAt,
		LastError:  w.lastErr,
		Dropped:    w.dropped.Load(),
	}
}

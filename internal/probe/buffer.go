// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

const (
	watermarkAckedSeq     = "probe_acked_seq"
	watermarkLostBelow    = "probe_lost_below"
	watermarkDroppedTotal = "probe_dropped_total"

	defaultAckRetain       = 10 * time.Minute
	defaultMaxAge          = 24 * time.Hour
	defaultMaxBytes        = int64(100 << 20) // 100 MiB
	defaultCleanupInterval = 60 * time.Second
	cleanupBatchSize       = 5000
)

// BufferStore defines the persistence operations required by Buffer.
// It is satisfied by *sqlite.MetricsStore.
type BufferStore interface {
	Incarnation() string
	Watermark(ctx context.Context, name string) (int64, error)
	SetWatermark(ctx context.Context, name string, value int64) error
	MinSeq(ctx context.Context, nodeID, incarnation string) (uint64, error)
	LastSeq(ctx context.Context, nodeID, incarnation string) (uint64, error)
	UnackedStats(ctx context.Context, nodeID, incarnation string, afterSeq uint64) (uint64, int64, error)
	DeleteStreamHeartbeatsUpTo(ctx context.Context, nodeID, incarnation string, maxSeq uint64, limit int) (int64, error)
	DeleteStreamHeartbeatsOlderThan(ctx context.Context, nodeID, incarnation string, afterSeq uint64, beforeMs int64, limit int) (int64, uint64, error)
	DeleteOldestStreamHeartbeats(ctx context.Context, nodeID, incarnation string, ackedSeq uint64, limit int) (int64, int64, uint64, error)
	DBSizeBytes(ctx context.Context) (int64, error)
	IncrementalVacuum(ctx context.Context, pages int) error
}

// BufferOptions configures a probe heartbeat Buffer.
type BufferOptions struct {
	Store     BufferStore
	NodeID    string
	AckRetain time.Duration
	MaxAge    time.Duration
	MaxBytes  int64
	Now       func() time.Time
	Log       *slog.Logger
}

type ackEntry struct {
	seq uint64
	at  time.Time
}

// Buffer tracks acknowledgment watermarks and enforces retention and size
// limits on the probe's local heartbeat buffer.
type Buffer struct {
	store       BufferStore
	nodeID      string
	incarnation string
	ackRetain   time.Duration
	maxAge      time.Duration
	maxBytes    int64
	now         func() time.Time
	log         *slog.Logger

	cleanupMu sync.Mutex

	mu           sync.Mutex
	ackedSeq     uint64
	lostBelow    uint64
	droppedTotal uint64
	ackHistory   []ackEntry
}

// NewBuffer initializes a Buffer, loading persisted watermarks from opts.Store.
func NewBuffer(ctx context.Context, opts BufferOptions) (*Buffer, error) {
	if opts.Store == nil {
		return nil, errors.New("probe buffer: store is required")
	}
	if opts.NodeID == "" {
		return nil, errors.New("probe buffer: nodeID is required")
	}
	if opts.AckRetain <= 0 {
		opts.AckRetain = defaultAckRetain
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = defaultMaxAge
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	acked, err := opts.Store.Watermark(ctx, watermarkAckedSeq)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", watermarkAckedSeq, err)
	}
	lost, err := opts.Store.Watermark(ctx, watermarkLostBelow)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", watermarkLostBelow, err)
	}
	dropped, err := opts.Store.Watermark(ctx, watermarkDroppedTotal)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", watermarkDroppedTotal, err)
	}

	b := &Buffer{
		store:        opts.Store,
		nodeID:       opts.NodeID,
		incarnation:  opts.Store.Incarnation(),
		ackRetain:    opts.AckRetain,
		maxAge:       opts.MaxAge,
		maxBytes:     opts.MaxBytes,
		now:          opts.Now,
		log:          opts.Log,
		ackedSeq:     uint64(max(acked, 0)),
		lostBelow:    uint64(max(lost, 0)),
		droppedTotal: uint64(max(dropped, 0)),
	}
	if b.ackedSeq > 0 {
		b.ackHistory = []ackEntry{{seq: b.ackedSeq, at: b.now()}}
	}
	return b, nil
}

// RecordAck records a monotonic acknowledgment watermark and persists it.
func (b *Buffer) RecordAck(ctx context.Context, seq uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if seq <= b.ackedSeq {
		return nil
	}
	if err := b.store.SetWatermark(ctx, watermarkAckedSeq, int64(seq)); err != nil {
		return fmt.Errorf("persist %s: %w", watermarkAckedSeq, err)
	}
	b.ackedSeq = seq
	b.ackHistory = append(b.ackHistory, ackEntry{seq: seq, at: b.now()})
	return nil
}

// AckedSeq returns the highest acknowledged sequence number.
func (b *Buffer) AckedSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ackedSeq
}

// LostBelow returns the lowest sequence number not known to be lost.
func (b *Buffer) LostBelow() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lostBelow
}

// DroppedTotal returns the cumulative number of unacknowledged heartbeats dropped.
func (b *Buffer) DroppedTotal() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.droppedTotal
}

// StartAfter computes the starting sequence number for resuming upload to a
// voter reporting voterPos: max(voterPos, minSeq-1) when minSeq > 0, else voterPos.
func (b *Buffer) StartAfter(ctx context.Context, voterPos uint64) (uint64, error) {
	minSeq, err := b.store.MinSeq(ctx, b.nodeID, b.incarnation)
	if err != nil {
		return 0, fmt.Errorf("read min seq: %w", err)
	}
	if minSeq > 0 && minSeq-1 > voterPos {
		return minSeq - 1, nil
	}
	return voterPos, nil
}

// Status returns the current probe buffer fill and drop statistics.
func (b *Buffer) Status(ctx context.Context) (*pb.ProbeBufferStatus, error) {
	b.mu.Lock()
	ackedSeq := b.ackedSeq
	droppedTotal := b.droppedTotal
	b.mu.Unlock()

	bytes, err := b.store.DBSizeBytes(ctx)
	if err != nil {
		return nil, fmt.Errorf("read db size: %w", err)
	}
	unacked, oldestMs, err := b.store.UnackedStats(ctx, b.nodeID, b.incarnation, ackedSeq)
	if err != nil {
		return nil, fmt.Errorf("read unacked stats: %w", err)
	}

	return &pb.ProbeBufferStatus{
		Bytes:           bytes,
		Unacked:         unacked,
		OldestUnackedMs: oldestMs,
		DroppedTotal:    droppedTotal,
	}, nil
}

// Cleanup enforces retention and size limits in the order defined by ADR 0034 section 5:
// 1. Delete acked heartbeats whose AckRetain window has elapsed.
// 2. Delete unacked heartbeats older than MaxAge (updating lostBelow and droppedTotal).
// 3. Vacuum and delete oldest heartbeats while DBSizeBytes exceeds MaxBytes.
func (b *Buffer) Cleanup(ctx context.Context) error {
	b.cleanupMu.Lock()
	defer b.cleanupMu.Unlock()

	if err := b.cleanupAcked(ctx); err != nil {
		return err
	}
	if err := b.cleanupExpired(ctx); err != nil {
		return err
	}
	return b.cleanupOverflow(ctx)
}

func (b *Buffer) cleanupAcked(ctx context.Context) error {
	cutoff := b.now().Add(-b.ackRetain)

	b.mu.Lock()
	var maxSeq uint64
	for _, entry := range b.ackHistory {
		if !entry.at.After(cutoff) && entry.seq > maxSeq {
			maxSeq = entry.seq
		}
	}
	b.mu.Unlock()

	if maxSeq == 0 {
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleted, err := b.store.DeleteStreamHeartbeatsUpTo(ctx, b.nodeID, b.incarnation, maxSeq, cleanupBatchSize)
		if err != nil {
			return fmt.Errorf("delete acked heartbeats up to %d: %w", maxSeq, err)
		}
		if deleted < cleanupBatchSize {
			break
		}
	}

	b.mu.Lock()
	remaining := b.ackHistory[:0]
	for _, entry := range b.ackHistory {
		if entry.seq > maxSeq {
			remaining = append(remaining, entry)
		}
	}
	b.ackHistory = remaining
	b.mu.Unlock()
	return nil
}

func (b *Buffer) cleanupExpired(ctx context.Context) error {
	beforeMs := b.now().Add(-b.maxAge).UnixMilli()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		ackedSeq := b.ackedSeq
		b.mu.Unlock()

		deleted, maxDeletedSeq, err := b.store.DeleteStreamHeartbeatsOlderThan(ctx, b.nodeID, b.incarnation, ackedSeq, beforeMs, cleanupBatchSize)
		if err != nil {
			return fmt.Errorf("delete expired heartbeats: %w", err)
		}
		if deleted > 0 {
			if err := b.recordUnackedDrop(ctx, uint64(deleted), maxDeletedSeq, "expired"); err != nil {
				return err
			}
		}
		if deleted < cleanupBatchSize {
			break
		}
	}
	return nil
}

func (b *Buffer) cleanupOverflow(ctx context.Context) error {
	if err := b.store.IncrementalVacuum(ctx, 0); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, err := b.store.DBSizeBytes(ctx)
		if err != nil {
			return fmt.Errorf("read db size: %w", err)
		}
		if size <= b.maxBytes {
			break
		}

		b.mu.Lock()
		ackedSeq := b.ackedSeq
		b.mu.Unlock()

		deleted, unackedDeleted, maxUnackedSeq, err := b.store.DeleteOldestStreamHeartbeats(ctx, b.nodeID, b.incarnation, ackedSeq, cleanupBatchSize)
		if err != nil {
			return fmt.Errorf("delete oldest heartbeats: %w", err)
		}
		if deleted == 0 {
			break
		}
		if unackedDeleted > 0 {
			if err := b.recordUnackedDrop(ctx, uint64(unackedDeleted), maxUnackedSeq, "size_limit"); err != nil {
				return err
			}
		}
		if err := b.store.IncrementalVacuum(ctx, 0); err != nil {
			return fmt.Errorf("incremental vacuum: %w", err)
		}
	}
	return nil
}

func (b *Buffer) recordUnackedDrop(ctx context.Context, count, maxSeq uint64, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	newLostBelow := b.lostBelow
	if maxSeq > 0 && maxSeq+1 > newLostBelow {
		newLostBelow = maxSeq + 1
	}
	newDroppedTotal := b.droppedTotal + count

	if err := b.store.SetWatermark(ctx, watermarkLostBelow, int64(newLostBelow)); err != nil {
		return fmt.Errorf("persist %s: %w", watermarkLostBelow, err)
	}
	if err := b.store.SetWatermark(ctx, watermarkDroppedTotal, int64(newDroppedTotal)); err != nil {
		return fmt.Errorf("persist %s: %w", watermarkDroppedTotal, err)
	}

	b.lostBelow = newLostBelow
	b.droppedTotal = newDroppedTotal
	b.log.Warn("dropped unacked probe heartbeats",
		"reason", reason,
		"dropped", count,
		"lost_below", newLostBelow,
		"dropped_total", newDroppedTotal,
	)
	return nil
}

// RunCleanup runs Cleanup periodically until ctx is canceled.
func (b *Buffer) RunCleanup(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCleanupInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.Cleanup(ctx); err != nil && ctx.Err() == nil {
				b.log.Warn("probe buffer cleanup failed", "error", err)
			}
		}
	}
}

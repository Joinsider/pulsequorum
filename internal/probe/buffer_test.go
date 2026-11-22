// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

var _ BufferStore = (*sqlite.MetricsStore)(nil)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func openTestMetricsStore(t *testing.T) *sqlite.MetricsStore {
	t.Helper()
	ctx := context.Background()
	m, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatalf("OpenMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func insertTestHeartbeats(t *testing.T, m *sqlite.MetricsStore, nodeID string, seqs []uint64, tsFn func(seq uint64) int64) {
	t.Helper()
	inc := m.Incarnation()
	hbs := make([]store.Heartbeat, 0, len(seqs))
	for _, seq := range seqs {
		hbs = append(hbs, store.Heartbeat{
			NodeID:      nodeID,
			Incarnation: inc,
			Seq:         seq,
			TenantID:    "tenant-1",
			MonitorID:   "mon-1",
			TsMs:        tsFn(seq),
			Status:      store.CheckUp,
			LatencyMs:   10,
		})
	}
	if _, err := m.InsertHeartbeats(context.Background(), hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}
}

func remainingTestSeqs(t *testing.T, m *sqlite.MetricsStore, nodeID string) []uint64 {
	t.Helper()
	hbs, more, err := m.HeartbeatsAfter(context.Background(), nodeID, m.Incarnation(), 0, 0, 1000)
	if err != nil {
		t.Fatalf("HeartbeatsAfter: %v", err)
	}
	if more {
		t.Fatalf("HeartbeatsAfter unexpectedly returned more=true")
	}
	seqs := make([]uint64, len(hbs))
	for i, hb := range hbs {
		seqs[i] = hb.Seq
	}
	return seqs
}

func TestBufferRule1AckRetain(t *testing.T) {
	ctx := context.Background()
	m := openTestMetricsStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	const nodeID = "probe-1"

	// Heartbeats created 2h ago (buffered while offline).
	twoHoursAgo := clk.Now().Add(-2 * time.Hour)
	insertTestHeartbeats(t, m, nodeID, []uint64{1, 2, 3}, func(seq uint64) int64 {
		return twoHoursAgo.Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})

	buf, err := NewBuffer(ctx, BufferOptions{
		Store:     m,
		NodeID:    nodeID,
		AckRetain: 10 * time.Minute,
		MaxAge:    24 * time.Hour,
		MaxBytes:  100 << 20,
		Now:       clk.Now,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	// Acked at t=0.
	if err := buf.RecordAck(ctx, 3); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}

	// At t=5m, acked heartbeats must still be retained.
	clk.Advance(5 * time.Minute)
	if err := buf.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup at 5m: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 3 {
		t.Fatalf("at 5m remaining seqs = %v, want [1 2 3]", got)
	}

	// At t=10m, acked heartbeats are deleted; lost_below and dropped_total stay 0.
	clk.Advance(5 * time.Minute)
	if err := buf.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup at 10m: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 0 {
		t.Fatalf("at 10m remaining seqs = %v, want []", got)
	}
	if buf.LostBelow() != 0 {
		t.Fatalf("LostBelow() = %d, want 0", buf.LostBelow())
	}
	if buf.DroppedTotal() != 0 {
		t.Fatalf("DroppedTotal() = %d, want 0", buf.DroppedTotal())
	}
}

func TestBufferRule2MaxAge(t *testing.T) {
	ctx := context.Background()
	m := openTestMetricsStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	const nodeID = "probe-1"

	// seq 1, 2 are 25h old; seq 3, 4 are 1h old.
	insertTestHeartbeats(t, m, nodeID, []uint64{1, 2}, func(seq uint64) int64 {
		return clk.Now().Add(-25 * time.Hour).Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})
	insertTestHeartbeats(t, m, nodeID, []uint64{3, 4}, func(seq uint64) int64 {
		return clk.Now().Add(-1 * time.Hour).Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})

	buf, err := NewBuffer(ctx, BufferOptions{
		Store:    m,
		NodeID:   nodeID,
		MaxAge:   24 * time.Hour,
		MaxBytes: 100 << 20,
		Now:      clk.Now,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if err := buf.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	got := remainingTestSeqs(t, m, nodeID)
	if len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("remaining seqs = %v, want [3 4]", got)
	}
	if buf.LostBelow() != 3 {
		t.Fatalf("LostBelow() = %d, want 3", buf.LostBelow())
	}
	if buf.DroppedTotal() != 2 {
		t.Fatalf("DroppedTotal() = %d, want 2", buf.DroppedTotal())
	}
}

type fakeSizeStore struct {
	*sqlite.MetricsStore
	nodeID string
	perRow int64
	base   int64
}

func (f *fakeSizeStore) DBSizeBytes(ctx context.Context) (int64, error) {
	minSeq, err := f.MinSeq(ctx, f.nodeID, f.Incarnation())
	if err != nil {
		return 0, err
	}
	if minSeq == 0 {
		return f.base, nil
	}
	lastSeq, err := f.LastSeq(ctx, f.nodeID, f.Incarnation())
	if err != nil {
		return 0, err
	}
	count, _, err := f.UnackedStats(ctx, f.nodeID, f.Incarnation(), 0)
	if err != nil {
		return 0, err
	}
	_ = lastSeq
	return f.base + int64(min(count, 1<<30))*f.perRow, nil
}

func TestBufferRule3SizeLimit(t *testing.T) {
	ctx := context.Background()
	m := openTestMetricsStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	const nodeID = "probe-1"

	storeWrapper := &fakeSizeStore{
		MetricsStore: m,
		nodeID:       nodeID,
		perRow:       1000,
		base:         0,
	}

	// Insert 3 rows: seq 1, 2 (acked), seq 3 (unacked), all recent.
	insertTestHeartbeats(t, m, nodeID, []uint64{1, 2, 3}, func(seq uint64) int64 {
		return clk.Now().Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})

	// Set MaxBytes = 5000 first to record ack without triggering overflow, then test with small MaxBytes.
	buf, err := NewBuffer(ctx, BufferOptions{
		Store:     storeWrapper,
		NodeID:    nodeID,
		AckRetain: 10 * time.Minute,
		MaxAge:    24 * time.Hour,
		MaxBytes:  0, // will override below
		Now:       clk.Now,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	buf.maxBytes = 2500 // 3 rows = 3000 > 2500; deleting oldest (batch=5000) clears all 3 in one call, wait:
	// Let's test two separate cases cleanly:
	// Case A: Only acked rows exist -> overflow deletes acked rows, lost_below stays 0.
	if err := buf.RecordAck(ctx, 3); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	if err := buf.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup case A: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 0 {
		t.Fatalf("case A remaining seqs = %v, want []", got)
	}
	if buf.LostBelow() != 0 {
		t.Fatalf("case A LostBelow() = %d, want 0", buf.LostBelow())
	}
	if buf.DroppedTotal() != 0 {
		t.Fatalf("case A DroppedTotal() = %d, want 0", buf.DroppedTotal())
	}

	// Case B: Unacked rows exist -> overflow deletes unacked rows, lost_below advances.
	insertTestHeartbeats(t, m, nodeID, []uint64{4, 5, 6}, func(seq uint64) int64 {
		return clk.Now().Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})
	if err := buf.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup case B: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 0 {
		t.Fatalf("case B remaining seqs = %v, want []", got)
	}
	if buf.LostBelow() != 7 {
		t.Fatalf("case B LostBelow() = %d, want 7", buf.LostBelow())
	}
	if buf.DroppedTotal() != 3 {
		t.Fatalf("case B DroppedTotal() = %d, want 3", buf.DroppedTotal())
	}
}

func TestBufferPersistenceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	m := openTestMetricsStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	const nodeID = "probe-1"

	insertTestHeartbeats(t, m, nodeID, []uint64{1, 2, 3, 4}, func(seq uint64) int64 {
		if seq <= 2 {
			return clk.Now().Add(-25 * time.Hour).UnixMilli()
		}
		return clk.Now().UnixMilli()
	})

	buf1, err := NewBuffer(ctx, BufferOptions{
		Store:     m,
		NodeID:    nodeID,
		AckRetain: 10 * time.Minute,
		MaxAge:    24 * time.Hour,
		Now:       clk.Now,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer 1: %v", err)
	}
	if err := buf1.RecordAck(ctx, 1); err != nil {
		t.Fatalf("RecordAck(1): %v", err)
	}
	// Monotonic: lower seq ignored.
	if err := buf1.RecordAck(ctx, 1); err != nil {
		t.Fatalf("RecordAck(1) repeat: %v", err)
	}
	if err := buf1.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup 1: %v", err)
	}
	// seq 2 was unacked and expired -> lostBelow=3, droppedTotal=1.
	if buf1.AckedSeq() != 1 || buf1.LostBelow() != 3 || buf1.DroppedTotal() != 1 {
		t.Fatalf("buf1 state = (%d, %d, %d), want (1, 3, 1)", buf1.AckedSeq(), buf1.LostBelow(), buf1.DroppedTotal())
	}

	// Simulate restart.
	buf2, err := NewBuffer(ctx, BufferOptions{
		Store:     m,
		NodeID:    nodeID,
		AckRetain: 10 * time.Minute,
		MaxAge:    24 * time.Hour,
		Now:       clk.Now,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer 2: %v", err)
	}
	if buf2.AckedSeq() != 1 || buf2.LostBelow() != 3 || buf2.DroppedTotal() != 1 {
		t.Fatalf("buf2 state = (%d, %d, %d), want (1, 3, 1)", buf2.AckedSeq(), buf2.LostBelow(), buf2.DroppedTotal())
	}

	// Restarted probe still honors AckRetain for seq 1.
	clk.Advance(5 * time.Minute)
	if err := buf2.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup 2 at 5m: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 3 {
		t.Fatalf("at 5m after restart remaining = %v, want [1 3 4]", got)
	}

	clk.Advance(5 * time.Minute)
	if err := buf2.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup 2 at 10m: %v", err)
	}
	if got := remainingTestSeqs(t, m, nodeID); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("at 10m after restart remaining = %v, want [3 4]", got)
	}
}

func TestBufferStartAfterAndStatus(t *testing.T) {
	ctx := context.Background()
	m := openTestMetricsStore(t)
	clk := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	const nodeID = "probe-1"

	buf, err := NewBuffer(ctx, BufferOptions{
		Store:  m,
		NodeID: nodeID,
		Now:    clk.Now,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	// Empty buffer returns voterPos.
	got, err := buf.StartAfter(ctx, 42)
	if err != nil {
		t.Fatalf("StartAfter empty: %v", err)
	}
	if got != 42 {
		t.Fatalf("StartAfter empty = %d, want 42", got)
	}

	// Buffer has seqs 10..12.
	insertTestHeartbeats(t, m, nodeID, []uint64{10, 11, 12}, func(seq uint64) int64 {
		return clk.Now().Add(time.Duration(min(seq, 1<<30)) * time.Second).UnixMilli()
	})

	// voterPos=5 < minSeq-1=9 -> returns 9.
	got, err = buf.StartAfter(ctx, 5)
	if err != nil {
		t.Fatalf("StartAfter(5): %v", err)
	}
	if got != 9 {
		t.Fatalf("StartAfter(5) = %d, want 9", got)
	}

	// voterPos=11 > minSeq-1=9 -> returns 11.
	got, err = buf.StartAfter(ctx, 11)
	if err != nil {
		t.Fatalf("StartAfter(11): %v", err)
	}
	if got != 11 {
		t.Fatalf("StartAfter(11) = %d, want 11", got)
	}

	if err := buf.RecordAck(ctx, 10); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	st, err := buf.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Unacked != 2 {
		t.Fatalf("Status.Unacked = %d, want 2", st.Unacked)
	}
	if st.OldestUnackedMs != clk.Now().Add(11*time.Second).UnixMilli() {
		t.Fatalf("Status.OldestUnackedMs = %d, want %d", st.OldestUnackedMs, clk.Now().Add(11*time.Second).UnixMilli())
	}
	if st.Bytes <= 0 {
		t.Fatalf("Status.Bytes = %d, want > 0", st.Bytes)
	}
}

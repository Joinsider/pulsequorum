// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

func makeProbeHeartbeats(nodeID, incarnation string, seqs []uint64, tsFn func(seq uint64) int64) []store.Heartbeat {
	hbs := make([]store.Heartbeat, 0, len(seqs))
	for _, seq := range seqs {
		hbs = append(hbs, store.Heartbeat{
			NodeID:      nodeID,
			Incarnation: incarnation,
			Seq:         seq,
			TenantID:    "tenant-1",
			MonitorID:   "mon-1",
			TsMs:        tsFn(seq),
			Status:      store.CheckUp,
			LatencyMs:   12,
		})
	}
	return hbs
}

func remainingSeqs(t *testing.T, m *MetricsStore, nodeID, incarnation string) []uint64 {
	t.Helper()
	hbs, more, err := m.HeartbeatsAfter(context.Background(), nodeID, incarnation, 0, 0, 100)
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

func assertSeqsEqual(t *testing.T, got, want []uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seqs = %v, want %v", got, want)
		}
	}
}

func TestProbeBufferLastSeqAndMinSeq(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	const (
		node = "node-1"
		inc  = "inc-1"
	)

	last, err := m.LastSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("LastSeq empty: %v", err)
	}
	if last != 0 {
		t.Fatalf("LastSeq empty = %d, want 0", last)
	}

	minSeq, err := m.MinSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("MinSeq empty: %v", err)
	}
	if minSeq != 0 {
		t.Fatalf("MinSeq empty = %d, want 0", minSeq)
	}

	hbs := makeProbeHeartbeats(node, inc, []uint64{1, 2, 3, 4, 5}, func(seq uint64) int64 {
		return int64(seq * 1000)
	})
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	last, err = m.LastSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("LastSeq after insert: %v", err)
	}
	if last != 5 {
		t.Fatalf("LastSeq after insert = %d, want 5", last)
	}

	minSeq, err = m.MinSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("MinSeq after insert: %v", err)
	}
	if minSeq != 1 {
		t.Fatalf("MinSeq after insert = %d, want 1", minSeq)
	}

	// Delete first two rows; MinSeq should advance to 3.
	deleted, err := m.DeleteStreamHeartbeatsUpTo(ctx, node, inc, 2, 10)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsUpTo(2): %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	minSeq, err = m.MinSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("MinSeq after partial delete: %v", err)
	}
	if minSeq != 3 {
		t.Fatalf("MinSeq after partial delete = %d, want 3", minSeq)
	}

	// Delete all remaining rows; MinSeq becomes 0, LastSeq must stay 5 from stream_positions.
	deleted, err = m.DeleteStreamHeartbeatsUpTo(ctx, node, inc, 5, 10)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsUpTo(5): %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted = %d, want 3", deleted)
	}
	minSeq, err = m.MinSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("MinSeq after full delete: %v", err)
	}
	if minSeq != 0 {
		t.Fatalf("MinSeq after full delete = %d, want 0", minSeq)
	}
	last, err = m.LastSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("LastSeq after full delete: %v", err)
	}
	if last != 5 {
		t.Fatalf("LastSeq after full delete = %d, want 5", last)
	}

	// Advancing stream_positions higher than any heartbeat updates LastSeq.
	if err := m.AdvanceStreamPosition(ctx, store.StreamPosition{NodeID: node, Incarnation: inc, Seq: 12}); err != nil {
		t.Fatalf("AdvanceStreamPosition: %v", err)
	}
	last, err = m.LastSeq(ctx, node, inc)
	if err != nil {
		t.Fatalf("LastSeq after AdvanceStreamPosition: %v", err)
	}
	if last != 12 {
		t.Fatalf("LastSeq after AdvanceStreamPosition = %d, want 12", last)
	}
}

func TestProbeBufferUnackedStats(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	const (
		node = "node-stats"
		inc  = "inc-1"
	)

	cnt, oldest, err := m.UnackedStats(ctx, node, inc, 0)
	if err != nil {
		t.Fatalf("UnackedStats empty: %v", err)
	}
	if cnt != 0 || oldest != 0 {
		t.Fatalf("UnackedStats empty = (%d, %d), want (0, 0)", cnt, oldest)
	}

	hbs := makeProbeHeartbeats(node, inc, []uint64{1, 2, 3, 4, 5}, func(seq uint64) int64 {
		return int64(seq * 1000)
	})
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	cnt, oldest, err = m.UnackedStats(ctx, node, inc, 0)
	if err != nil {
		t.Fatalf("UnackedStats(0): %v", err)
	}
	if cnt != 5 || oldest != 1000 {
		t.Fatalf("UnackedStats(0) = (%d, %d), want (5, 1000)", cnt, oldest)
	}

	cnt, oldest, err = m.UnackedStats(ctx, node, inc, 2)
	if err != nil {
		t.Fatalf("UnackedStats(2): %v", err)
	}
	if cnt != 3 || oldest != 3000 {
		t.Fatalf("UnackedStats(2) = (%d, %d), want (3, 3000)", cnt, oldest)
	}

	cnt, oldest, err = m.UnackedStats(ctx, node, inc, 5)
	if err != nil {
		t.Fatalf("UnackedStats(5): %v", err)
	}
	if cnt != 0 || oldest != 0 {
		t.Fatalf("UnackedStats(5) = (%d, %d), want (0, 0)", cnt, oldest)
	}
}

func TestProbeBufferDeleteStreamHeartbeatsUpTo(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	const (
		node      = "node-upto"
		otherNode = "node-other"
		inc       = "inc-1"
	)

	if _, err := m.DeleteStreamHeartbeatsUpTo(ctx, node, inc, 10, 0); err == nil {
		t.Fatal("DeleteStreamHeartbeatsUpTo with limit 0 expected error")
	}

	hbs := makeProbeHeartbeats(node, inc, []uint64{1, 2, 3, 4, 5, 6}, func(seq uint64) int64 {
		return int64(seq * 100)
	})
	otherHbs := makeProbeHeartbeats(otherNode, inc, []uint64{1, 2}, func(seq uint64) int64 {
		return int64(seq * 100)
	})
	if _, err := m.InsertHeartbeats(ctx, append(hbs, otherHbs...)); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	// Delete up to seq 4 with limit 2: must delete seq 1 and 2 (ascending order).
	deleted, err := m.DeleteStreamHeartbeatsUpTo(ctx, node, inc, 4, 2)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsUpTo(4, 2): %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{3, 4, 5, 6})

	// Delete remaining up to seq 4.
	deleted, err = m.DeleteStreamHeartbeatsUpTo(ctx, node, inc, 4, 10)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsUpTo(4, 10): %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{5, 6})

	// Other stream must be untouched.
	assertSeqsEqual(t, remainingSeqs(t, m, otherNode, inc), []uint64{1, 2})
}

func TestProbeBufferDeleteStreamHeartbeatsOlderThan(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	const (
		node = "node-older"
		inc  = "inc-1"
	)

	if _, _, err := m.DeleteStreamHeartbeatsOlderThan(ctx, node, inc, 0, 1000, 0); err == nil {
		t.Fatal("DeleteStreamHeartbeatsOlderThan with limit 0 expected error")
	}

	hbs := makeProbeHeartbeats(node, inc, []uint64{1, 2, 3, 4, 5, 6}, func(seq uint64) int64 {
		return int64(seq * 100)
	})
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	// Acked rows (seq <= 2) must not be deleted even though ts_ms < 550.
	// Unacked rows with ts_ms < 550 are seq 3, 4, 5; limit=2 deletes 3 and 4.
	deleted, maxSeq, err := m.DeleteStreamHeartbeatsOlderThan(ctx, node, inc, 2, 550, 2)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsOlderThan: %v", err)
	}
	if deleted != 2 || maxSeq != 4 {
		t.Fatalf("got (deleted=%d, maxSeq=%d), want (2, 4)", deleted, maxSeq)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{1, 2, 5, 6})

	// Next batch deletes seq 5 (ts_ms=500 < 550), leaving seq 6 (ts_ms=600 >= 550).
	deleted, maxSeq, err = m.DeleteStreamHeartbeatsOlderThan(ctx, node, inc, 2, 550, 5)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsOlderThan second pass: %v", err)
	}
	if deleted != 1 || maxSeq != 5 {
		t.Fatalf("got (deleted=%d, maxSeq=%d), want (1, 5)", deleted, maxSeq)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{1, 2, 6})

	// No more eligible rows.
	deleted, maxSeq, err = m.DeleteStreamHeartbeatsOlderThan(ctx, node, inc, 2, 550, 5)
	if err != nil {
		t.Fatalf("DeleteStreamHeartbeatsOlderThan empty pass: %v", err)
	}
	if deleted != 0 || maxSeq != 0 {
		t.Fatalf("got (deleted=%d, maxSeq=%d), want (0, 0)", deleted, maxSeq)
	}
}

func TestProbeBufferDeleteOldestStreamHeartbeats(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	const (
		node = "node-oldest"
		inc  = "inc-1"
	)

	if _, _, _, err := m.DeleteOldestStreamHeartbeats(ctx, node, inc, 0, 0); err == nil {
		t.Fatal("DeleteOldestStreamHeartbeats with limit 0 expected error")
	}

	hbs := makeProbeHeartbeats(node, inc, []uint64{1, 2, 3, 4, 5, 6}, func(seq uint64) int64 {
		return int64(seq * 100)
	})
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}

	// Delete only acked rows (seq 1, 2 with ackedSeq=4): unackedDeleted and maxUnackedSeq must be 0.
	deleted, unackedDeleted, maxUnacked, err := m.DeleteOldestStreamHeartbeats(ctx, node, inc, 4, 2)
	if err != nil {
		t.Fatalf("DeleteOldestStreamHeartbeats(acked=4, limit=2): %v", err)
	}
	if deleted != 2 || unackedDeleted != 0 || maxUnacked != 0 {
		t.Fatalf("got (%d, %d, %d), want (2, 0, 0)", deleted, unackedDeleted, maxUnacked)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{3, 4, 5, 6})

	// Delete mix of acked (3, 4) and unacked (5) rows.
	deleted, unackedDeleted, maxUnacked, err = m.DeleteOldestStreamHeartbeats(ctx, node, inc, 4, 3)
	if err != nil {
		t.Fatalf("DeleteOldestStreamHeartbeats(acked=4, limit=3): %v", err)
	}
	if deleted != 3 || unackedDeleted != 1 || maxUnacked != 5 {
		t.Fatalf("got (%d, %d, %d), want (3, 1, 5)", deleted, unackedDeleted, maxUnacked)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), []uint64{6})

	// Delete remaining unacked row (6).
	deleted, unackedDeleted, maxUnacked, err = m.DeleteOldestStreamHeartbeats(ctx, node, inc, 4, 5)
	if err != nil {
		t.Fatalf("DeleteOldestStreamHeartbeats(acked=4, limit=5): %v", err)
	}
	if deleted != 1 || unackedDeleted != 1 || maxUnacked != 6 {
		t.Fatalf("got (%d, %d, %d), want (1, 1, 6)", deleted, unackedDeleted, maxUnacked)
	}
	assertSeqsEqual(t, remainingSeqs(t, m, node, inc), nil)

	// Empty stream returns all zeros.
	deleted, unackedDeleted, maxUnacked, err = m.DeleteOldestStreamHeartbeats(ctx, node, inc, 4, 5)
	if err != nil {
		t.Fatalf("DeleteOldestStreamHeartbeats empty: %v", err)
	}
	if deleted != 0 || unackedDeleted != 0 || maxUnacked != 0 {
		t.Fatalf("got (%d, %d, %d), want (0, 0, 0)", deleted, unackedDeleted, maxUnacked)
	}
}

func TestProbeBufferDBSizeBytesAndIncrementalVacuum(t *testing.T) {
	ctx := context.Background()
	m := openMetrics(t, t.TempDir())

	size, err := m.DBSizeBytes(ctx)
	if err != nil {
		t.Fatalf("DBSizeBytes: %v", err)
	}
	if size <= 0 {
		t.Fatalf("DBSizeBytes = %d, want > 0", size)
	}

	hbs := makeProbeHeartbeats("node-vac", "inc-1", []uint64{1, 2, 3, 4, 5}, func(seq uint64) int64 {
		return int64(seq * 100)
	})
	if _, err := m.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}
	if _, err := m.DeleteStreamHeartbeatsUpTo(ctx, "node-vac", "inc-1", 5, 10); err != nil {
		t.Fatalf("DeleteStreamHeartbeatsUpTo: %v", err)
	}

	if err := m.IncrementalVacuum(ctx, 0); err != nil {
		t.Fatalf("IncrementalVacuum: %v", err)
	}
}

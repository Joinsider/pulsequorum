// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package replication

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/joinsider/pulsequorum/internal/store"
)

func TestAntiEntropyCatchesUpCompactedStreams(t *testing.T) {
	n := newTestNet(t, Options{PageSize: 3})
	a := n.add("a", newFakeMetrics("inc-a"), "b")
	b := n.add("b", newFakeMetrics("inc-b"))

	// B's own stream 1..5. Stream of C: 1-3 and 6-10 stored, 4-5 compacted away;
	// B's position for C is 10 because no heartbeat 4 or 5 exists anywhere.
	mustInsert(t, b.metrics, makeHBs("b", "inc-b", 1, 2, 3, 4, 5))
	mustInsert(t, b.metrics, makeHBs("c", "inc-c", 1, 2, 3, 6, 7, 8, 9, 10))
	if err := b.metrics.AdvanceStreamPosition(t.Context(), store.StreamPosition{NodeID: "c", Incarnation: "inc-c", Seq: 10}); err != nil {
		t.Fatal(err)
	}
	n.start()

	waitFor(t, "a caught up with b", func() bool {
		st := statusOf(a.rep, "b")
		return !st.CaughtUpAt.IsZero() && a.metrics.position("c", "inc-c") == 10
	})

	if got := seqsString(a.metrics.seqs("b", "inc-b")); got != "[1 2 3 4 5]" {
		t.Errorf("a holds b's stream %s, want [1 2 3 4 5]", got)
	}
	if got := a.metrics.position("b", "inc-b"); got != 5 {
		t.Errorf("a position of b's stream = %d, want 5", got)
	}
	if got := seqsString(a.metrics.seqs("c", "inc-c")); got != "[1 2 3 6 7 8 9 10]" {
		t.Errorf("a holds c's stream %s, want [1 2 3 6 7 8 9 10]", got)
	}
	for _, limit := range b.metrics.fetchLimitsSeen() {
		if limit != 3 {
			t.Errorf("fetch limit %d, want page size 3", limit)
		}
	}
	if until := a.rep.ReplicatedUntil(); until.IsZero() {
		t.Error("ReplicatedUntil is zero after a complete round")
	}
	if st := statusOf(a.rep, "b"); st.LastError != "" {
		t.Errorf("LastError = %q, want empty", st.LastError)
	}
}

func TestPushDropIsRecoveredByAntiEntropy(t *testing.T) {
	n := newTestNet(t, Options{})
	a := n.add("a", newFakeMetrics("inc-a"), "b")
	b := n.add("b", newFakeMetrics("inc-b"), "a")
	n.setDown("b", true)
	n.start()
	waitFor(t, "worker for b exists", func() bool { return len(a.rep.Status()) == 1 })

	// While b is unreachable no push stream is open, so the batches are dropped.
	for _, seqs := range [][]uint64{{1, 2}, {3, 4}, {5, 6}} {
		hbs := makeHBs("a", "inc-a", seqs...)
		mustInsert(t, a.metrics, hbs)
		a.rep.Publish(hbs)
	}
	waitFor(t, "three batches counted as dropped", func() bool {
		return statusOf(a.rep, "b").Dropped == 3
	})

	// Anti-entropy is not periodic in this test: only the reconnect of the push
	// stream may trigger the catch-up of the dropped batches.
	n.setDown("b", false)
	waitFor(t, "b holds all of a's heartbeats", func() bool {
		return seqsString(b.metrics.seqs("a", "inc-a")) == "[1 2 3 4 5 6]"
	})
	if got := b.metrics.position("a", "inc-a"); got != 6 {
		t.Errorf("b position of a = %d, want 6", got)
	}
}

func TestSlowPeerDoesNotBlockOthers(t *testing.T) {
	n := newTestNet(t, Options{AntiEntropyInterval: 50 * time.Millisecond})
	a := n.add("a", newFakeMetrics("inc-a"), "b", "c")
	n.add("b", newFakeMetrics("inc-b"))
	c := n.add("c", newFakeMetrics("inc-c"), "a")
	n.stallPushes("b")
	n.start()

	const total = 1000
	payload := strings.Repeat("x", 1000)
	start := time.Now()
	for seq := uint64(1); seq <= total; seq++ {
		hb := makeHB("a", "inc-a", seq)
		hb.Message = payload
		mustInsert(t, a.metrics, []store.Heartbeat{hb})
		a.rep.Publish([]store.Heartbeat{hb})
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("publishing %d batches took %v; Publish must not block", total, elapsed)
	}
	if dropped := statusOf(a.rep, "b").Dropped; dropped == 0 {
		t.Errorf("stalled peer dropped %d batches, want > 0", dropped)
	}

	// The healthy peer converges, through pushes or anti-entropy.
	waitFor(t, "c holds all of a's heartbeats", func() bool {
		return len(c.metrics.seqs("a", "inc-a")) == total
	})
}

func TestRefreshStartsAndStopsWorkers(t *testing.T) {
	n := newTestNet(t, Options{})
	a := n.add("a", newFakeMetrics("inc-a"), "b")
	n.add("b", newFakeMetrics("inc-b"))
	n.add("c", newFakeMetrics("inc-c"))
	n.start()

	ids := func() string {
		var out []string
		for _, s := range a.rep.Status() {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	waitFor(t, "worker for b only", func() bool { return ids() == "b" })

	n.setPeers("a", "c")
	waitFor(t, "worker for c only", func() bool { return ids() == "c" })
}

func TestPeerStatusLag(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if _, ok := (PeerStatus{}).Lag(now); ok {
		t.Error("Lag of a never caught up peer reports ok")
	}
	st := PeerStatus{CaughtUpAt: now.Add(-90 * time.Second)}
	lag, ok := st.Lag(now)
	if !ok || lag != 90*time.Second {
		t.Errorf("Lag = %v, %v; want 90s, true", lag, ok)
	}
}

// Anti-entropy pages reach OnStored page by page, and only once stored (ADR 0021).
func TestFetchStreamOnStoredReceivesStoredPages(t *testing.T) {
	source := newFakeMetrics("inc-c")
	mustInsert(t, source, makeHBs("c", "inc-c", 1, 2, 3))
	client := serveServer(t, &Server{NodeID: "c", Metrics: source, identity: fixedIdentity("b")})

	var got []uint64
	r := New(Options{
		NodeID:   "b",
		Metrics:  newFakeMetrics("inc-b"),
		Peers:    func() []Peer { return nil },
		Dial:     func(string) (*grpc.ClientConn, error) { return nil, errUnreachable },
		PageSize: 2,
		OnStored: func(hbs []store.Heartbeat) {
			for _, h := range hbs {
				got = append(got, h.Seq)
			}
		},
	})
	w := newWorker(r, Peer{ID: "c"}, func() {})
	if err := w.fetchStream(context.Background(), client, streamKey{node: "c", inc: "inc-c"}, 0, 3); err != nil {
		t.Fatalf("fetchStream: %v", err)
	}
	if got := seqsString(got); got != "[1 2 3]" {
		t.Fatalf("OnStored seqs = %s, want [1 2 3]", got)
	}
}

// OnStored is not called for a page whose insert fails.
func TestFetchStreamOnStoredSkippedOnInsertError(t *testing.T) {
	source := newFakeMetrics("inc-c")
	mustInsert(t, source, makeHBs("c", "inc-c", 1, 2))
	client := serveServer(t, &Server{NodeID: "c", Metrics: source, identity: fixedIdentity("b")})

	var calls int
	r := New(Options{
		NodeID:   "b",
		Metrics:  failingInsert{newFakeMetrics("inc-b"), errUnreachable},
		Peers:    func() []Peer { return nil },
		Dial:     func(string) (*grpc.ClientConn, error) { return nil, errUnreachable },
		OnStored: func([]store.Heartbeat) { calls++ },
	})
	w := newWorker(r, Peer{ID: "c"}, func() {})
	if err := w.fetchStream(context.Background(), client, streamKey{node: "c", inc: "inc-c"}, 0, 2); err == nil {
		t.Fatal("fetchStream succeeded although the insert failed")
	}
	if calls != 0 {
		t.Fatalf("OnStored called %d times for a failed insert, want 0", calls)
	}
}

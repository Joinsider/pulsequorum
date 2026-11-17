// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package replication

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
)

// countingTrigger records how often each node was triggered.
type countingTrigger struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countingTrigger) Trigger(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = make(map[string]int)
	}
	c.n[nodeID]++
}

func (c *countingTrigger) count(nodeID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[nodeID]
}

func TestGetWatermarksReportsOwnStreamUpToLastSeq(t *testing.T) {
	m := newFakeMetrics("inc-b")
	// Own stream: 1, 2, 4, 5 stored, so the position is 2 but the last seq is 5.
	mustInsert(t, m, makeHBs("b", "inc-b", 1, 2, 4, 5))
	mustInsert(t, m, makeHBs("c", "inc-c", 1, 2, 3))

	srv := &Server{NodeID: "b", Metrics: m}
	resp, err := srv.GetWatermarks(context.Background(), &pb.GetWatermarksRequest{})
	if err != nil {
		t.Fatalf("GetWatermarks: %v", err)
	}
	got := make(map[string]uint64)
	for _, p := range resp.GetPositions() {
		got[p.GetNodeId()+"/"+p.GetIncarnation()] = p.GetSeq()
	}
	want := map[string]uint64{"b/inc-b": 5, "c/inc-c": 3}
	if len(got) != len(want) || got["b/inc-b"] != want["b/inc-b"] || got["c/inc-c"] != want["c/inc-c"] {
		t.Errorf("watermarks = %v, want %v", got, want)
	}
}

func TestFetchRangeCapsLimitAndHonoursUntil(t *testing.T) {
	tests := []struct {
		name      string
		limit     uint32
		until     uint64
		wantLimit int
		wantSeqs  string
	}{
		{name: "zero limit means maximum", limit: 0, until: 0, wantLimit: 5000, wantSeqs: "[1 2 3 4 5 6 7 8 9 10]"},
		{name: "limit above maximum is capped", limit: 10000, until: 0, wantLimit: 5000, wantSeqs: "[1 2 3 4 5 6 7 8 9 10]"},
		{name: "explicit limit is kept", limit: 3, until: 0, wantLimit: 3, wantSeqs: "[1 2 3]"},
		{name: "until bounds the range", limit: 100, until: 4, wantLimit: 100, wantSeqs: "[1 2 3 4]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newFakeMetrics("inc-b")
			mustInsert(t, m, makeHBs("c", "inc-c", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10))
			srv := &Server{NodeID: "b", Metrics: m}

			resp, err := srv.FetchRange(context.Background(), &pb.FetchRangeRequest{
				NodeId: "c", Incarnation: "inc-c", AfterSeq: 0, Limit: tt.limit, UntilSeq: tt.until,
			})
			if err != nil {
				t.Fatalf("FetchRange: %v", err)
			}
			var seqs []uint64
			for _, h := range resp.GetHeartbeats() {
				seqs = append(seqs, h.GetSeq())
			}
			if got := seqsString(seqs); got != tt.wantSeqs {
				t.Errorf("seqs = %s, want %s", got, tt.wantSeqs)
			}
			if got := m.fetchLimitsSeen(); len(got) != 1 || got[0] != tt.wantLimit {
				t.Errorf("store limit = %v, want %d", got, tt.wantLimit)
			}
		})
	}
}

func TestFetchRangeMoreFlagAndNoBound(t *testing.T) {
	m := newFakeMetrics("inc-b")
	mustInsert(t, m, makeHBs("c", "inc-c", 1, 2, 3, 4, 5))
	srv := &Server{NodeID: "b", Metrics: m}

	resp, err := srv.FetchRange(context.Background(), &pb.FetchRangeRequest{
		NodeId: "c", Incarnation: "inc-c", AfterSeq: 1, Limit: 2, UntilSeq: math.MaxUint64,
	})
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	if !resp.GetMore() || len(resp.GetHeartbeats()) != 2 {
		t.Errorf("more=%v with %d heartbeats, want more=true with 2", resp.GetMore(), len(resp.GetHeartbeats()))
	}
}

func TestFetchRangeRequiresStreamKey(t *testing.T) {
	srv := &Server{NodeID: "b", Metrics: newFakeMetrics("inc-b")}
	for _, req := range []*pb.FetchRangeRequest{
		{Incarnation: "inc-c"},
		{NodeId: "c"},
	} {
		_, err := srv.FetchRange(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("FetchRange(%v) code = %v, want InvalidArgument", req, status.Code(err))
		}
	}
}

func TestStreamHeartbeatsStoresOnlyValidHeartbeats(t *testing.T) {
	m := newFakeMetrics("inc-b")
	trig := &countingTrigger{}
	client := serveServer(t, &Server{NodeID: "b", Metrics: m, Triggers: trig, identity: fixedIdentity("a")})

	stream, err := client.StreamHeartbeats(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	valid := func(seq uint64) *pb.Heartbeat { return heartbeatToPB(makeHB("c", "inc-c", seq)) }
	invalid := []func(*pb.Heartbeat){
		func(h *pb.Heartbeat) { h.NodeId = "" },
		func(h *pb.Heartbeat) { h.Incarnation = "" },
		func(h *pb.Heartbeat) { h.TenantId = "" },
		func(h *pb.Heartbeat) { h.MonitorId = "" },
		func(h *pb.Heartbeat) { h.Seq = 0 },
		func(h *pb.Heartbeat) { h.Message = strings.Repeat("m", 1025) },
		func(h *pb.Heartbeat) { h.DetailsJson = []byte(`"` + strings.Repeat("d", 16<<10) + `"`) },
		func(h *pb.Heartbeat) { h.DetailsJson = []byte("{") },
		func(h *pb.Heartbeat) { h.NodeId, h.Incarnation = "b", "inc-b" }, // own current stream
	}
	msg := &pb.StreamHeartbeatsRequest{Heartbeats: []*pb.Heartbeat{valid(1), valid(2)}}
	for i, mutate := range invalid {
		h := valid(uint64(100 + i))
		mutate(h)
		msg.Heartbeats = append(msg.Heartbeats, h)
	}
	if err := stream.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	if got := seqsString(m.seqs("c", "inc-c")); got != "[1 2]" {
		t.Errorf("stored c seqs = %s, want [1 2]", got)
	}
	if got := m.seqs("b", "inc-b"); len(got) != 0 {
		t.Errorf("own stream was written: %v", got)
	}
	persisted := positionsFromPB(resp.GetPersisted())
	if len(persisted) != 1 || persisted[0].NodeID != "c" || persisted[0].Seq != 2 {
		t.Errorf("persisted = %+v, want c/inc-c at 2", persisted)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
}

func TestStreamHeartbeatsRejectsOversizedBatch(t *testing.T) {
	client := serveServer(t, &Server{NodeID: "b", Metrics: newFakeMetrics("inc-b"), identity: fixedIdentity("a")})
	stream, err := client.StreamHeartbeats(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	req := &pb.StreamHeartbeatsRequest{}
	for i := 0; i <= maxPushBatch; i++ {
		req.Heartbeats = append(req.Heartbeats, heartbeatToPB(makeHB("c", "inc-c", uint64(i+1))))
	}
	if err := stream.Send(req); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("recv code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestStreamHeartbeatsRequiresCertificate(t *testing.T) {
	srv := &Server{NodeID: "b", Metrics: newFakeMetrics("inc-b"), identity: func(context.Context) (pki.Identity, bool) {
		return pki.Identity{}, false
	}}
	client := serveServer(t, srv)
	stream, err := client.StreamHeartbeats(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	_ = stream.Send(&pb.StreamHeartbeatsRequest{})
	if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Errorf("recv code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestStreamHeartbeatsTriggersOncePerStream(t *testing.T) {
	trig := &countingTrigger{}
	client := serveServer(t, &Server{NodeID: "b", Metrics: newFakeMetrics("inc-b"), Triggers: trig, identity: fixedIdentity("a")})

	for stream := 1; stream <= 2; stream++ {
		s, err := client.StreamHeartbeats(context.Background())
		if err != nil {
			t.Fatalf("open stream: %v", err)
		}
		// Three messages on one stream must trigger only once.
		for range 3 {
			if err := s.Send(&pb.StreamHeartbeatsRequest{}); err != nil {
				t.Fatalf("send: %v", err)
			}
			if _, err := s.Recv(); err != nil {
				t.Fatalf("recv: %v", err)
			}
		}
		if got := trig.count("a"); got != stream {
			t.Errorf("after stream %d: triggers = %d, want %d", stream, got, stream)
		}
		if err := s.CloseSend(); err != nil {
			t.Fatalf("close send: %v", err)
		}
	}
}

// failingInsert fails every InsertHeartbeats call.
type failingInsert struct {
	*fakeMetrics
	err error
}

func (m failingInsert) InsertHeartbeats(context.Context, []store.Heartbeat) (int, error) {
	return 0, m.err
}

// OnStored receives exactly the validated heartbeats that were stored (ADR 0021).
func TestStreamHeartbeatsOnStoredReceivesStoredOnly(t *testing.T) {
	m := newFakeMetrics("inc-b")
	var got []store.Heartbeat
	srv := &Server{NodeID: "b", Metrics: m, identity: fixedIdentity("a"),
		OnStored: func(hbs []store.Heartbeat) { got = append(got, hbs...) }}
	client := serveServer(t, srv)

	stream, err := client.StreamHeartbeats(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	invalid := heartbeatToPB(makeHB("c", "inc-c", 3))
	invalid.TenantId = ""
	msg := &pb.StreamHeartbeatsRequest{Heartbeats: []*pb.Heartbeat{
		heartbeatToPB(makeHB("c", "inc-c", 1)), invalid, heartbeatToPB(makeHB("c", "inc-c", 2)),
	}}
	if err := stream.Send(msg); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("OnStored got %d heartbeats, want seqs [1 2]", len(got))
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}
}

// OnStored is not called when the insert fails.
func TestStreamHeartbeatsOnStoredSkippedOnInsertError(t *testing.T) {
	var calls int
	srv := &Server{NodeID: "b", Metrics: failingInsert{newFakeMetrics("inc-b"), errors.New("disk full")},
		identity: fixedIdentity("a"), OnStored: func([]store.Heartbeat) { calls++ }}
	client := serveServer(t, srv)

	stream, err := client.StreamHeartbeats(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(&pb.StreamHeartbeatsRequest{Heartbeats: []*pb.Heartbeat{heartbeatToPB(makeHB("c", "inc-c", 1))}}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("recv succeeded although the insert failed")
	}
	if calls != 0 {
		t.Fatalf("OnStored called %d times for a failed insert, want 0", calls)
	}
}

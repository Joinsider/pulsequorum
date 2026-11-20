// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package replication

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/clusternet"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Limits of the replication protocol. Oversized heartbeats are dropped, oversized
// batches end the stream (FA-REP-01, ADR 0006).
const (
	maxFetchLimit   = 5000
	maxPushBatch    = 1000
	maxMessageBytes = 1024
	maxDetailsBytes = 16 << 10
)

// Server implements pb.ReplicationServiceServer on the cluster port. Only voters
// may call it; the interceptor of clusternet enforces this.
type Server struct {
	pb.UnimplementedReplicationServiceServer

	// NodeID is the ID of this node; its own stream is authoritative locally.
	NodeID  string
	Metrics store.Metrics
	// Triggers is notified once per opened push stream with the caller's node ID,
	// because pushes may have been lost while the stream was down (ADR 0006,
	// ADR 0012 B6). It may be nil.
	Triggers interface{ Trigger(nodeID string) }
	// OnStored is called with the validated heartbeats of a push after they were
	// stored, so that the leader can evaluate them (ADR 0021). It must not
	// block, and the slice is only valid during the call. It may be nil.
	OnStored func([]store.Heartbeat)
	Log      *slog.Logger

	// identity returns the verified caller. nil means clusternet.Peer; tests
	// replace it because they run without mTLS.
	identity func(context.Context) (pki.Identity, bool)
}

// GetWatermarks reports the positions of all known streams. The own current
// stream is reported with its last stored seq, since this node is its producer
// and a seq it lost was never stored anywhere (ADR 0012 B2).
func (s *Server) GetWatermarks(ctx context.Context, _ *pb.GetWatermarksRequest) (*pb.GetWatermarksResponse, error) {
	positions, err := s.Metrics.StreamPositions(ctx)
	if err != nil {
		return nil, s.storeErr("read positions", err)
	}
	own := s.Metrics.Incarnation()
	last, err := s.Metrics.LastSeq(ctx, s.NodeID, own)
	if err != nil {
		return nil, s.storeErr("read own sequence", err)
	}
	found := false
	for i := range positions {
		if positions[i].NodeID == s.NodeID && positions[i].Incarnation == own {
			positions[i].Seq = last
			found = true
		}
	}
	if !found && last > 0 {
		positions = append(positions, store.StreamPosition{NodeID: s.NodeID, Incarnation: own, Seq: last})
	}
	return &pb.GetWatermarksResponse{Positions: positionsToPB(positions)}, nil
}

// FetchRange returns up to limit heartbeats of one stream with seq in
// (after_seq, until_seq]. A limit of 0 or above 5000 means 5000; until_seq 0
// means no bound.
func (s *Server) FetchRange(ctx context.Context, req *pb.FetchRangeRequest) (*pb.FetchRangeResponse, error) {
	if req.GetNodeId() == "" || req.GetIncarnation() == "" {
		return nil, status.Error(codes.InvalidArgument, "node_id and incarnation are required")
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > maxFetchLimit {
		limit = maxFetchLimit
	}
	until := req.GetUntilSeq()
	if until == 0 {
		until = math.MaxUint64
	}
	hbs, more, err := s.Metrics.HeartbeatsAfter(ctx, req.GetNodeId(), req.GetIncarnation(), req.GetAfterSeq(), until, limit)
	if err != nil {
		return nil, s.storeErr("fetch range", err)
	}
	return &pb.FetchRangeResponse{Heartbeats: heartbeatsToPB(hbs), More: more}, nil
}

// StreamHeartbeats stores the heartbeats pushed by a voter and answers every
// message with the persisted positions of the streams in that message.
// Invalid heartbeats are dropped; anti-entropy never brings them back, so they
// must not be able to poison a batch.
func (s *Server) StreamHeartbeats(stream pb.ReplicationService_StreamHeartbeatsServer) error {
	ctx := stream.Context()
	caller, ok := s.caller(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "client certificate required")
	}
	if s.Triggers != nil {
		s.Triggers.Trigger(caller.NodeID)
	}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n := len(req.GetHeartbeats()); n > maxPushBatch {
			return status.Errorf(codes.InvalidArgument, "batch of %d heartbeats exceeds %d", n, maxPushBatch)
		}
		resp, err := s.persist(ctx, req)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *Server) persist(ctx context.Context, req *pb.StreamHeartbeatsRequest) (*pb.StreamHeartbeatsResponse, error) {
	own := s.Metrics.Incarnation()
	touched := make(map[streamKey]bool)
	valid := make([]store.Heartbeat, 0, len(req.GetHeartbeats()))
	for _, p := range req.GetHeartbeats() {
		h := heartbeatFromPB(p)
		touched[streamKey{node: h.NodeID, inc: h.Incarnation}] = true
		if reason := rejectReason(h, s.NodeID, own); reason != "" {
			s.logger().Debug("dropped heartbeat", "reason", reason, "node", h.NodeID, "seq", h.Seq)
			continue
		}
		valid = append(valid, h)
	}
	if len(valid) > 0 {
		if _, err := s.Metrics.InsertHeartbeats(ctx, valid); err != nil {
			return nil, s.storeErr("insert heartbeats", err)
		}
		if s.OnStored != nil {
			s.OnStored(valid)
		}
	}
	positions, err := s.Metrics.StreamPositions(ctx)
	if err != nil {
		return nil, s.storeErr("read positions", err)
	}
	persisted := make([]store.StreamPosition, 0, len(touched))
	for _, p := range positions {
		if touched[streamKey{node: p.NodeID, inc: p.Incarnation}] {
			persisted = append(persisted, p)
		}
	}
	return &pb.StreamHeartbeatsResponse{Persisted: positionsToPB(persisted)}, nil
}

// rejectReason returns why a heartbeat from a peer (pushed or fetched) is
// refused, or "" if it is valid. Nobody but this node writes its own current
// stream (FA-REP-01).
func rejectReason(h store.Heartbeat, selfNode, own string) string {
	switch {
	case h.NodeID == "" || h.Incarnation == "":
		return "missing stream key"
	case h.TenantID == "" || h.MonitorID == "":
		return "missing tenant or monitor"
	case h.Seq == 0:
		return "seq 0"
	case h.NodeID == selfNode && h.Incarnation == own:
		return "write to own stream"
	case len(h.Message) > maxMessageBytes:
		return "message too large"
	case len(h.Details) > maxDetailsBytes:
		return "details too large"
	case !json.Valid(h.Details):
		return "details are not JSON"
	}
	return ""
}

func (s *Server) caller(ctx context.Context) (pki.Identity, bool) {
	if s.identity != nil {
		return s.identity(ctx)
	}
	return clusternet.Peer(ctx)
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) storeErr(op string, err error) error {
	s.logger().Warn("replication store failed", "op", op, "err", err)
	return status.Errorf(codes.Unavailable, "%s: %v", op, err)
}

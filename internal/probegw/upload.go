// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	maxUploadBatch  = 500
	maxMessageBytes = 1024
	maxDetailsBytes = 16 << 10
	maxFutureSkew   = 5 * time.Minute
	pastWindowGrace = time.Hour
)

// UploadHeartbeats receives buffered and live heartbeats from a probe,
// validates caller identity and tenant boundaries on every entry, persists
// valid heartbeats, and responds with the contiguous StreamPosition.
func (s *Server) UploadHeartbeats(stream pb.ProbeService_UploadHeartbeatsServer) error {
	ctx := stream.Context()
	caller, _, err := s.verifyProbe(ctx)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	token := s.sessions.registerUpload(caller.NodeID, s.now().UnixMilli(), cancel)
	defer s.sessions.unregisterUpload(caller.NodeID, token)

	probeBucket := newTokenBucket(max(defaultPerProbeMinRate, 2*s.sessions.expectedRate(caller.NodeID)), float64(s.opts.PerProbeBurst), s.opts.Clock)

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		hbs := req.GetHeartbeats()
		if len(hbs) > maxUploadBatch {
			return status.Errorf(codes.InvalidArgument, "batch of %d heartbeats exceeds %d", len(hbs), maxUploadBatch)
		}
		if len(hbs) > 0 {
			probeBucket.setRate(max(defaultPerProbeMinRate, 2*s.sessions.expectedRate(caller.NodeID)))
			if err := probeBucket.wait(sctx, len(hbs)); err != nil {
				return err
			}
			if err := s.global.wait(sctx, len(hbs)); err != nil {
				return err
			}
		}

		resp, err := s.processBatch(sctx, caller, req)
		if err != nil {
			return err
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
		s.sessions.touch(caller.NodeID, s.now().UnixMilli())
	}
}

type seqRef struct {
	inc string
	seq uint64
}

func (s *Server) processBatch(ctx context.Context, caller pki.Identity, req *pb.UploadHeartbeatsRequest) (*pb.UploadHeartbeatsResponse, error) {
	nowMs := s.now().UnixMilli()
	minTs := nowMs - (s.opts.BufferMaxAge + pastWindowGrace).Milliseconds()
	maxTs := nowMs + maxFutureSkew.Milliseconds()

	var (
		valid     = make([]store.Heartbeat, 0, len(req.GetHeartbeats()))
		discarded []seqRef
		batchInc  = s.sessions.incarnation(caller.NodeID)
	)

	for _, p := range req.GetHeartbeats() {
		hb, disc, err := s.validateUploadedHeartbeat(ctx, caller, p, minTs, maxTs)
		if err != nil {
			return nil, err
		}
		if batchInc == "" {
			batchInc = p.GetIncarnation()
		}
		if disc != nil {
			discarded = append(discarded, *disc)
			continue
		}
		valid = append(valid, hb)
	}

	if err := s.commitUploadedBatch(ctx, caller.NodeID, batchInc, req.GetLostBelow(), valid, discarded); err != nil {
		return nil, err
	}

	return &pb.UploadHeartbeatsResponse{
		Persisted: s.streamPositionFor(ctx, caller.NodeID, batchInc),
	}, nil
}

func (s *Server) validateUploadedHeartbeat(ctx context.Context, caller pki.Identity, p *pb.Heartbeat, minTs, maxTs int64) (store.Heartbeat, *seqRef, error) {
	if err := checkBatchIdentity(caller, p); err != nil {
		s.rejected("upload_identity")
		s.log.Warn("probe upload batch rejected: identity or tenant violation", "node_id", caller.NodeID, "cert_tenant", caller.TenantID, "hb_node", p.GetNodeId(), "hb_tenant", p.GetTenantId())
		return store.Heartbeat{}, nil, err
	}
	if p.GetIncarnation() == "" || p.GetMonitorId() == "" || p.GetSeq() == 0 {
		return store.Heartbeat{}, nil, status.Error(codes.InvalidArgument, "heartbeat requires incarnation, monitor_id and positive seq")
	}
	if p.GetTsMs() < minTs || p.GetTsMs() > maxTs {
		return store.Heartbeat{}, nil, status.Error(codes.InvalidArgument, "heartbeat timestamp outside allowed window")
	}
	details := bytes.Clone(p.GetDetailsJson())
	if len(details) == 0 {
		details = []byte("{}")
	}
	if len(p.GetMessage()) > maxMessageBytes || len(details) > maxDetailsBytes || !json.Valid(details) {
		return store.Heartbeat{}, nil, status.Error(codes.InvalidArgument, "heartbeat message or details invalid or too large")
	}

	if _, err := s.opts.State.GetMonitor(ctx, p.GetTenantId(), p.GetMonitorId()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// ADR 0035 D4: deleted monitors are silently discarded while still
			// advancing the stream position so the buffer does not stall.
			return store.Heartbeat{}, &seqRef{inc: p.GetIncarnation(), seq: p.GetSeq()}, nil
		}
		return store.Heartbeat{}, nil, status.Errorf(codes.Unavailable, "check monitor: %v", err)
	}

	return store.Heartbeat{
		NodeID:      caller.NodeID,
		Incarnation: p.GetIncarnation(),
		Seq:         p.GetSeq(),
		TenantID:    p.GetTenantId(),
		MonitorID:   p.GetMonitorId(),
		TsMs:        p.GetTsMs(),
		Status:      store.CheckStatus(p.GetStatus()),
		LatencyMs:   int(p.GetLatencyMs()),
		ErrorClass:  cluster.ErrorClassFromProto(p.GetErrorClass()),
		Message:     p.GetMessage(),
		Details:     details,
	}, nil, nil
}

func (s *Server) commitUploadedBatch(ctx context.Context, nodeID, batchInc string, lost uint64, valid []store.Heartbeat, discarded []seqRef) error {
	if lost > 1 && batchInc != "" {
		// Scoped strictly to caller.NodeID (ADR 0034 section 6, ADR 0035 D4).
		if err := s.opts.Metrics.AdvanceStreamPosition(ctx, store.StreamPosition{
			NodeID:      nodeID,
			Incarnation: batchInc,
			Seq:         lost - 1,
		}); err != nil {
			return status.Errorf(codes.Unavailable, "advance lost position: %v", err)
		}
	}
	if len(valid) > 0 {
		if _, err := s.opts.Metrics.InsertHeartbeats(ctx, valid); err != nil {
			return status.Errorf(codes.Unavailable, "store heartbeats: %v", err)
		}
		if s.opts.OnStored != nil {
			s.opts.OnStored(valid)
		}
		if s.opts.Publish != nil {
			s.opts.Publish(valid)
		}
	}
	if err := s.advanceDiscarded(ctx, nodeID, discarded); err != nil {
		return status.Errorf(codes.Unavailable, "advance discarded positions: %v", err)
	}
	return nil
}

func checkBatchIdentity(caller pki.Identity, p *pb.Heartbeat) error {
	if p.GetNodeId() != caller.NodeID {
		return status.Error(codes.PermissionDenied, "heartbeat node_id does not match caller certificate")
	}
	if p.GetTenantId() == "" || p.GetTenantId() == store.SystemTenantID {
		return status.Error(codes.PermissionDenied, "invalid heartbeat tenant_id")
	}
	if caller.TenantID != "" && p.GetTenantId() != caller.TenantID {
		return status.Error(codes.PermissionDenied, "heartbeat tenant_id does not match caller certificate")
	}
	return nil
}

func (s *Server) advanceDiscarded(ctx context.Context, nodeID string, refs []seqRef) error {
	if len(refs) == 0 {
		return nil
	}
	byInc := make(map[string][]uint64)
	for _, ref := range refs {
		byInc[ref.inc] = append(byInc[ref.inc], ref.seq)
	}
	for inc, seqs := range byInc {
		if err := s.advanceDiscardedForIncarnation(ctx, nodeID, inc, seqs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) advanceDiscardedForIncarnation(ctx context.Context, nodeID, inc string, seqs []uint64) error {
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	cur := s.streamPositionFor(ctx, nodeID, inc).GetSeq()
	next, err := s.walkDiscardedSeqs(ctx, nodeID, inc, cur, seqs)
	if err != nil {
		return err
	}
	next, err = s.walkTrailingStoredSeqs(ctx, nodeID, inc, next)
	if err != nil {
		return err
	}
	if next <= cur {
		return nil
	}
	return s.opts.Metrics.AdvanceStreamPosition(ctx, store.StreamPosition{
		NodeID:      nodeID,
		Incarnation: inc,
		Seq:         next,
	})
}

func (s *Server) walkDiscardedSeqs(ctx context.Context, nodeID, inc string, next uint64, seqs []uint64) (uint64, error) {
	for _, seq := range seqs {
		if seq <= next {
			continue
		}
		if seq == next+1 {
			next = seq
			continue
		}
		// There might be stored heartbeats between next and seq.
		hbs, _, err := s.opts.Metrics.HeartbeatsAfter(ctx, nodeID, inc, next, seq-1, int(min(seq-next-1, 5000)))
		if err != nil {
			return 0, err
		}
		next, _ = advanceContiguousSeq(next, hbs)
		if seq != next+1 {
			break
		}
		next = seq
	}
	return next, nil
}

func (s *Server) walkTrailingStoredSeqs(ctx context.Context, nodeID, inc string, next uint64) (uint64, error) {
	// Advance past any contiguous stored heartbeats immediately following the
	// discarded sequence numbers (e.g. [1 stored, 2 deleted, 3 stored] -> 3).
	for {
		hbs, more, err := s.opts.Metrics.HeartbeatsAfter(ctx, nodeID, inc, next, 0, 5000)
		if err != nil {
			return 0, err
		}
		var fullBatch bool
		prev := next
		next, fullBatch = advanceContiguousSeq(next, hbs)
		if !more || !fullBatch || next == prev {
			return next, nil
		}
	}
}

func advanceContiguousSeq(next uint64, hbs []store.Heartbeat) (uint64, bool) {
	for _, h := range hbs {
		if h.Seq != next+1 {
			return next, false
		}
		next = h.Seq
	}
	return next, true
}

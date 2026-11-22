// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	probeProtocolVersion = 1
	maxSnapshotChunk     = 500
	monitorPageSize      = 1000
	registerProbeTimeout = 10 * time.Second
)

type resolvedMonitor struct {
	hash  string
	proto *pb.ProbeMonitor
	rate  float64
}

// Subscribe streams the monitors assigned to the calling probe, starting with a
// full snapshot followed by debounced deltas and periodic keepalive ticks.
func (s *Server) Subscribe(req *pb.SubscribeRequest, stream pb.ProbeService_SubscribeServer) error {
	if req.GetProtocolVersion() != probeProtocolVersion {
		return status.Errorf(codes.FailedPrecondition, "unsupported probe protocol version %d", req.GetProtocolVersion())
	}
	ctx := stream.Context()
	caller, level, err := s.verifyProbe(ctx)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	token := s.sessions.registerSubscribe(caller.NodeID, req.GetIncarnation(), s.now().UnixMilli(), req.GetBuffer(), cancel)
	defer s.sessions.unregisterSubscribe(caller.NodeID, token)

	if level < cluster.ProbeCommandLevel {
		s.log.Warn("cluster command level below 10; serving empty assignment to probe", "node_id", caller.NodeID, "level", level)
		return s.serveBelowLevel10(sctx, caller, req, stream)
	}
	s.syncProbeRegistration(sctx, caller, req)

	snap, voters, err := s.buildSnapshot(sctx, caller)
	if err != nil {
		return err
	}
	s.sessions.setExpectedRate(caller.NodeID, snap.totalRate)

	lastHashes := make(map[string]string, len(snap.monitors))
	upserts := make([]*pb.ProbeMonitor, 0, len(snap.monitors))
	for _, id := range slices.Sorted(maps.Keys(snap.monitors)) {
		rm := snap.monitors[id]
		lastHashes[id] = rm.hash
		upserts = append(upserts, rm.proto)
	}
	lastVoters := voterSignature(voters)

	if err := s.sendSnapshot(sctx, caller.NodeID, req.GetIncarnation(), snap.revision, upserts, voters, stream); err != nil {
		return err
	}
	return s.runSubscribeLoop(sctx, caller, lastHashes, lastVoters, stream)
}

func (s *Server) serveBelowLevel10(ctx context.Context, caller pki.Identity, req *pb.SubscribeRequest, stream pb.ProbeService_SubscribeServer) error {
	voters := s.currentVoters()
	if err := s.sendSnapshot(ctx, caller.NodeID, req.GetIncarnation(), 0, nil, voters, stream); err != nil {
		return err
	}
	ticker := time.NewTicker(s.opts.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			resp := s.tickResponse(ctx, nil)
			if err := stream.Send(resp); err != nil {
				return err
			}
			s.sessions.touch(caller.NodeID, s.now().UnixMilli())
		}
	}
}

func (s *Server) runSubscribeLoop(
	ctx context.Context,
	caller pki.Identity,
	lastHashes map[string]string,
	lastVoters string,
	stream pb.ProbeService_SubscribeServer,
) error {
	var ch <-chan struct{}
	if s.opts.SubscribeChanges != nil {
		var unsub func()
		ch, unsub = s.opts.SubscribeChanges()
		defer unsub()
	}
	tick := time.NewTicker(s.opts.TickInterval)
	defer tick.Stop()
	poll := time.NewTicker(fallbackPollInterval)
	defer poll.Stop()

	var debounce *time.Timer
	var debounceC <-chan time.Time
	scheduleRecompute := func() {
		if debounce == nil {
			debounce = time.NewTimer(s.opts.DebounceInterval)
			debounceC = debounce.C
			return
		}
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(s.opts.DebounceInterval)
		debounceC = debounce.C
	}
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			scheduleRecompute()
		case <-poll.C:
			scheduleRecompute()
		case <-debounceC:
			debounceC = nil
			if _, _, err := s.verifyProbe(ctx); err != nil {
				return err
			}
			snap, voters, err := s.buildSnapshot(ctx, caller)
			if err != nil {
				s.log.Warn("recomputing probe snapshot failed", "node_id", caller.NodeID, "err", err)
				continue
			}
			s.sessions.setExpectedRate(caller.NodeID, snap.totalRate)
			var voterDelta []*pb.NodeInfo
			if sig := voterSignature(voters); sig != lastVoters {
				lastVoters = sig
				voterDelta = voters
			}
			upserts, removed, nextHashes := diffMonitors(lastHashes, snap.monitors)
			if len(upserts) == 0 && len(removed) == 0 && len(voterDelta) == 0 {
				continue
			}
			if err := s.sendDeltas(ctx, snap.revision, upserts, removed, voterDelta, stream); err != nil {
				return err
			}
			lastHashes = nextHashes
			s.sessions.touch(caller.NodeID, s.now().UnixMilli())
		case <-tick.C:
			var voterDelta []*pb.NodeInfo
			voters := s.currentVoters()
			if sig := voterSignature(voters); sig != lastVoters {
				lastVoters = sig
				voterDelta = voters
			}
			if err := stream.Send(s.tickResponse(ctx, voterDelta)); err != nil {
				return err
			}
			s.sessions.touch(caller.NodeID, s.now().UnixMilli())
		}
	}
}

func (s *Server) syncProbeRegistration(ctx context.Context, caller pki.Identity, req *pb.SubscribeRequest) {
	nodes, err := s.opts.State.ListNodes(ctx)
	if err != nil {
		return
	}
	var existing *store.Node
	for i := range nodes {
		if nodes[i].ID == caller.NodeID {
			existing = &nodes[i]
			break
		}
	}
	if existing == nil {
		return
	}
	caps := slices.Clone(req.GetCapabilities())
	slices.Sort(caps)
	caps = slices.Compact(caps)
	labels := maps.Clone(req.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	same := existing.Name == req.GetName() &&
		existing.Region == req.GetRegion() &&
		existing.Version == req.GetVersion() &&
		existing.Incarnation == req.GetIncarnation() &&
		slices.Equal(existing.Capabilities, caps) &&
		maps.Equal(existing.Labels, labels)
	if same || s.opts.Cluster == nil {
		return
	}
	cmd := cluster.NewRegisterProbe(uuid.NewString(), store.Node{
		ID:            caller.NodeID,
		Name:          req.GetName(),
		Role:          store.NodeRoleProbe,
		OwnerTenantID: caller.TenantID,
		Incarnation:   req.GetIncarnation(),
		Region:        req.GetRegion(),
		Labels:        labels,
		Capabilities:  caps,
		Version:       req.GetVersion(),
	})
	callCtx, cancel := context.WithTimeout(ctx, registerProbeTimeout)
	defer cancel()
	if res, err := s.opts.Cluster.Apply(callCtx, cmd); err != nil || res.Err != nil {
		if err == nil {
			err = res.Err
		}
		s.log.Warn("updating probe metadata in registry failed", "node_id", caller.NodeID, "err", err)
	}
}

type snapshot struct {
	revision  uint64
	monitors  map[string]resolvedMonitor
	totalRate float64
}

func (s *Server) buildSnapshot(ctx context.Context, caller pki.Identity) (snapshot, []*pb.NodeInfo, error) {
	rev, err := s.opts.State.AppliedIndex(ctx)
	if err != nil {
		return snapshot{}, nil, status.Errorf(codes.Unavailable, "read applied index: %v", err)
	}
	tenants, err := s.opts.State.ListTenants(ctx)
	if err != nil {
		return snapshot{}, nil, status.Errorf(codes.Unavailable, "list tenants: %v", err)
	}
	registry, err := s.opts.State.ListNodes(ctx)
	if err != nil {
		return snapshot{}, nil, status.Errorf(codes.Unavailable, "list nodes: %v", err)
	}
	out := snapshot{revision: rev, monitors: make(map[string]resolvedMonitor)}
	for _, t := range tenants {
		if t.Suspended || t.ID == store.SystemTenantID {
			continue
		}
		if caller.TenantID != "" && t.ID != caller.TenantID {
			continue
		}
		if err := s.collectTenantMonitors(ctx, caller, t.ID, registry, &out); err != nil {
			return snapshot{}, nil, err
		}
	}
	return out, s.currentVoters(), nil
}

func (s *Server) collectTenantMonitors(ctx context.Context, caller pki.Identity, tenantID string, registry []store.Node, out *snapshot) error {
	after := ""
	for {
		page, err := s.opts.State.ListMonitors(ctx, tenantID, store.MonitorQuery{AfterID: after, Limit: monitorPageSize})
		if err != nil {
			return status.Errorf(codes.Unavailable, "list monitors for tenant %s: %v", tenantID, err)
		}
		for _, m := range page.Items {
			// Defense-in-depth D3: never deliver a foreign tenant's monitor to a private probe.
			if caller.TenantID != "" && m.TenantID != caller.TenantID {
				s.rejected("foreign_monitor")
				s.log.Error("tenant boundary check dropped foreign monitor for private probe", "node_id", caller.NodeID, "probe_tenant", caller.TenantID, "monitor_tenant", m.TenantID, "monitor_id", m.ID)
				continue
			}
			if m.Paused || m.IntervalSeconds <= 0 {
				continue
			}
			if !s.opts.Assigner.Runs(m, registry, caller.NodeID) {
				continue
			}
			cfg, err := configsecret.ResolveMonitor(ctx, s.opts.Keyring, m)
			if err != nil {
				s.log.Warn("resolving monitor secrets for probe failed", "node_id", caller.NodeID, "monitor_id", m.ID, "err", err)
				continue
			}
			pm := &pb.ProbeMonitor{
				Id:              m.ID,
				TenantId:        m.TenantID,
				Type:            m.Type,
				ConfigJson:      cfg,
				IntervalSeconds: uint32(m.IntervalSeconds),        //nolint:gosec // G115: validated positive
				TimeoutSeconds:  uint32(max(m.TimeoutSeconds, 0)), //nolint:gosec // G115: clamped non-negative
				SsrfProtection:  s.opts.SSRFProtection,
			}
			out.monitors[m.ID] = resolvedMonitor{
				hash:  hashProbeMonitor(pm),
				proto: pm,
				rate:  1.0 / float64(m.IntervalSeconds),
			}
			out.totalRate += 1.0 / float64(m.IntervalSeconds)
		}
		if page.NextAfterID == "" {
			return nil
		}
		after = page.NextAfterID
	}
}

func (s *Server) sendSnapshot(
	ctx context.Context,
	nodeID, incarnation string,
	revision uint64,
	upserts []*pb.ProbeMonitor,
	voters []*pb.NodeInfo,
	stream pb.ProbeService_SubscribeServer,
) error {
	pos := s.streamPositionFor(ctx, nodeID, incarnation)
	activeCA, bundleFPs := s.caInfo(ctx)
	nowMs := s.now().UnixMilli()

	if len(upserts) == 0 {
		return stream.Send(&pb.SubscribeResponse{
			Revision:                revision,
			FullSnapshot:            true,
			StreamPosition:          pos,
			Voters:                  voters,
			ServerTimeMs:            nowMs,
			ActiveCaFingerprint:     activeCA,
			TrustBundleFingerprints: bundleFPs,
		})
	}
	for i := 0; i < len(upserts); i += maxSnapshotChunk {
		end := min(i+maxSnapshotChunk, len(upserts))
		resp := &pb.SubscribeResponse{
			Revision: revision,
			Upserts:  upserts[i:end],
		}
		if i == 0 {
			resp.FullSnapshot = true
			resp.StreamPosition = pos
			resp.Voters = voters
			resp.ServerTimeMs = nowMs
			resp.ActiveCaFingerprint = activeCA
			resp.TrustBundleFingerprints = bundleFPs
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) sendDeltas(
	ctx context.Context,
	revision uint64,
	upserts []*pb.ProbeMonitor,
	removed []string,
	voters []*pb.NodeInfo,
	stream pb.ProbeService_SubscribeServer,
) error {
	activeCA, bundleFPs := s.caInfo(ctx)
	nowMs := s.now().UnixMilli()
	if len(upserts) == 0 {
		return stream.Send(&pb.SubscribeResponse{
			Revision:                revision,
			RemovedMonitorIds:       removed,
			Voters:                  voters,
			ServerTimeMs:            nowMs,
			ActiveCaFingerprint:     activeCA,
			TrustBundleFingerprints: bundleFPs,
		})
	}
	for i := 0; i < len(upserts); i += maxSnapshotChunk {
		end := min(i+maxSnapshotChunk, len(upserts))
		resp := &pb.SubscribeResponse{
			Revision: revision,
			Upserts:  upserts[i:end],
		}
		if i == 0 {
			resp.RemovedMonitorIds = removed
			resp.Voters = voters
			resp.ServerTimeMs = nowMs
			resp.ActiveCaFingerprint = activeCA
			resp.TrustBundleFingerprints = bundleFPs
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) tickResponse(ctx context.Context, voters []*pb.NodeInfo) *pb.SubscribeResponse {
	activeCA, bundleFPs := s.caInfo(ctx)
	return &pb.SubscribeResponse{
		Voters:                  voters,
		ServerTimeMs:            s.now().UnixMilli(),
		ActiveCaFingerprint:     activeCA,
		TrustBundleFingerprints: bundleFPs,
	}
}

func (s *Server) streamPositionFor(ctx context.Context, nodeID, incarnation string) *pb.StreamPosition {
	if s.opts.Metrics == nil || incarnation == "" {
		return &pb.StreamPosition{NodeId: nodeID, Incarnation: incarnation}
	}
	positions, err := s.opts.Metrics.StreamPositions(ctx)
	if err != nil {
		return &pb.StreamPosition{NodeId: nodeID, Incarnation: incarnation}
	}
	for _, p := range positions {
		if p.NodeID == nodeID && p.Incarnation == incarnation {
			return &pb.StreamPosition{NodeId: nodeID, Incarnation: incarnation, Seq: p.Seq}
		}
	}
	return &pb.StreamPosition{NodeId: nodeID, Incarnation: incarnation}
}

func (s *Server) currentVoters() []*pb.NodeInfo {
	if s.opts.Cluster == nil {
		return nil
	}
	members, _, err := s.opts.Cluster.Members()
	if err != nil {
		return nil
	}
	var out []*pb.NodeInfo
	for _, m := range members {
		if m.Voter && m.Address != "" {
			out = append(out, &pb.NodeInfo{Id: m.ID, AdvertiseAddr: m.Address, Role: pb.NodeInfo_ROLE_VOTER})
		}
	}
	slices.SortFunc(out, func(a, b *pb.NodeInfo) int {
		if a.GetId() < b.GetId() {
			return -1
		}
		if a.GetId() > b.GetId() {
			return 1
		}
		return 0
	})
	return out
}

func (s *Server) caInfo(ctx context.Context) (active string, bundle []string) {
	if s.opts.Bundle == nil {
		return "", nil
	}
	certs, _, err := s.opts.Bundle(ctx)
	if err != nil || len(certs) == 0 {
		return "", nil
	}
	active = pki.Fingerprint(certs[0])
	bundle = make([]string, 0, len(certs))
	for _, c := range certs {
		bundle = append(bundle, pki.Fingerprint(c))
	}
	slices.Sort(bundle)
	return active, slices.Compact(bundle)
}

func voterSignature(voters []*pb.NodeInfo) string {
	var buf []byte
	for _, v := range voters {
		buf = append(buf, v.GetId()...)
		buf = append(buf, '=')
		buf = append(buf, v.GetAdvertiseAddr()...)
		buf = append(buf, ';')
	}
	return string(buf)
}

func diffMonitors(prev map[string]string, next map[string]resolvedMonitor) (upserts []*pb.ProbeMonitor, removed []string, nextHashes map[string]string) {
	nextHashes = make(map[string]string, len(next))
	for _, id := range slices.Sorted(maps.Keys(next)) {
		rm := next[id]
		nextHashes[id] = rm.hash
		if prev[id] != rm.hash {
			upserts = append(upserts, rm.proto)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(prev)) {
		if _, ok := next[id]; !ok {
			removed = append(removed, id)
		}
	}
	return upserts, removed, nextHashes
}

func hashProbeMonitor(m *pb.ProbeMonitor) string {
	h := sha256.New()
	writeLenStr(h, m.GetId())
	writeLenStr(h, m.GetTenantId())
	writeLenStr(h, m.GetType())
	var buf [13]byte
	binary.BigEndian.PutUint32(buf[0:4], m.GetIntervalSeconds())
	binary.BigEndian.PutUint32(buf[4:8], m.GetTimeoutSeconds())
	if m.GetSsrfProtection() {
		buf[8] = 1
	}
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(m.GetConfigJson()))) //nolint:gosec // G115: bounded by MaxRecvMsgSize (4 MiB)
	h.Write(buf[:])
	h.Write(m.GetConfigJson())
	return hex.EncodeToString(h.Sum(nil))
}

func writeLenStr(h interface{ Write([]byte) (int, error) }, s string) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s))) //nolint:gosec // G115: short identifiers
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(s))
}

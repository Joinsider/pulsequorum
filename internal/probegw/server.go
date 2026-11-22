// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package probegw implements the voter-side ProbeService gRPC gateway
// (ADR 0034, ADR 0035): probe verification, monitor assignment streaming,
// keepalive ticks, and validated heartbeat ingestion.
package probegw

import (
	"context"
	"crypto/x509"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	defaultTickInterval     = 5 * time.Second
	defaultDebounceInterval = 250 * time.Millisecond
	defaultBufferMaxAge     = 24 * time.Hour
	defaultPerProbeMinRate  = 10.0
	defaultPerProbeBurst    = 2000
	defaultGlobalRate       = 20000.0
	defaultGlobalBurst      = 20000
	fallbackPollInterval    = 10 * time.Second
)

// Cluster is the part of cluster.Node the probe gateway uses.
type Cluster interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
	Members() ([]cluster.Member, uint64, error)
	CommandLevel(ctx context.Context) (uint32, error)
	IsLeader() bool
}

// Options configure the voter-side probe gateway.
type Options struct {
	NodeID           string
	State            store.State
	Metrics          store.Metrics
	Cluster          Cluster
	SubscribeChanges func() (<-chan struct{}, func())
	Bundle           func(context.Context) ([]*x509.Certificate, string, error)
	Assigner         scheduler.Assigner
	Keyring          configsecret.Keyring
	SSRFProtection   bool
	OnStored         func([]store.Heartbeat)
	Publish          func([]store.Heartbeat)
	OnRejected       func(reason string)
	Clock            func() time.Time
	TickInterval     time.Duration
	DebounceInterval time.Duration
	BufferMaxAge     time.Duration
	PerProbeBurst    int
	GlobalRate       float64
	GlobalBurst      int
	Log              *slog.Logger

	// identity overrides clusternet.Peer for in-process unit tests without TLS.
	identity func(context.Context) (pki.Identity, bool)
}

// Server implements pb.ProbeServiceServer on the voter's cluster port.
type Server struct {
	pb.UnimplementedProbeServiceServer

	opts     Options
	sessions *sessionRegistry
	global   *tokenBucket
	log      *slog.Logger
}

var _ pb.ProbeServiceServer = (*Server)(nil)

// New creates a ProbeService server.
func New(opts Options) *Server {
	if opts.TickInterval <= 0 {
		opts.TickInterval = defaultTickInterval
	}
	if opts.DebounceInterval <= 0 {
		opts.DebounceInterval = defaultDebounceInterval
	}
	if opts.BufferMaxAge <= 0 {
		opts.BufferMaxAge = defaultBufferMaxAge
	}
	if opts.PerProbeBurst <= 0 {
		opts.PerProbeBurst = defaultPerProbeBurst
	}
	if opts.GlobalRate <= 0 {
		opts.GlobalRate = defaultGlobalRate
	}
	if opts.GlobalBurst <= 0 {
		opts.GlobalBurst = defaultGlobalBurst
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		opts:     opts,
		sessions: newSessionRegistry(),
		global:   newTokenBucket(opts.GlobalRate, float64(opts.GlobalBurst), opts.Clock),
		log:      log,
	}
}

// Sessions returns the active probe sessions connected to this voter for GetStatus.
func (s *Server) Sessions() []*pb.ProbeSession {
	return s.sessions.sessionsProto()
}

// SessionDetails returns active probe sessions including buffer status for the API.
func (s *Server) SessionDetails() map[string]SessionDetail {
	return s.sessions.details()
}

// Run watches FSM state changes to immediately terminate sessions of revoked
// probes or deleted tenants (ADR 0034 section 7, ADR 0035 D8).
func (s *Server) Run(ctx context.Context) {
	var ch <-chan struct{}
	if s.opts.SubscribeChanges != nil {
		var unsub func()
		ch, unsub = s.opts.SubscribeChanges()
		defer unsub()
	}
	ticker := time.NewTicker(fallbackPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-ticker.C:
		}
		s.evictRevokedSessions(ctx)
	}
}

func (s *Server) evictRevokedSessions(ctx context.Context) {
	ids := s.sessions.activeIDs()
	if len(ids) == 0 {
		return
	}
	revoked, err := s.opts.State.ListRevokedNodes(ctx)
	if err != nil {
		return
	}
	revokedSet := make(map[string]bool, len(revoked))
	for _, r := range revoked {
		revokedSet[r.NodeID] = true
	}
	level, err := s.effectiveLevel(ctx)
	if err != nil {
		return
	}
	var regMap map[string]store.Node
	if level >= cluster.ProbeCommandLevel {
		nodes, err := s.opts.State.ListNodes(ctx)
		if err != nil {
			return
		}
		regMap = make(map[string]store.Node, len(nodes))
		for _, n := range nodes {
			regMap[n.ID] = n
		}
	}
	for _, id := range ids {
		if revokedSet[id] {
			s.log.Warn("closing active session of revoked probe", "node_id", id)
			s.sessions.cancelNode(id)
			continue
		}
		if regMap != nil {
			n, ok := regMap[id]
			if !ok || n.Role != store.NodeRoleProbe {
				s.log.Warn("closing active session of unregistered probe", "node_id", id)
				s.sessions.cancelNode(id)
			}
		}
	}
}

func (s *Server) caller(ctx context.Context) (pki.Identity, bool) {
	if s.opts.identity != nil {
		return s.opts.identity(ctx)
	}
	return clusternet.Peer(ctx)
}

func (s *Server) now() time.Time {
	return s.opts.Clock()
}

func (s *Server) rejected(reason string) {
	if s.opts.OnRejected != nil {
		s.opts.OnRejected(reason)
	}
}

func (s *Server) effectiveLevel(ctx context.Context) (uint32, error) {
	if s.opts.Cluster != nil {
		stored, err := s.opts.Cluster.CommandLevel(ctx)
		if err != nil {
			return 0, err
		}
		return cluster.EffectiveCommandLevel(stored), nil
	}
	stored, err := s.opts.State.CommandLevel(ctx)
	if err != nil {
		return 0, err
	}
	return cluster.EffectiveCommandLevel(stored), nil
}

// verifyProbe checks that the caller holds a valid, non-revoked probe
// certificate whose identity matches the node registry (ADR 0035 section 1).
func (s *Server) verifyProbe(ctx context.Context) (pki.Identity, uint32, error) {
	id, ok := s.caller(ctx)
	if !ok {
		return pki.Identity{}, 0, status.Error(codes.Unauthenticated, "client certificate required")
	}
	if id.Role != pki.RoleProbe || id.NodeID == "" {
		s.rejected("non_probe_role")
		s.log.Warn("probe gateway rejected non-probe caller", "node_id", id.NodeID, "role", id.Role)
		return pki.Identity{}, 0, status.Error(codes.PermissionDenied, "probe certificate required")
	}
	revoked, err := s.opts.State.ListRevokedNodes(ctx)
	if err != nil {
		return pki.Identity{}, 0, status.Errorf(codes.Unavailable, "check revocations: %v", err)
	}
	for _, r := range revoked {
		if r.NodeID == id.NodeID {
			s.rejected("revoked")
			s.log.Warn("probe rejected: node is revoked", "node_id", id.NodeID, "tenant_id", id.TenantID)
			return pki.Identity{}, 0, status.Error(codes.PermissionDenied, "probe was removed from the cluster")
		}
	}
	level, err := s.effectiveLevel(ctx)
	if err != nil {
		return pki.Identity{}, 0, status.Errorf(codes.Unavailable, "read command level: %v", err)
	}
	if level >= cluster.ProbeCommandLevel {
		nodes, err := s.opts.State.ListNodes(ctx)
		if err != nil {
			return pki.Identity{}, 0, status.Errorf(codes.Unavailable, "read node registry: %v", err)
		}
		var reg *store.Node
		for i := range nodes {
			if nodes[i].ID == id.NodeID {
				reg = &nodes[i]
				break
			}
		}
		if reg == nil {
			s.rejected("unregistered")
			s.log.Warn("probe rejected: not in node registry", "node_id", id.NodeID, "tenant_id", id.TenantID)
			return pki.Identity{}, 0, status.Error(codes.PermissionDenied, "probe is not registered")
		}
		if reg.Role != store.NodeRoleProbe || reg.OwnerTenantID != id.TenantID {
			s.rejected("owner_mismatch")
			s.log.Warn("probe rejected: registry owner mismatch", "node_id", id.NodeID, "cert_tenant", id.TenantID, "reg_tenant", reg.OwnerTenantID, "reg_role", reg.Role)
			return pki.Identity{}, 0, status.Error(codes.PermissionDenied, "probe identity does not match cluster registry")
		}
	}
	return id, level, nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package membership

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusternet"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
	"github.com/joinsider/uptimecluster/internal/store"
)

// forwardedKey marks a Join forwarded by a follower, so it is forwarded at
// most once.
const forwardedKey = "uc-forwarded"

// Access levels of the ClusterService and ReplicationService methods
// (clusternet.Authorize). Apply accepts forwarded commands from voters only: a
// voter is a full member and could propose any command itself, probes never
// write (FA-CLU-05). Replication runs between voters only; probe heartbeats
// reach the cluster through the probe gateway (P5).
var Access = map[string]clusternet.Access{
	pb.ClusterService_Join_FullMethodName:                 clusternet.Anonymous,
	pb.ClusterService_RenewCertificate_FullMethodName:     clusternet.AnyNode,
	pb.ClusterService_Apply_FullMethodName:                clusternet.VoterOnly,
	pb.ClusterService_GetStatus_FullMethodName:            clusternet.VoterOnly,
	pb.ReplicationService_StreamHeartbeats_FullMethodName: clusternet.VoterOnly,
	pb.ReplicationService_GetWatermarks_FullMethodName:    clusternet.VoterOnly,
	pb.ReplicationService_FetchRange_FullMethodName:       clusternet.VoterOnly,
}

// Node is the part of cluster.Node the service uses.
type Node interface {
	Raft
	Leader() (address, id string)
	// ApplyLocal proposes a forwarded command on the leader without forwarding it again.
	ApplyLocal(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// KEK hands the generated master key to joining voters (ADR 0011 option B).
type KEK interface {
	Transferable() ([]byte, bool)
}

// Service implements ClusterService on the cluster port.
type Service struct {
	pb.UnimplementedClusterServiceServer

	Node      Node
	State     store.State
	Authority *clusterpki.Authority
	Manager   *Manager
	KEK       KEK
	// KEKFingerprint is sent to joining voters, so a voter with an external
	// key can check it uses the same one.
	KEKFingerprint string
	// DialLeader returns a client for the leader at address (mTLS as voter).
	DialLeader func(address string) (pb.ClusterServiceClient, func(), error)
	Log        *slog.Logger
}

var _ pb.ClusterServiceServer = (*Service)(nil)

// Join enrolls a node with a join token (FA-CLU-03, FA-PKI-02).
func (s *Service) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	info := req.GetNode()
	// A member with a valid certificate may move its own address without a
	// token (FA-CLU-10). Never forwarded: the leader must see the moving
	// node's own certificate, so it calls the leader directly.
	if peer, ok := clusternet.Peer(ctx); ok && req.GetTokenId() == "" {
		if !s.Node.IsLeader() {
			_, leaderID := s.Node.Leader()
			return nil, status.Errorf(codes.FailedPrecondition, "not the leader; leader=%s", leaderID)
		}
		return s.updateAddress(ctx, peer, info)
	}
	if !s.Node.IsLeader() {
		return s.forwardJoin(ctx, req)
	}
	if _, err := uuid.Parse(info.GetId()); err != nil {
		return nil, status.Error(codes.InvalidArgument, "node ID must be a UUID")
	}
	wantVoter := info.GetRole() != pb.NodeInfo_ROLE_PROBE
	if wantVoter {
		if host, port, err := net.SplitHostPort(info.GetAdvertiseAddr()); err != nil || host == "" || port == "" {
			return nil, status.Error(codes.InvalidArgument, "voter address must be host:port")
		}
		// Before the token is spent: a new node under a used address is
		// rejected with the old node ID (FA-CLU-08, ADR 0012 A5).
		var inUse *AddressInUseError
		if err := s.Manager.CheckAddress(info.GetId(), info.GetAdvertiseAddr()); errors.As(err, &inUse) {
			return nil, status.Error(codes.AlreadyExists, inUse.Error())
		} else if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}

	bundle, _, err := s.Authority.Bundle(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "cluster CA is not ready")
	}
	token := pki.JoinToken{ID: req.GetTokenId(), Secret: req.GetTokenSecret(), Fingerprint: pki.Fingerprint(bundle[0])}
	issued, err := s.Authority.Redeem(ctx, token.String(), info.GetId(), req.GetCsrDer())
	switch {
	case errors.Is(err, clusterpki.ErrInvalidToken):
		s.Log.Warn("join rejected: invalid token", "node_id", info.GetId(), "address", info.GetAdvertiseAddr())
		return nil, status.Error(codes.PermissionDenied, "invalid join token")
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	role := issued.Identity.Role
	if (role == pki.RoleVoter) != wantVoter {
		return nil, status.Error(codes.PermissionDenied, "the join token is for another role")
	}

	resp := &pb.JoinResponse{ClusterId: issued.Identity.ClusterID, CertificateChainDer: issued.ChainDER, CaBundleDer: issued.BundleDER}
	if role == pki.RoleVoter {
		if err := s.Manager.AddVoter(ctx, info.GetId(), info.GetAdvertiseAddr()); err != nil {
			var inUse *AddressInUseError
			if errors.As(err, &inUse) {
				return nil, status.Error(codes.AlreadyExists, inUse.Error())
			}
			return nil, status.Errorf(codes.Unavailable, "add node to the cluster: %v", err)
		}
		if key, ok := s.KEK.Transferable(); ok {
			resp.Kek = key
		}
		resp.KekFingerprint = s.KEKFingerprint
		members, _, err := s.Node.Members()
		if err == nil {
			for _, m := range members {
				if m.Voter {
					resp.Voters = append(resp.Voters, &pb.NodeInfo{Id: m.ID, AdvertiseAddr: m.Address, Role: pb.NodeInfo_ROLE_VOTER})
				}
			}
		}
	}
	s.Log.Info("node joined", "node_id", info.GetId(), "role", role, "address", info.GetAdvertiseAddr())
	return resp, nil
}

// updateAddress changes the address of the calling voter in the Raft
// configuration; voters keep their vote (Manager.AddVoter).
func (s *Service) updateAddress(ctx context.Context, peer pki.Identity, info *pb.NodeInfo) (*pb.JoinResponse, error) {
	if peer.Role != pki.RoleVoter || peer.NodeID != info.GetId() {
		return nil, status.Error(codes.PermissionDenied, "a node can only change its own address")
	}
	if err := s.stillMember(ctx, peer); err != nil {
		return nil, err
	}
	if host, port, err := net.SplitHostPort(info.GetAdvertiseAddr()); err != nil || host == "" || port == "" {
		return nil, status.Error(codes.InvalidArgument, "voter address must be host:port")
	}
	if err := s.Manager.AddVoter(ctx, peer.NodeID, info.GetAdvertiseAddr()); err != nil {
		var inUse *AddressInUseError
		if errors.As(err, &inUse) {
			return nil, status.Error(codes.AlreadyExists, inUse.Error())
		}
		return nil, status.Errorf(codes.Unavailable, "update address: %v", err)
	}
	return &pb.JoinResponse{ClusterId: peer.ClusterID}, nil
}

func (s *Service) forwardJoin(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get(forwardedKey)) > 0 {
		return nil, status.Error(codes.Unavailable, "no leader available, retry")
	}
	addr, _ := s.Node.Leader()
	if addr == "" {
		return nil, status.Error(codes.Unavailable, "no leader available, retry")
	}
	client, done, err := s.DialLeader(addr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "connect to leader: %v", err)
	}
	defer done()
	return client.Join(metadata.AppendToOutgoingContext(ctx, forwardedKey, "1"), req)
}

// RenewCertificate issues a new certificate with the caller's identity
// (FA-PKI-03). It runs on the leader only and re-checks membership and
// revocation against the state, because the TLS handshake used the cached
// revocation list.
func (s *Service) RenewCertificate(ctx context.Context, req *pb.RenewCertificateRequest) (*pb.RenewCertificateResponse, error) {
	peer, ok := clusternet.Peer(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	if !s.Node.IsLeader() {
		addr, _ := s.Node.Leader()
		return nil, status.Errorf(codes.FailedPrecondition, "not the leader; leader=%s", addr)
	}
	if err := s.stillMember(ctx, peer); err != nil {
		return nil, err
	}
	issued, err := s.Authority.Renew(ctx, peer, req.GetCsrDer())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.RenewCertificateResponse{CertificateChainDer: issued.ChainDER, CaBundleDer: issued.BundleDER}, nil
}

func (s *Service) stillMember(ctx context.Context, peer pki.Identity) error {
	revoked, err := s.State.ListRevokedNodes(ctx)
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	for _, r := range revoked {
		if r.NodeID == peer.NodeID {
			return status.Error(codes.PermissionDenied, "node was removed from the cluster")
		}
	}
	if peer.Role != pki.RoleVoter {
		return nil // probes are not Raft members (P5)
	}
	members, _, err := s.Node.Members()
	if err != nil {
		return status.Error(codes.Unavailable, err.Error())
	}
	for _, m := range members {
		if m.ID == peer.NodeID {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "node is not a member of the cluster")
}

// GetStatus reports this node's Raft state (ADR 0012 A3/A9).
func (s *Service) GetStatus(context.Context, *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	st := s.Node.Status()
	return &pb.GetStatusResponse{
		NodeId: st.NodeID, RaftState: st.RaftState, AppliedIndex: st.AppliedIndex, CommitIndex: st.CommitIndex,
		LastLogIndex: st.LastLogIndex, LastContactMs: st.LastContactMs, LeaderId: st.LeaderID,
	}, nil
}

// renewTimeout bounds one renewal call to the leader.
const renewTimeout = 15 * time.Second

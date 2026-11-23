// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
)

// TestPermanentJoinError covers ADR 0018: a binary too old for the cluster
// command level is a permanent join error, while a "not the leader" answer is
// retried.
func TestPermanentJoinError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "invalid token", err: status.Error(codes.PermissionDenied, "invalid join token"), want: true},
		{name: "address conflict", err: status.Error(codes.AlreadyExists, "address in use"), want: true},
		{name: "invalid request", err: status.Error(codes.InvalidArgument, "node ID must be a UUID"), want: true},
		{
			name: "binary too old for the command level",
			err: status.Error(codes.FailedPrecondition,
				"this binary supports command version 2, the cluster requires 3: upgrade the binary before joining (ADR 0018)"),
			want: true,
		},
		{name: "not the leader", err: status.Error(codes.FailedPrecondition, "not the leader; leader=10.0.0.1:8443"), want: false},
		{name: "unavailable", err: status.Error(codes.Unavailable, "no leader available, retry"), want: false},
		{name: "transport error", err: errors.New("connection refused"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentJoinError(tc.err); got != tc.want {
				t.Errorf("permanentJoinError = %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeJoinServer struct {
	pb.UnimplementedClusterServiceServer
	ca     *pki.CA
	calls  atomic.Int32
	onJoin func(call int32, req *pb.JoinRequest) (*pb.JoinResponse, error)
}

func (s *fakeJoinServer) Join(_ context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	n := s.calls.Add(1)
	if s.onJoin != nil {
		return s.onJoin(n, req)
	}
	role := pki.RoleVoter
	if req.GetNode().GetRole() == pb.NodeInfo_ROLE_PROBE {
		role = pki.RoleProbe
	}
	leaf, err := s.ca.Issue(req.GetCsrDer(), pki.Identity{
		ClusterID: "c1",
		NodeID:    req.GetNode().GetId(),
		Role:      role,
	}, time.Now())
	if err != nil {
		return nil, err
	}
	return &pb.JoinResponse{
		ClusterId:           "c1",
		CertificateChainDer: [][]byte{leaf.Raw, s.ca.Cert.Raw},
		CaBundleDer:         [][]byte{s.ca.Cert.Raw},
		Kek:                 []byte("kek"),
		KekFingerprint:      "fp-1",
		Voters:              []*pb.NodeInfo{{AdvertiseAddr: "10.0.0.1:9443"}},
	}, nil
}

func startFakeJoinServer(t *testing.T, onJoin func(call int32, req *pb.JoinRequest) (*pb.JoinResponse, error)) (string, string, *pki.CA) {
	t.Helper()
	ca, err := pki.NewCA("c1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srvKey, _ := pki.NewKey()
	srvCSR, _ := pki.NewCSR(srvKey)
	srvLeaf, err := ca.Issue(srvCSR, pki.Identity{ClusterID: "c1", NodeID: uuid.NewString(), Role: pki.RoleVoter}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*pki.Leaf(srvLeaf, srvKey, ca.Cert)},
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	pb.RegisterClusterServiceServer(srv, &fakeJoinServer{ca: ca, onJoin: onJoin})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	tok, err := pki.NewJoinToken(pki.Fingerprint(ca.Cert))
	if err != nil {
		t.Fatal(err)
	}
	return lis.Addr().String(), tok.String(), ca
}

func TestJoinFlowAndErrors(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	// Invalid token format fails immediately with ErrJoinRejected.
	_, err := Join(ctx, JoinOptions{Token: "bad-token", Log: log})
	if !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("expected ErrJoinRejected for bad token, got %v", err)
	}

	// Permanent server error stops without retry.
	addrPerm, tokPerm, _ := startFakeJoinServer(t, func(_ int32, _ *pb.JoinRequest) (*pb.JoinResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "invalid join token")
	})
	nodeID := uuid.NewString()
	_, err = Join(ctx, JoinOptions{
		Seeds:  []string{addrPerm},
		Token:  tokPerm,
		NodeID: nodeID,
		Cert:   &clusterpki.NodeCert{DataDir: t.TempDir(), NodeID: nodeID},
		Log:    log,
	})
	if !errors.Is(err, ErrJoinRejected) {
		t.Fatalf("expected ErrJoinRejected from server, got %v", err)
	}

	// Successful join with the probe role.
	addrRetry, tokRetry, _ := startFakeJoinServer(t, nil)
	// Also test transient failure -> context cancellation.
	addrUnavail, tokUnavail, _ := startFakeJoinServer(t, func(_ int32, _ *pb.JoinRequest) (*pb.JoinResponse, error) {
		return nil, status.Error(codes.Unavailable, "no leader")
	})
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = Join(shortCtx, JoinOptions{
		Seeds:  []string{addrUnavail},
		Token:  tokUnavail,
		NodeID: nodeID,
		Cert:   &clusterpki.NodeCert{DataDir: t.TempDir(), NodeID: nodeID},
		Log:    log,
	})
	if err == nil {
		t.Fatal("expected context deadline error on unavailable seed")
	}

	// Successful probe join.
	probeID := uuid.NewString()
	probeCert := &clusterpki.NodeCert{DataDir: t.TempDir(), NodeID: probeID}
	joined, err := Join(ctx, JoinOptions{
		Seeds:  []string{addrRetry},
		Token:  tokRetry,
		NodeID: probeID,
		Role:   pki.RoleProbe,
		Cert:   probeCert,
		Log:    log,
	})
	if err != nil {
		t.Fatalf("Join probe: %v", err)
	}
	if joined.ClusterID != "c1" || len(joined.Voters) != 1 || joined.Voters[0] != "10.0.0.1:9443" {
		t.Fatalf("Joined = %+v", joined)
	}

	// install validation errors: incomplete chain, wrong CA, wrong role.
	key, _, err := probeCert.NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	if err := install(JoinOptions{NodeID: probeID, Cert: probeCert}, pki.RoleProbe, key, &pb.JoinResponse{}, nil); err == nil {
		t.Fatal("expected error for incomplete certificate chain")
	}
	otherCA, _ := pki.NewCA("c1", time.Now())
	if err := install(JoinOptions{NodeID: probeID, Cert: probeCert}, pki.RoleProbe, key, &pb.JoinResponse{
		CertificateChainDer: [][]byte{otherCA.Cert.Raw, otherCA.Cert.Raw},
	}, probeCert.Leaf()); err == nil {
		t.Fatal("expected error for mismatched CA fingerprint")
	}
}

func TestRenewViaLeader(t *testing.T) {
	ctx := context.Background()

	// No leader known.
	fnNoLeader := RenewViaLeader(func() string { return "" }, nil)
	if _, err := fnNoLeader(ctx, []byte{1}); err == nil {
		t.Fatal("expected error when no leader is known")
	}

	// Dial failure.
	fnDialErr := RenewViaLeader(
		func() string { return "10.0.0.1:9443" },
		func(string) (pb.ClusterServiceClient, func(), error) {
			return nil, nil, errors.New("dial failed")
		},
	)
	if _, err := fnDialErr(ctx, []byte{1}); err == nil {
		t.Fatal("expected error on dial failure")
	}

	// Success.
	fc := &fakeRenewClient{}
	fnOK := RenewViaLeader(
		func() string { return "10.0.0.1:9443" },
		func(string) (pb.ClusterServiceClient, func(), error) {
			return fc, func() {}, nil
		},
	)
	issued, err := fnOK(ctx, []byte("csr"))
	if err != nil || len(issued.ChainDER) != 2 {
		t.Fatalf("RenewViaLeader = (%+v, %v)", issued, err)
	}
}

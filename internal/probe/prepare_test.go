// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
)

type stubJoinClusterServer struct {
	pb.UnimplementedClusterServiceServer
	ca     *pki.CA
	voters []string
}

func (s *stubJoinClusterServer) Join(_ context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	now := time.Now()
	leaf, err := s.ca.Issue(req.GetCsrDer(), pki.Identity{
		ClusterID: "cluster-1",
		NodeID:    req.GetNode().GetId(),
		Role:      pki.RoleProbe,
		TenantID:  "default",
	}, now)
	if err != nil {
		return nil, err
	}
	var voters []*pb.NodeInfo
	for _, addr := range s.voters {
		voters = append(voters, &pb.NodeInfo{AdvertiseAddr: addr, Role: pb.NodeInfo_ROLE_VOTER})
	}
	return &pb.JoinResponse{
		ClusterId:           "cluster-1",
		CertificateChainDer: [][]byte{leaf.Raw, s.ca.Cert.Raw},
		CaBundleDer:         [][]byte{s.ca.Cert.Raw},
		Voters:              voters,
	}, nil
}

func startStubJoinServer(t *testing.T, voters []string) (string, string, *pki.CA) {
	t.Helper()
	now := time.Now()
	ca, err := pki.NewCA("cluster-1", now)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	srvCSR, err := pki.NewCSR(srvKey)
	if err != nil {
		t.Fatal(err)
	}
	srvLeaf, err := ca.Issue(srvCSR, pki.Identity{
		ClusterID: "cluster-1",
		NodeID:    uuid.NewString(),
		Role:      pki.RoleVoter,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	tlsCert := pki.Leaf(srvLeaf, srvKey, ca.Cert)
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*tlsCert},
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	pb.RegisterClusterServiceServer(srv, &stubJoinClusterServer{ca: ca, voters: voters})
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

func TestPrepareErrorsAndExistingCert(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)

	// Missing token when no certificate exists.
	_, _, err := Prepare(ctx, PrepareOptions{
		DataDir: t.TempDir(),
		NodeID:  uuid.NewString(),
		Seeds:   []string{"127.0.0.1:9443"},
		Log:     log,
	})
	if err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("expected missing token error, got %v", err)
	}

	// Missing seeds when no certificate exists.
	_, _, err = Prepare(ctx, PrepareOptions{
		DataDir: t.TempDir(),
		NodeID:  uuid.NewString(),
		Token:   uuid.NewString(),
		Log:     log,
	})
	if err == nil || !strings.Contains(err.Error(), "no cluster seeds") {
		t.Fatalf("expected missing seeds error, got %v", err)
	}

	// Join succeeds and persists certificate and learned voters; subsequent Prepare reuses existing cert.
	addr, token, _ := startStubJoinServer(t, []string{"127.0.0.1:19443"})
	dir := t.TempDir()
	nodeID := uuid.NewString()
	cert, voters, err := Prepare(ctx, PrepareOptions{
		DataDir:     dir,
		NodeID:      nodeID,
		Name:        "probe-1",
		Seeds:       []string{addr},
		Token:       token,
		Region:      "eu",
		Version:     "0.1.0",
		Incarnation: "inc-1",
	})
	if err != nil {
		t.Fatalf("Prepare join: %v", err)
	}
	if cert.Leaf() == nil {
		t.Fatal("expected installed certificate leaf")
	}
	if !slices.Contains(voters.Candidates(), "127.0.0.1:19443") {
		t.Fatalf("candidates = %v, want learned voter", voters.Candidates())
	}

	// Second Prepare with existing certificate ignores Token and does not dial.
	cert2, _, err := Prepare(ctx, PrepareOptions{
		DataDir: dir,
		NodeID:  nodeID,
		Seeds:   []string{addr},
		Token:   "ignored-token",
		Log:     log,
	})
	if err != nil {
		t.Fatalf("Prepare existing cert: %v", err)
	}
	if cert2.Leaf() == nil {
		t.Fatal("expected existing certificate leaf")
	}
	_ = clusterpki.NodeCert{}
}

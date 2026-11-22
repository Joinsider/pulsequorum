// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package membership

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/secrets"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

type probeJoinEnv struct {
	ctx   context.Context
	node  *cluster.Node
	state *sqlite.StateStore
	auth  *clusterpki.Authority
	svc   *Service
	mgr   *Manager
}

func newProbeJoinEnv(t *testing.T) *probeJoinEnv {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{
		NodeID: uuid.NewString(), Advertise: "127.0.0.1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error",
	}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cluster.EnsureSecretKey(ctx, node, state, kek); err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, state.ListSecretKeys)
	if err := kr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	auth := &clusterpki.Authority{State: state, Cluster: node, Keyring: kr}
	if err := auth.Ensure(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Apply(ctx, cluster.NewSetFeatureLevel("lvl10", cluster.ProbeCommandLevel)); err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{Raft: node, State: state, Log: log}
	svc := &Service{
		Node:           node,
		State:          state,
		Authority:      auth,
		Manager:        mgr,
		KEK:            kek,
		KEKFingerprint: kek.Fingerprint,
		Log:            log,
	}
	return &probeJoinEnv{ctx: ctx, node: node, state: state, auth: auth, svc: svc, mgr: mgr}
}

func peerCtxWithCert(ctx context.Context, leaf *x509.Certificate) context.Context {
	return peer.NewContext(ctx, &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}},
		},
	})
}

func TestProbeJoinRenewAndRemove(t *testing.T) {
	e := newProbeJoinEnv(t)

	// Issue a probe join token bound to default tenant.
	rawTok, _, err := e.auth.CreateJoinTokenFor(e.ctx, pki.RoleProbe, cluster.DefaultTenantID, time.Hour, &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := pki.ParseJoinToken(rawTok)
	if err != nil {
		t.Fatal(err)
	}

	probeID := uuid.NewString()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := e.svc.Join(e.ctx, &pb.JoinRequest{
		TokenId:     tok.ID,
		TokenSecret: tok.Secret,
		CsrDer:      csr,
		Node: &pb.NodeInfo{
			Id:           probeID,
			Name:         "edge-probe-1",
			Role:         pb.NodeInfo_ROLE_PROBE,
			Region:       "eu-central",
			Labels:       map[string]string{"site": "home"},
			Capabilities: []string{"icmp"},
			Version:      "0.1.0",
			Incarnation:  "inc-probe-1",
		},
	})
	if err != nil {
		t.Fatalf("probe Join: %v", err)
	}
	// Probes never receive KEK, do receive voters, and never enter Raft config.
	if len(resp.GetKek()) != 0 || resp.GetKekFingerprint() != "" {
		t.Fatalf("probe Join returned KEK (len=%d, fp=%q)", len(resp.GetKek()), resp.GetKekFingerprint())
	}
	if len(resp.GetVoters()) != 1 {
		t.Fatalf("probe Join voters = %d, want 1", len(resp.GetVoters()))
	}
	members, _, err := e.node.Members()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].ID == probeID {
		t.Fatalf("probe leaked into Raft configuration: %+v", members)
	}

	// Probe must be registered in the node registry with owner from the token.
	nodes, err := e.state.ListNodes(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reg *store.Node
	for i := range nodes {
		if nodes[i].ID == probeID {
			reg = &nodes[i]
		}
	}
	if reg == nil || reg.Role != store.NodeRoleProbe || reg.OwnerTenantID != cluster.DefaultTenantID || reg.Incarnation != "inc-probe-1" {
		t.Fatalf("registered probe = %+v", reg)
	}

	leaf, err := x509.ParseCertificate(resp.GetCertificateChainDer()[0])
	if err != nil {
		t.Fatal(err)
	}

	// RenewCertificate directly on the leader succeeds for registered probe.
	newKey, _ := pki.NewKey()
	newCSR, _ := pki.NewCSR(newKey)
	renewResp, err := e.svc.RenewCertificate(peerCtxWithCert(e.ctx, leaf), &pb.RenewCertificateRequest{CsrDer: newCSR})
	if err != nil {
		t.Fatalf("RenewCertificate on leader: %v", err)
	}
	if len(renewResp.GetCertificateChainDer()) < 2 {
		t.Fatalf("expected full chain on renew")
	}

	// Forwarded renewal rejected unless caller has a voter certificate.
	_, err = e.svc.RenewCertificate(peerCtxWithCert(e.ctx, leaf), &pb.RenewCertificateRequest{
		CsrDer:        newCSR,
		ForwardNodeId: probeID,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("probe forwarding renewal code = %v, want PermissionDenied", status.Code(err))
	}

	// Forwarded renewal from a voter certificate succeeds on the leader.
	voterKey, _ := pki.NewKey()
	voterCSR, _ := pki.NewCSR(voterKey)
	voterIssued, err := e.auth.IssueSelf(e.ctx, e.node.ID(), voterCSR)
	if err != nil {
		t.Fatal(err)
	}
	voterLeaf, _ := x509.ParseCertificate(voterIssued.ChainDER[0])
	if _, err := e.svc.RenewCertificate(peerCtxWithCert(e.ctx, voterLeaf), &pb.RenewCertificateRequest{
		CsrDer:          newCSR,
		ForwardNodeId:   probeID,
		ForwardTenantId: cluster.DefaultTenantID,
	}); err != nil {
		t.Fatalf("voter forwarding probe renewal: %v", err)
	}

	// Wrong tenant in forwarded renewal is rejected.
	_, err = e.svc.RenewCertificate(peerCtxWithCert(e.ctx, voterLeaf), &pb.RenewCertificateRequest{
		CsrDer:          newCSR,
		ForwardNodeId:   probeID,
		ForwardTenantId: "other-tenant",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong-tenant renewal code = %v, want PermissionDenied", status.Code(err))
	}

	// Removing the probe revokes it without altering Raft voter count.
	rem, err := e.mgr.Remove(e.ctx, probeID, "admin")
	if err != nil {
		t.Fatalf("Remove probe: %v", err)
	}
	if rem.VotersLeft != 1 {
		t.Fatalf("VotersLeft = %d, want 1", rem.VotersLeft)
	}
	if _, err := e.svc.RenewCertificate(peerCtxWithCert(e.ctx, leaf), &pb.RenewCertificateRequest{CsrDer: newCSR}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked probe renew code = %v, want PermissionDenied", status.Code(err))
	}
}

type fakeRenewFollowerNode struct {
	Node
	leaderAddr string
}

func (f fakeRenewFollowerNode) IsLeader() bool               { return false }
func (f fakeRenewFollowerNode) Leader() (address, id string) { return f.leaderAddr, "leader-1" }

type fakeRenewClient struct {
	pb.ClusterServiceClient
	got *pb.RenewCertificateRequest
}

func (f *fakeRenewClient) RenewCertificate(_ context.Context, req *pb.RenewCertificateRequest, _ ...grpc.CallOption) (*pb.RenewCertificateResponse, error) {
	f.got = req
	return &pb.RenewCertificateResponse{CertificateChainDer: [][]byte{{1}, {2}}}, nil
}

func TestProbeRenewForwardedByFollower(t *testing.T) {
	ca, err := pki.NewCA("c1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := pki.NewKey()
	csr, _ := pki.NewCSR(key)
	probeID := uuid.NewString()
	leaf, err := ca.Issue(csr, pki.Identity{ClusterID: "c1", NodeID: probeID, Role: pki.RoleProbe, TenantID: "t1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeRenewClient{}
	svc := &Service{
		Node: fakeRenewFollowerNode{leaderAddr: "10.0.0.1:9443"},
		DialLeader: func(addr string) (pb.ClusterServiceClient, func(), error) {
			if addr != "10.0.0.1:9443" {
				return nil, nil, errors.New("unexpected leader addr")
			}
			return fc, func() {}, nil
		},
	}
	resp, err := svc.RenewCertificate(peerCtxWithCert(context.Background(), leaf), &pb.RenewCertificateRequest{CsrDer: csr})
	if err != nil {
		t.Fatalf("follower RenewCertificate: %v", err)
	}
	if len(resp.GetCertificateChainDer()) != 2 {
		t.Fatalf("expected forwarded chain, got %d", len(resp.GetCertificateChainDer()))
	}
	if fc.got.GetForwardNodeId() != probeID || fc.got.GetForwardTenantId() != "t1" {
		t.Fatalf("forwarded identity = (%q, %q), want (%q, %q)", fc.got.GetForwardNodeId(), fc.got.GetForwardTenantId(), probeID, "t1")
	}

	// Voter callers on a follower still receive FailedPrecondition so they dial the leader themselves.
	voterLeaf, err := ca.Issue(csr, pki.Identity{ClusterID: "c1", NodeID: uuid.NewString(), Role: pki.RoleVoter}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.RenewCertificate(peerCtxWithCert(context.Background(), voterLeaf), &pb.RenewCertificateRequest{CsrDer: csr})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("voter on follower code = %v, want FailedPrecondition", status.Code(err))
	}
}

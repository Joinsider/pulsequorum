// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusternet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/pki"
)

type fixture struct {
	ca *pki.CA
	v  *pki.Verifier
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ca, err := pki.NewCA("c1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{ca: ca, v: &pki.Verifier{ClusterID: "c1", Roots: func() []*x509.Certificate { return []*x509.Certificate{ca.Cert} }}}
}

func (f *fixture) holder(t *testing.T, id string, role pki.Role) *pki.Holder {
	t.Helper()
	key, _ := pki.NewKey()
	csr, _ := pki.NewCSR(key)
	cert, err := f.ca.Issue(csr, pki.Identity{ClusterID: "c1", NodeID: id, Role: role}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := &pki.Holder{}
	h.Set(pki.Leaf(cert, key, f.ca.Cert))
	return h
}

func (f *fixture) listen(t *testing.T, h *pki.Holder) *Mux {
	t.Helper()
	m, err := Listen("127.0.0.1:0", "", Security{Holder: h, Verifier: f.v}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m.advertise = m.Addr().String()
	go func() { _ = m.Serve() }()
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestRaftOnlyBetweenVoters(t *testing.T) {
	f := newFixture(t)
	a := f.listen(t, f.holder(t, "a", pki.RoleVoter))
	b := f.listen(t, f.holder(t, "b", pki.RoleVoter))

	got := make(chan string, 1)
	go func() {
		c, err := b.RaftLayer().Accept()
		if err != nil {
			got <- err.Error()
			return
		}
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		got <- string(buf)
		_ = c.Close()
	}()
	c, err := a.RaftLayer().Dial(raft.ServerAddress(b.Addr().String()), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping"))
	if msg := <-got; msg != "ping" {
		t.Fatalf("received %q", msg)
	}
	_ = c.Close()
	if a.RaftLayer().Addr().String() != a.Addr().String() {
		t.Error("raft layer does not advertise its address")
	}

	// A probe or a client without certificate never reaches Raft.
	probe := f.holder(t, "p", pki.RoleProbe)
	for name, cfg := range map[string]*tls.Config{
		"probe":          pki.ClientConfig(probe, f.v, "", nil, ALPNRaft),
		"no certificate": noClientCert(f, ALPNRaft),
	} {
		conn, err := tls.Dial("tcp", b.Addr().String(), cfg)
		if err != nil {
			continue // rejected in the handshake
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil {
			t.Errorf("%s: raft connection stayed open", name)
		}
		_ = conn.Close()
	}
}

func noClientCert(f *fixture, alpn string) *tls.Config {
	var ca atomic.Pointer[x509.Certificate]
	cfg := pki.JoinClientConfig(pki.Fingerprint(f.ca.Cert), &ca)
	cfg.NextProtos = []string{alpn}
	return cfg
}

// stubCluster answers every RPC with success so that only the interceptor
// decides.
type stubCluster struct {
	pb.UnimplementedClusterServiceServer
}

// stubReplication answers a stream with one empty response.
type stubReplication struct {
	pb.UnimplementedReplicationServiceServer
}

func (stubReplication) StreamHeartbeats(s grpc.BidiStreamingServer[pb.StreamHeartbeatsRequest, pb.StreamHeartbeatsResponse]) error {
	return s.Send(&pb.StreamHeartbeatsResponse{})
}

func (stubCluster) Join(context.Context, *pb.JoinRequest) (*pb.JoinResponse, error) {
	return &pb.JoinResponse{ClusterId: "c1"}, nil
}

func (stubCluster) RenewCertificate(context.Context, *pb.RenewCertificateRequest) (*pb.RenewCertificateResponse, error) {
	return &pb.RenewCertificateResponse{}, nil
}

func (stubCluster) GetStatus(context.Context, *pb.GetStatusRequest) (*pb.GetStatusResponse, error) {
	return &pb.GetStatusResponse{NodeId: "srv"}, nil
}

func TestGRPCAuthorization(t *testing.T) {
	f := newFixture(t)
	m := f.listen(t, f.holder(t, "srv", pki.RoleVoter))
	srv := grpc.NewServer(ServerOptions(map[string]Access{
		pb.ClusterService_Join_FullMethodName:                 Anonymous,
		pb.ClusterService_RenewCertificate_FullMethodName:     AnyNode,
		pb.ClusterService_GetStatus_FullMethodName:            VoterOnly,
		pb.ReplicationService_StreamHeartbeats_FullMethodName: VoterOnly,
	})...)
	pb.RegisterClusterServiceServer(srv, stubCluster{})
	pb.RegisterReplicationServiceServer(srv, stubReplication{})
	go func() { _ = srv.Serve(m.GRPCListener()) }()
	t.Cleanup(srv.Stop)

	client := func(cfg *tls.Config) pb.ClusterServiceClient {
		conn, err := grpc.NewClient(m.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pb.NewClusterServiceClient(conn)
	}
	replication := func(cfg *tls.Config) pb.ReplicationServiceClient {
		conn, err := grpc.NewClient(m.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pb.NewReplicationServiceClient(conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	anon := client(noClientCert(f, ALPNGRPC))
	probe := client(pki.ClientConfig(f.holder(t, "p", pki.RoleProbe), f.v, "", nil, ALPNGRPC))
	voter := client(pki.ClientConfig(f.holder(t, "v", pki.RoleVoter), f.v, "", pki.OnlyVoters, ALPNGRPC))

	if _, err := anon.Join(ctx, &pb.JoinRequest{}); err != nil {
		t.Errorf("anonymous Join: %v", err)
	}
	expect := func(name string, err error, want codes.Code) {
		t.Helper()
		if status.Code(err) != want {
			t.Errorf("%s: code = %v (%v), want %v", name, status.Code(err), err, want)
		}
	}
	_, err := anon.RenewCertificate(ctx, &pb.RenewCertificateRequest{})
	expect("anonymous Renew", err, codes.Unauthenticated)
	_, err = anon.GetStatus(ctx, &pb.GetStatusRequest{})
	expect("anonymous GetStatus", err, codes.Unauthenticated)
	_, err = probe.RenewCertificate(ctx, &pb.RenewCertificateRequest{})
	expect("probe Renew", err, codes.OK)
	_, err = probe.GetStatus(ctx, &pb.GetStatusRequest{})
	expect("probe GetStatus", err, codes.PermissionDenied)
	_, err = voter.GetStatus(ctx, &pb.GetStatusRequest{})
	expect("voter GetStatus", err, codes.OK)
	_, err = voter.Apply(ctx, &pb.ApplyRequest{})
	expect("unlisted Apply", err, codes.PermissionDenied)

	// Streams are checked when they open, before the handler runs.
	stream := func(c pb.ReplicationServiceClient) error {
		s, err := c.StreamHeartbeats(ctx)
		if err != nil {
			return err
		}
		_, err = s.Recv()
		return err
	}
	expect("anonymous stream", stream(replication(noClientCert(f, ALPNGRPC))), codes.Unauthenticated)
	probeCfg := pki.ClientConfig(f.holder(t, "p2", pki.RoleProbe), f.v, "", nil, ALPNGRPC)
	voterCfg := pki.ClientConfig(f.holder(t, "v3", pki.RoleVoter), f.v, "", pki.OnlyVoters, ALPNGRPC)
	expect("probe stream", stream(replication(probeCfg)), codes.PermissionDenied)
	expect("voter stream", stream(replication(voterCfg)), codes.OK)
	_, err = replication(voterCfg).GetWatermarks(ctx, &pb.GetWatermarksRequest{})
	expect("unlisted GetWatermarks", err, codes.PermissionDenied)

	// The voter dial helper refuses a server that is not a voter.
	pm := f.listen(t, f.holder(t, "probe-srv", pki.RoleProbe))
	conn, err := DialVoter(pm.Addr().String(), Security{Holder: f.holder(t, "v2", pki.RoleVoter), Verifier: f.v})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, err = pb.NewClusterServiceClient(conn).GetStatus(ctx, &pb.GetStatusRequest{})
	expect("voter client to probe server", err, codes.Unavailable)
}

var _ net.Listener = (*chanListener)(nil)

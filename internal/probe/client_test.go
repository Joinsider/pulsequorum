// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/joinsider/pulsequorum/internal/clusterpki"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

const testBufSize = 1024 * 1024

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf.Bytes())
}

type fakeMonitorSync struct {
	mu    sync.Mutex
	calls [][]store.Monitor
	ch    chan []store.Monitor
}

func newFakeMonitorSync() *fakeMonitorSync {
	return &fakeMonitorSync{
		ch: make(chan []store.Monitor, 16),
	}
}

func (s *fakeMonitorSync) Sync(monitors []store.Monitor) {
	cp := slices.Clone(monitors)
	s.mu.Lock()
	s.calls = append(s.calls, cp)
	s.mu.Unlock()
	select {
	case s.ch <- cp:
	default:
	}
}

func (s *fakeMonitorSync) waitCall(t *testing.T, timeout time.Duration) []store.Monitor {
	t.Helper()
	select {
	case got := <-s.ch:
		return got
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for MonitorSync.Sync", timeout)
		return nil
	}
}

type fakeProbeVoter struct {
	pb.UnimplementedProbeServiceServer

	lis *bufconn.Listener
	srv *grpc.Server

	onSubscribe func(req *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error
	onUpload    func(req *pb.UploadHeartbeatsRequest) uint64

	mu             sync.Mutex
	subscribeCalls int
	uploaded       []*pb.Heartbeat
}

func newFakeProbeVoter(t *testing.T) *fakeProbeVoter {
	t.Helper()
	v := &fakeProbeVoter{
		lis: bufconn.Listen(testBufSize),
		srv: grpc.NewServer(),
	}
	pb.RegisterProbeServiceServer(v.srv, v)
	go func() {
		_ = v.srv.Serve(v.lis)
	}()
	t.Cleanup(func() {
		v.srv.Stop()
		_ = v.lis.Close()
	})
	return v
}

func (v *fakeProbeVoter) Subscribe(req *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
	v.mu.Lock()
	v.subscribeCalls++
	v.mu.Unlock()
	if v.onSubscribe != nil {
		return v.onSubscribe(req, stream)
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (v *fakeProbeVoter) UploadHeartbeats(stream grpc.BidiStreamingServer[pb.UploadHeartbeatsRequest, pb.UploadHeartbeatsResponse]) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var ackSeq uint64
		if v.onUpload != nil {
			ackSeq = v.onUpload(req)
		} else {
			hbs := req.GetHeartbeats()
			if len(hbs) > 0 {
				v.mu.Lock()
				v.uploaded = append(v.uploaded, hbs...)
				v.mu.Unlock()
				ackSeq = hbs[len(hbs)-1].GetSeq()
			}
		}
		if ackSeq > 0 {
			if err := stream.Send(&pb.UploadHeartbeatsResponse{
				Persisted: &pb.StreamPosition{Seq: ackSeq},
			}); err != nil {
				return err
			}
		}
	}
}

func (v *fakeProbeVoter) SubscribeCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.subscribeCalls
}

func newTestNodeCert(t *testing.T, nodeID, tenantID string) *clusterpki.NodeCert {
	t.Helper()
	now := time.Now()
	ca, err := pki.NewCA("cluster-test", now)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	nc := &clusterpki.NodeCert{
		DataDir: t.TempDir(),
		NodeID:  nodeID,
	}
	key, csr, err := nc.NewCSR()
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	leaf, err := ca.Issue(csr, pki.Identity{
		ClusterID: "cluster-test",
		NodeID:    nodeID,
		Role:      pki.RoleProbe,
		TenantID:  tenantID,
	}, now)
	if err != nil {
		t.Fatalf("ca.Issue: %v", err)
	}
	if err := nc.Install(key, clusterpki.Issued{
		ChainDER:  [][]byte{leaf.Raw, ca.Cert.Raw},
		BundleDER: [][]byte{ca.Cert.Raw},
	}); err != nil {
		t.Fatalf("nc.Install: %v", err)
	}
	return nc
}

func newTestVoterList(t *testing.T, addrs ...string) *VoterList {
	t.Helper()
	// Preserve input order deterministically during Fisher-Yates shuffle.
	vl, err := NewVoterList(t.TempDir(), addrs, func(n int) int { return n - 1 })
	if err != nil {
		t.Fatalf("NewVoterList: %v", err)
	}
	return vl
}

func newBufconnDialer(voters map[string]*fakeProbeVoter) func(context.Context, string) (*grpc.ClientConn, error) {
	return func(_ context.Context, addr string) (*grpc.ClientConn, error) {
		v, ok := voters[addr]
		if !ok || v == nil {
			return nil, errors.New("connection refused")
		}
		return grpc.NewClient(
			"passthrough:///"+addr,
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return v.lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
	}
}

func newTestBuffer(t *testing.T, nodeID string) (*sqlite.MetricsStore, *Buffer) {
	t.Helper()
	ms := openTestMetricsStore(t)
	buf, err := NewBuffer(context.Background(), BufferOptions{
		Store:  ms,
		NodeID: nodeID,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}
	return ms, buf
}

func TestPolicySSRF(t *testing.T) {
	privateIP := netip.MustParseAddr("10.0.0.1")
	publicIP := netip.MustParseAddr("93.184.216.34")

	t.Run("private probe localSSRF=false ignores SetRemote(true)", func(t *testing.T) {
		p := NewPolicy(false, true)
		p.SetRemote(true)
		if err := p.Check(privateIP); err != nil {
			t.Fatalf("expected private probe with localSSRF=false to allow %s, got %v", privateIP, err)
		}
		p.SetRemote(false)
		if err := p.Check(privateIP); err != nil {
			t.Fatalf("expected private probe with localSSRF=false to allow %s, got %v", privateIP, err)
		}
	})

	t.Run("private probe localSSRF=true blocks private IP", func(t *testing.T) {
		p := NewPolicy(true, true)
		p.SetRemote(false)
		if err := p.Check(privateIP); err == nil {
			t.Fatalf("expected private probe with localSSRF=true to block %s", privateIP)
		}
		if err := p.Check(publicIP); err != nil {
			t.Fatalf("expected public IP %s to be allowed, got %v", publicIP, err)
		}
	})

	t.Run("public probe enforces union of local and remote", func(t *testing.T) {
		p := NewPolicy(false, false)
		p.SetRemote(true)
		if err := p.Check(privateIP); err == nil {
			t.Fatalf("expected public probe with SetRemote(true) to block %s", privateIP)
		}
		if err := p.Check(publicIP); err != nil {
			t.Fatalf("expected public IP %s to be allowed, got %v", publicIP, err)
		}

		p.SetRemote(false)
		if err := p.Check(privateIP); err != nil {
			t.Fatalf("expected public probe with localSSRF=false and SetRemote(false) to allow %s, got %v", privateIP, err)
		}

		pLocal := NewPolicy(true, false)
		pLocal.SetRemote(false)
		if err := pLocal.Check(privateIP); err == nil {
			t.Fatalf("expected public probe with localSSRF=true and SetRemote(false) to block %s", privateIP)
		}
	})
}

func TestClientTickTimeoutFailover(t *testing.T) {
	const (
		addrA = "voter-a:443"
		addrB = "voter-b:443"
	)

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     1,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "mon-from-a", TenantId: "t1", Type: "http", ConfigJson: []byte(`{"url":"https://a.example"}`), IntervalSeconds: 10, TimeoutSeconds: 5},
			},
		}); err != nil {
			return err
		}
		// Stop sending ticks so SessionTimeout fires.
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	voterB := newFakeProbeVoter(t)
	voterB.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     2,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "mon-from-b", TenantId: "t1", Type: "http", ConfigJson: []byte(`{"url":"https://b.example"}`), IntervalSeconds: 10, TimeoutSeconds: 5},
			},
		}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	ms, buf := newTestBuffer(t, "probe-1")
	syncer := newFakeMonitorSync()

	client, err := NewClient(ClientOptions{
		NodeID:         "probe-1",
		Cert:           newTestNodeCert(t, "probe-1", ""),
		Voters:         newTestVoterList(t, addrA, addrB),
		Buffer:         buf,
		Metrics:        ms,
		Sync:           syncer,
		SessionTimeout: 80 * time.Millisecond,
		ConnectTimeout: 80 * time.Millisecond,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA, addrB: voterB}),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	start := time.Now()
	first := syncer.waitCall(t, 400*time.Millisecond)
	if len(first) != 1 || first[0].ID != "mon-from-a" {
		t.Fatalf("expected initial snapshot from voter A, got %+v", first)
	}

	second := syncer.waitCall(t, 500*time.Millisecond)
	elapsed := time.Since(start)
	if len(second) != 1 || second[0].ID != "mon-from-b" {
		t.Fatalf("expected failover snapshot from voter B, got %+v", second)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("failover took %v, want < 500ms", elapsed)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestClientCandidateRotationAndFullJitterBackoff(t *testing.T) {
	const (
		addrA = "voter-a:443"
		addrB = "voter-b:443"
	)

	ms, buf := newTestBuffer(t, "probe-2")

	var mu sync.Mutex
	var events []string
	roundDone := make(chan struct{}, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewClient(ClientOptions{
		NodeID:         "probe-2",
		Cert:           newTestNodeCert(t, "probe-2", ""),
		Voters:         newTestVoterList(t, addrA, addrB),
		Buffer:         buf,
		Metrics:        ms,
		ConnectTimeout: 50 * time.Millisecond,
		BackoffMin:     20 * time.Millisecond,
		BackoffMax:     80 * time.Millisecond,
		Dial: func(_ context.Context, addr string) (*grpc.ClientConn, error) {
			mu.Lock()
			events = append(events, "dial:"+addr)
			mu.Unlock()
			return nil, errors.New("unreachable")
		},
		RandIntN: func(n int64) int64 {
			mu.Lock()
			events = append(events, "rand:"+time.Duration(n).String())
			mu.Unlock()
			select {
			case roundDone <- struct{}{}:
			default:
			}
			// Deterministic 5ms delay so backoff.Sleep runs quickly and deterministically.
			return min(int64(5*time.Millisecond), n-1)
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	// Wait for 2 full failed rounds (2 backoff calls).
	for i := range 2 {
		select {
		case <-roundDone:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for backoff round %d", i+1)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}

	mu.Lock()
	got := slices.Clone(events)
	mu.Unlock()

	wantPrefix := []string{
		"dial:" + addrA,
		"dial:" + addrB,
		"rand:20ms",
		"dial:" + addrA,
		"dial:" + addrB,
		"rand:40ms",
	}
	if len(got) < len(wantPrefix) {
		t.Fatalf("events = %v, want prefix %v", got, wantPrefix)
	}
	if !slices.Equal(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("events = %v, want prefix %v", got, wantPrefix)
	}
}

func TestClientSnapshotSyncPreservesUnchangedRevisions(t *testing.T) {
	const (
		addrA = "voter-a:443"
		addrB = "voter-b:443"
	)

	m1CfgA := []byte(`{"url":"https://one.example"}`)
	m1CfgA2 := []byte(`{"url":"https://one-updated.example"}`)
	m2CfgB := []byte(`{"url":"https://two.example"}`)

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     1,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m1", TenantId: "t1", Type: "http", ConfigJson: m1CfgA, IntervalSeconds: 10, TimeoutSeconds: 5},
				{Id: "m2", TenantId: "t1", Type: "http", ConfigJson: m2CfgB, IntervalSeconds: 10, TimeoutSeconds: 5},
			},
		}); err != nil {
			return err
		}
		// Cleanly close stream to trigger failover to Voter B.
		return nil
	}

	sendUpdate := make(chan struct{})
	voterB := newFakeProbeVoter(t)
	voterB.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     2,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m1", TenantId: "t1", Type: "http", ConfigJson: m1CfgA, IntervalSeconds: 10, TimeoutSeconds: 5},
				{Id: "m2", TenantId: "t1", Type: "http", ConfigJson: m2CfgB, IntervalSeconds: 10, TimeoutSeconds: 5},
			},
		}); err != nil {
			return err
		}
		select {
		case <-sendUpdate:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     3,
			FullSnapshot: false,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m1", TenantId: "t1", Type: "http", ConfigJson: m1CfgA2, IntervalSeconds: 10, TimeoutSeconds: 5},
			},
		}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	ms, buf := newTestBuffer(t, "probe-3")
	syncer := newFakeMonitorSync()

	client, err := NewClient(ClientOptions{
		NodeID:         "probe-3",
		Cert:           newTestNodeCert(t, "probe-3", ""),
		Voters:         newTestVoterList(t, addrA, addrB),
		Buffer:         buf,
		Metrics:        ms,
		Sync:           syncer,
		SessionTimeout: time.Second,
		ConnectTimeout: time.Second,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA, addrB: voterB}),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	call1 := syncer.waitCall(t, time.Second)
	if len(call1) != 2 || call1[0].Revision != 1 || call1[1].Revision != 1 {
		t.Fatalf("call1 = %+v, want m1 rev=1 and m2 rev=1", call1)
	}

	call2 := syncer.waitCall(t, time.Second)
	if len(call2) != 2 || call2[0].ID != "m1" || call2[0].Revision != 1 || call2[1].ID != "m2" || call2[1].Revision != 1 {
		t.Fatalf("call2 after identical failover snapshot = %+v, want m1 rev=1 and m2 rev=1", call2)
	}

	close(sendUpdate)
	call3 := syncer.waitCall(t, time.Second)
	if len(call3) != 2 || call3[0].ID != "m1" || call3[0].Revision != 2 || call3[1].ID != "m2" || call3[1].Revision != 1 {
		t.Fatalf("call3 after m1 update = %+v, want m1 rev=2 and m2 rev=1", call3)
	}

	cancel()
	<-done
}

func TestClientPostEstablishedDisconnectReconnectsWithoutBackoff(t *testing.T) {
	const (
		addrA = "voter-a:443"
		addrB = "voter-b:443"
	)

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     1,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m-a", TenantId: "t1", Type: "http"},
			},
		}); err != nil {
			return err
		}
		return nil // clean io.EOF (e.g. MaxConnectionAge)
	}

	voterB := newFakeProbeVoter(t)
	voterB.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     2,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m-b", TenantId: "t1", Type: "http"},
			},
		}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	ms, buf := newTestBuffer(t, "probe-4")
	syncer := newFakeMonitorSync()

	client, err := NewClient(ClientOptions{
		NodeID:         "probe-4",
		Cert:           newTestNodeCert(t, "probe-4", ""),
		Voters:         newTestVoterList(t, addrA, addrB),
		Buffer:         buf,
		Metrics:        ms,
		Sync:           syncer,
		BackoffMin:     5 * time.Second,
		BackoffMax:     30 * time.Second,
		SessionTimeout: time.Second,
		ConnectTimeout: time.Second,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA, addrB: voterB}),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	start := time.Now()
	first := syncer.waitCall(t, 400*time.Millisecond)
	if len(first) != 1 || first[0].ID != "m-a" {
		t.Fatalf("first sync = %+v, want m-a", first)
	}

	second := syncer.waitCall(t, 500*time.Millisecond)
	elapsed := time.Since(start)
	if len(second) != 1 || second[0].ID != "m-b" {
		t.Fatalf("second sync = %+v, want m-b", second)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("reconnect after clean EOF took %v (expected immediate without 5s backoff)", elapsed)
	}

	cancel()
	<-done
}

func TestClientErrCertRejectedCleanExit(t *testing.T) {
	const (
		addrA = "voter-a:443"
		addrB = "voter-b:443"
	)

	reject := func(_ *pb.SubscribeRequest, _ grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		return status.Error(codes.PermissionDenied, "certificate revoked")
	}

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = reject
	voterB := newFakeProbeVoter(t)
	voterB.onSubscribe = reject

	ms, buf := newTestBuffer(t, "probe-5")
	client, err := NewClient(ClientOptions{
		NodeID:         "probe-5",
		Cert:           newTestNodeCert(t, "probe-5", ""),
		Voters:         newTestVoterList(t, addrA, addrB),
		Buffer:         buf,
		Metrics:        ms,
		BackoffMin:     5 * time.Second,
		BackoffMax:     30 * time.Second,
		ConnectTimeout: time.Second,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA, addrB: voterB}),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err = client.Run(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCertRejected) {
		t.Fatalf("Run returned %v, want ErrCertRejected", err)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("ErrCertRejected took %v, expected prompt exit", elapsed)
	}
	if voterA.SubscribeCount() != 1 || voterB.SubscribeCount() != 1 {
		t.Fatalf("expected 1 subscribe attempt per voter, got A=%d B=%d", voterA.SubscribeCount(), voterB.SubscribeCount())
	}
}

func TestClientPrivateProbeD6TenantFilter(t *testing.T) {
	const addrA = "voter-a:443"

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     1,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{Id: "m-t1", TenantId: "t1", Type: "http", ConfigJson: []byte(`{"url":"https://t1.example"}`)},
				{Id: "m-t2", TenantId: "t2", Type: "http", ConfigJson: []byte(`{"url":"https://t2.example"}`)},
			},
		}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	ms, buf := newTestBuffer(t, "probe-6")
	syncer := newFakeMonitorSync()

	client, err := NewClient(ClientOptions{
		NodeID:         "probe-6",
		Cert:           newTestNodeCert(t, "probe-6", "t1"),
		Voters:         newTestVoterList(t, addrA),
		Buffer:         buf,
		Metrics:        ms,
		Sync:           syncer,
		SessionTimeout: time.Second,
		ConnectTimeout: time.Second,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA}),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	got := syncer.waitCall(t, time.Second)
	if len(got) != 1 || got[0].ID != "m-t1" || got[0].TenantID != "t1" {
		t.Fatalf("MonitorSync received %+v, want only m-t1 for tenant t1", got)
	}

	cancel()
	<-done
}

func TestClientSecretHygieneADR0034(t *testing.T) {
	const (
		addrA  = "voter-a:443"
		secret = "super-secret-probe-token-xyz"
	)

	voterA := newFakeProbeVoter(t)
	voterA.onSubscribe = func(_ *pb.SubscribeRequest, stream grpc.ServerStreamingServer[pb.SubscribeResponse]) error {
		if err := stream.Send(&pb.SubscribeResponse{
			Revision:     1,
			FullSnapshot: true,
			Upserts: []*pb.ProbeMonitor{
				{
					Id:              "mon-secret",
					TenantId:        "t1",
					Type:            "http",
					ConfigJson:      []byte(`{"url":"https://secret.example","bearer_token":"` + secret + `"}`),
					IntervalSeconds: 10,
					TimeoutSeconds:  5,
				},
			},
		}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	}

	dbDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ms, err := sqlite.OpenMetrics(ctx, dbDir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatalf("OpenMetrics: %v", err)
	}
	defer func() { _ = ms.Close() }()

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	buf, err := NewBuffer(ctx, BufferOptions{
		Store:  ms,
		NodeID: "probe-7",
		Log:    logger,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	syncer := newFakeMonitorSync()
	client, err := NewClient(ClientOptions{
		NodeID:         "probe-7",
		Cert:           newTestNodeCert(t, "probe-7", "t1"),
		Voters:         newTestVoterList(t, addrA),
		Buffer:         buf,
		Metrics:        ms,
		Sync:           syncer,
		SessionTimeout: time.Second,
		ConnectTimeout: time.Second,
		Dial:           newBufconnDialer(map[string]*fakeProbeVoter{addrA: voterA}),
		Log:            logger,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx)
	}()

	got := syncer.waitCall(t, time.Second)
	if len(got) != 1 || !bytes.Contains(got[0].Config, []byte(secret)) {
		t.Fatalf("expected in-memory MonitorSync to receive decrypted ConfigJson, got %+v", got)
	}

	hbs := []store.Heartbeat{
		{
			NodeID:      "probe-7",
			Incarnation: ms.Incarnation(),
			Seq:         1,
			TenantID:    "t1",
			MonitorID:   "mon-secret",
			TsMs:        time.Now().UnixMilli(),
			Status:      store.CheckUp,
			LatencyMs:   12,
		},
	}
	if _, err := ms.InsertHeartbeats(ctx, hbs); err != nil {
		t.Fatalf("InsertHeartbeats: %v", err)
	}
	client.NotifyNewHeartbeats(hbs)

	// Wait briefly for the upload & ack cycle to exercise Buffer and MetricsStore.
	deadline := time.Now().Add(time.Second)
	for buf.AckedSeq() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if buf.AckedSeq() != 1 {
		t.Fatalf("AckedSeq = %d, want 1", buf.AckedSeq())
	}

	cancel()
	<-done

	if bytes.Contains(logBuf.Bytes(), []byte(secret)) {
		t.Fatalf("secret leaked into slog output: %s", logBuf.Bytes())
	}

	for _, name := range []string{sqlite.MetricsFile, sqlite.MetricsFile + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dbDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret leaked into %s", name)
		}
	}
}

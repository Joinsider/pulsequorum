// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

const (
	mdNodeID   = "x-test-node-id"
	mdRole     = "x-test-role"
	mdTenantID = "x-test-tenant-id"
)

type fakeCluster struct {
	mu      sync.Mutex
	level   uint32
	members []cluster.Member
	applied []*pb.Command
}

func (f *fakeCluster) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, cmd)
	return cluster.ApplyResult{}, nil
}

func (f *fakeCluster) Members() ([]cluster.Member, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cluster.Member, len(f.members))
	copy(out, f.members)
	return out, 1, nil
}

func (f *fakeCluster) CommandLevel(_ context.Context) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.level, nil
}

func (f *fakeCluster) IsLeader() bool { return true }

type changeNotifier struct {
	mu   sync.Mutex
	next int
	subs map[int]chan struct{}
}

func newChangeNotifier() *changeNotifier {
	return &changeNotifier{subs: make(map[int]chan struct{})}
}

func (n *changeNotifier) subscribe() (<-chan struct{}, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.next
	n.next++
	ch := make(chan struct{}, 1)
	n.subs[id] = ch
	return ch, func() {
		n.mu.Lock()
		delete(n.subs, id)
		n.mu.Unlock()
	}
}

func (n *changeNotifier) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type fakeKeyring struct{}

func (fakeKeyring) Encrypt(plaintext []byte, aad string) (string, error) {
	return "enc:" + aad + ":" + string(plaintext), nil
}

func (fakeKeyring) Decrypt(_ context.Context, ciphertext, aad string) ([]byte, error) {
	prefix := "enc:" + aad + ":"
	if !strings.HasPrefix(ciphertext, prefix) {
		return nil, fmt.Errorf("invalid ciphertext or aad %q", aad)
	}
	return []byte(strings.TrimPrefix(ciphertext, prefix)), nil
}

type testEnv struct {
	t        *testing.T
	state    *sqlite.StateStore
	metrics  *sqlite.MetricsStore
	cluster  *fakeCluster
	notifier *changeNotifier
	srv      *Server
	client   pb.ProbeServiceClient
}

type envConfig struct {
	tickInterval     time.Duration
	debounceInterval time.Duration
	commandLevel     uint32
	keyring          configsecret.Keyring
}

func newTestEnv(t *testing.T, cfg envConfig) *testEnv {
	t.Helper()
	ctx := context.Background()

	state, err := sqlite.OpenState(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = state.Close() })

	metrics, err := sqlite.OpenMetrics(ctx, t.TempDir(), sqlite.MetricsOptions{})
	if err != nil {
		t.Fatalf("OpenMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Close() })

	level := cfg.commandLevel
	if level == 0 {
		level = cluster.ProbeCommandLevel
	}
	fc := &fakeCluster{
		level: level,
		members: []cluster.Member{
			{ID: "voter-1", Address: "127.0.0.1:9001", Voter: true},
		},
	}
	notifier := newChangeNotifier()

	srv := New(Options{
		NodeID:           "voter-1",
		State:            state,
		Metrics:          metrics,
		Cluster:          fc,
		SubscribeChanges: notifier.subscribe,
		Assigner:         scheduler.Assigner{},
		Keyring:          cfg.keyring,
		TickInterval:     cfg.tickInterval,
		DebounceInterval: cfg.debounceInterval,
		identity: func(ctx context.Context) (pki.Identity, bool) {
			md, ok := metadata.FromIncomingContext(ctx)
			if !ok {
				return pki.Identity{}, false
			}
			vals := md.Get(mdNodeID)
			if len(vals) == 0 || vals[0] == "" {
				return pki.Identity{}, false
			}
			role := pki.RoleProbe
			if r := md.Get(mdRole); len(r) > 0 && r[0] != "" {
				role = pki.Role(r[0])
			}
			var tenantID string
			if tid := md.Get(mdTenantID); len(tid) > 0 {
				tenantID = tid[0]
			}
			return pki.Identity{
				ClusterID: "cluster-1",
				NodeID:    vals[0],
				Role:      role,
				TenantID:  tenantID,
			}, true
		},
	})

	runCtx, cancelRun := context.WithCancel(ctx)
	doneRun := make(chan struct{})
	go func() {
		defer close(doneRun)
		srv.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancelRun()
		<-doneRun
	})

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := grpc.NewServer()
	pb.RegisterProbeServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &testEnv{
		t:        t,
		state:    state,
		metrics:  metrics,
		cluster:  fc,
		notifier: notifier,
		srv:      srv,
		client:   pb.NewProbeServiceClient(conn),
	}
}

func (e *testEnv) probeCtx(ctx context.Context, nodeID, tenantID string) context.Context {
	pairs := []string{mdNodeID, nodeID, mdRole, string(pki.RoleProbe)}
	if tenantID != "" {
		pairs = append(pairs, mdTenantID, tenantID)
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...))
}

func (e *testEnv) seedTenant(id string) {
	e.t.Helper()
	err := e.state.Update(context.Background(), func(tx store.StateTx) error {
		return tx.PutTenant(context.Background(), store.Tenant{
			ID:   id,
			Name: id,
		})
	})
	if err != nil {
		e.t.Fatalf("PutTenant(%s): %v", id, err)
	}
}

func (e *testEnv) seedProbe(id, ownerTenantID string) {
	e.t.Helper()
	err := e.state.Update(context.Background(), func(tx store.StateTx) error {
		return tx.PutNode(context.Background(), store.Node{
			ID:            id,
			Name:          id,
			Role:          store.NodeRoleProbe,
			OwnerTenantID: ownerTenantID,
		})
	})
	if err != nil {
		e.t.Fatalf("PutNode(%s): %v", id, err)
	}
}

func (e *testEnv) seedMonitor(m store.Monitor) {
	e.t.Helper()
	if m.Type == "" {
		m.Type = "http"
	}
	if len(m.Config) == 0 {
		m.Config = json.RawMessage(`{"url":"https://example.com"}`)
	}
	if m.IntervalSeconds == 0 {
		m.IntervalSeconds = 60
	}
	if m.TimeoutSeconds == 0 {
		m.TimeoutSeconds = 10
	}
	if m.Assignment.Mode == "" {
		m.Assignment = store.Assignment{Mode: "all"}
	}
	err := e.state.Update(context.Background(), func(tx store.StateTx) error {
		return tx.PutMonitor(context.Background(), m, 0)
	})
	if err != nil {
		e.t.Fatalf("PutMonitor(%s): %v", m.ID, err)
	}
}

func TestSnapshotChunking(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")

	const totalMonitors = 520
	err := env.state.Update(context.Background(), func(tx store.StateTx) error {
		for i := range totalMonitors {
			if err := tx.PutMonitor(context.Background(), store.Monitor{
				TenantID:        "t1",
				ID:              fmt.Sprintf("mon-%04d", i),
				Name:            fmt.Sprintf("Monitor %d", i),
				Type:            "http",
				Config:          json.RawMessage(`{"url":"https://example.com"}`),
				IntervalSeconds: 60,
				TimeoutSeconds:  10,
				Assignment:      store.Assignment{Mode: "all"},
			}, 0); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed monitors: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var (
		chunks   []*pb.SubscribeResponse
		gotIDs   = make(map[string]bool, totalMonitors)
		received int
	)
	for received < totalMonitors {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, resp)
		for _, u := range resp.GetUpserts() {
			gotIDs[u.GetId()] = true
			received++
		}
	}

	if len(chunks) != 2 {
		t.Fatalf("expected 2 snapshot chunks, got %d", len(chunks))
	}
	if !chunks[0].GetFullSnapshot() {
		t.Errorf("chunk 0 FullSnapshot = false, want true")
	}
	if chunks[0].GetStreamPosition() == nil {
		t.Errorf("chunk 0 StreamPosition = nil, want non-nil")
	}
	if len(chunks[0].GetVoters()) != 1 {
		t.Errorf("chunk 0 Voters = %d, want 1", len(chunks[0].GetVoters()))
	}
	if chunks[0].GetServerTimeMs() == 0 {
		t.Errorf("chunk 0 ServerTimeMs = 0, want > 0")
	}

	if chunks[1].GetFullSnapshot() {
		t.Errorf("chunk 1 FullSnapshot = true, want false")
	}
	if chunks[1].GetStreamPosition() != nil {
		t.Errorf("chunk 1 StreamPosition = %v, want nil", chunks[1].GetStreamPosition())
	}
	if len(chunks[1].GetVoters()) != 0 {
		t.Errorf("chunk 1 Voters = %d, want 0", len(chunks[1].GetVoters()))
	}
	if chunks[1].GetServerTimeMs() != 0 {
		t.Errorf("chunk 1 ServerTimeMs = %d, want 0", chunks[1].GetServerTimeMs())
	}
	if len(gotIDs) != totalMonitors {
		t.Errorf("unique monitors = %d, want %d", len(gotIDs), totalMonitors)
	}
}

func TestDeltaUpsertAndRemoval(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 20 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1", IntervalSeconds: 60})
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m3", Name: "m3", IntervalSeconds: 60})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	snap, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if !snap.GetFullSnapshot() || len(snap.GetUpserts()) != 2 {
		t.Fatalf("unexpected initial snapshot: %+v", snap)
	}

	// Update m1, add m2, and remove m3, then trigger change notification.
	err = env.state.Update(ctx, func(tx store.StateTx) error {
		if err := tx.DeleteMonitor(ctx, "t1", "m3"); err != nil {
			return err
		}
		if err := tx.PutMonitor(ctx, store.Monitor{
			TenantID:        "t1",
			ID:              "m1",
			Name:            "m1",
			Type:            "http",
			Config:          json.RawMessage(`{"url":"https://example.com"}`),
			IntervalSeconds: 30,
			TimeoutSeconds:  10,
			Assignment:      store.Assignment{Mode: "all"},
		}, 1); err != nil {
			return err
		}
		return tx.PutMonitor(ctx, store.Monitor{
			TenantID:        "t1",
			ID:              "m2",
			Name:            "m2",
			Type:            "http",
			Config:          json.RawMessage(`{"url":"https://example.com"}`),
			IntervalSeconds: 60,
			TimeoutSeconds:  10,
			Assignment:      store.Assignment{Mode: "all"},
		}, 0)
	})
	if err != nil {
		t.Fatalf("update monitors: %v", err)
	}
	env.notifier.notify()

	delta, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv delta: %v", err)
	}
	if delta.GetFullSnapshot() {
		t.Errorf("delta FullSnapshot = true, want false")
	}
	if len(delta.GetRemovedMonitorIds()) != 1 || delta.GetRemovedMonitorIds()[0] != "m3" {
		t.Errorf("delta RemovedMonitorIds = %v, want [m3]", delta.GetRemovedMonitorIds())
	}
	gotUpserts := make(map[string]uint32, len(delta.GetUpserts()))
	for _, u := range delta.GetUpserts() {
		gotUpserts[u.GetId()] = u.GetIntervalSeconds()
	}
	if len(gotUpserts) != 2 || gotUpserts["m1"] != 30 || gotUpserts["m2"] != 60 {
		t.Errorf("delta Upserts = %v, want map[m1:30 m2:60]", gotUpserts)
	}
}

func TestKeepaliveTick(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     30 * time.Millisecond,
		debounceInterval: 5 * time.Second,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	snap, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if !snap.GetFullSnapshot() {
		t.Fatalf("first response FullSnapshot = false, want true")
	}

	for i := range 2 {
		tick, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv tick %d: %v", i, err)
		}
		if tick.GetFullSnapshot() {
			t.Errorf("tick %d FullSnapshot = true, want false", i)
		}
		if len(tick.GetUpserts()) != 0 || len(tick.GetRemovedMonitorIds()) != 0 {
			t.Errorf("tick %d not empty: upserts=%d removed=%d", i, len(tick.GetUpserts()), len(tick.GetRemovedMonitorIds()))
		}
		if tick.GetServerTimeMs() <= 0 {
			t.Errorf("tick %d ServerTimeMs = %d, want > 0", i, tick.GetServerTimeMs())
		}
	}
}

func TestSessionReplacement(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Subscribe replacement.
	sub1, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	if _, err := sub1.Recv(); err != nil {
		t.Fatalf("Subscribe 1 snapshot: %v", err)
	}

	sub2, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	if _, err := sub2.Recv(); err != nil {
		t.Fatalf("Subscribe 2 snapshot: %v", err)
	}

	if _, err := sub1.Recv(); err == nil {
		t.Fatalf("expected first Subscribe stream to close when second connected")
	}

	// UploadHeartbeats replacement.
	up1, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p1", ""))
	if err != nil {
		t.Fatalf("UploadHeartbeats 1: %v", err)
	}
	if err := up1.Send(&pb.UploadHeartbeatsRequest{}); err != nil {
		t.Fatalf("up1.Send: %v", err)
	}
	if _, err := up1.Recv(); err != nil {
		t.Fatalf("up1.Recv: %v", err)
	}

	up2, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p1", ""))
	if err != nil {
		t.Fatalf("UploadHeartbeats 2: %v", err)
	}
	if err := up2.Send(&pb.UploadHeartbeatsRequest{}); err != nil {
		t.Fatalf("up2.Send: %v", err)
	}
	if _, err := up2.Recv(); err != nil {
		t.Fatalf("up2.Recv: %v", err)
	}

	if _, err := up1.Recv(); err == nil {
		t.Fatalf("expected first UploadHeartbeats stream to close when second connected")
	}
}

func TestRevocationClosesLiveStream(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}

	if err := env.state.Update(ctx, func(tx store.StateTx) error {
		return tx.PutRevokedNode(ctx, store.RevokedNode{
			NodeID:    "p1",
			RevokedMs: time.Now().UnixMilli(),
		})
	}); err != nil {
		t.Fatalf("PutRevokedNode: %v", err)
	}
	env.notifier.notify()

	if _, err := stream.Recv(); err == nil {
		t.Fatalf("expected active Subscribe stream to terminate after revocation")
	}

	stream2, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err == nil {
		_, err = stream2.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked Subscribe code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
	}
}

func TestBelowCommandLevel10(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     30 * time.Millisecond,
		debounceInterval: 250 * time.Millisecond,
		commandLevel:     9,
	})
	env.seedTenant("t1")
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	snap, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if !snap.GetFullSnapshot() {
		t.Errorf("FullSnapshot = false, want true")
	}
	if len(snap.GetUpserts()) != 0 {
		t.Errorf("Upserts = %d, want 0 below level 10", len(snap.GetUpserts()))
	}

	tick, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv tick: %v", err)
	}
	if tick.GetServerTimeMs() <= 0 || tick.GetFullSnapshot() || len(tick.GetUpserts()) != 0 {
		t.Errorf("unexpected tick below level 10: %+v", tick)
	}
}

func TestUploadIdentityAndTenantBoundary(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedTenant("t2")
	env.seedProbe("p1", "")
	env.seedProbe("p-priv", "t1")
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1"})
	env.seedMonitor(store.Monitor{TenantID: "t2", ID: "m2", Name: "m2"})

	nowMs := time.Now().UnixMilli()

	t.Run("mismatched node_id", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		stream, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p1", ""))
		if err != nil {
			t.Fatalf("UploadHeartbeats: %v", err)
		}
		if err := stream.Send(&pb.UploadHeartbeatsRequest{
			Heartbeats: []*pb.Heartbeat{
				{
					NodeId:      "p1",
					Incarnation: "inc-1",
					Seq:         1,
					TenantId:    "t1",
					MonitorId:   "m1",
					TsMs:        nowMs,
					Status:      pb.CheckStatus_CHECK_STATUS_UP,
				},
				{
					NodeId:      "p2",
					Incarnation: "inc-1",
					Seq:         2,
					TenantId:    "t1",
					MonitorId:   "m1",
					TsMs:        nowMs,
					Status:      pb.CheckStatus_CHECK_STATUS_UP,
				},
			},
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}

		_, err = stream.Recv()
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Recv code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
		}

		if seq, err := env.metrics.LastSeq(ctx, "p1", "inc-1"); err != nil || seq != 0 {
			t.Fatalf("p1 LastSeq = %d (err=%v), want 0", seq, err)
		}
		if seq, err := env.metrics.LastSeq(ctx, "p2", "inc-1"); err != nil || seq != 0 {
			t.Fatalf("p2 LastSeq = %d (err=%v), want 0", seq, err)
		}
	})

	t.Run("private probe cross-tenant upload", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		stream, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p-priv", "t1"))
		if err != nil {
			t.Fatalf("UploadHeartbeats: %v", err)
		}
		if err := stream.Send(&pb.UploadHeartbeatsRequest{
			Heartbeats: []*pb.Heartbeat{
				{
					NodeId:      "p-priv",
					Incarnation: "inc-priv",
					Seq:         1,
					TenantId:    "t2",
					MonitorId:   "m2",
					TsMs:        nowMs,
					Status:      pb.CheckStatus_CHECK_STATUS_UP,
				},
			},
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}

		_, err = stream.Recv()
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Recv code = %v, want PermissionDenied (err=%v)", status.Code(err), err)
		}

		if seq, err := env.metrics.LastSeq(ctx, "p-priv", "inc-priv"); err != nil || seq != 0 {
			t.Fatalf("p-priv LastSeq = %d (err=%v), want 0", seq, err)
		}
	})
}

func TestDeletedMonitorSilentlyDropped(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p1", ""))
	if err != nil {
		t.Fatalf("UploadHeartbeats: %v", err)
	}

	nowMs := time.Now().UnixMilli()
	if err := stream.Send(&pb.UploadHeartbeatsRequest{
		Heartbeats: []*pb.Heartbeat{
			{
				NodeId:      "p1",
				Incarnation: "inc-1",
				Seq:         1,
				TenantId:    "t1",
				MonitorId:   "m1",
				TsMs:        nowMs,
				Status:      pb.CheckStatus_CHECK_STATUS_UP,
			},
			{
				NodeId:      "p1",
				Incarnation: "inc-1",
				Seq:         2,
				TenantId:    "t1",
				MonitorId:   "m-deleted",
				TsMs:        nowMs,
				Status:      pb.CheckStatus_CHECK_STATUS_UP,
			},
			{
				NodeId:      "p1",
				Incarnation: "inc-1",
				Seq:         3,
				TenantId:    "t1",
				MonitorId:   "m1",
				TsMs:        nowMs,
				Status:      pb.CheckStatus_CHECK_STATUS_UP,
			},
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if resp.GetPersisted().GetSeq() != 3 {
		t.Fatalf("Persisted.Seq = %d, want 3", resp.GetPersisted().GetSeq())
	}

	stored, _, err := env.metrics.HeartbeatsAfter(ctx, "p1", "inc-1", 0, 10, 10)
	if err != nil {
		t.Fatalf("HeartbeatsAfter: %v", err)
	}
	if len(stored) != 2 || stored[0].Seq != 1 || stored[1].Seq != 3 {
		t.Fatalf("stored heartbeats = %+v, want seqs [1, 3]", stored)
	}
}

func TestLostBelowScopedToCaller(t *testing.T) {
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")
	env.seedProbe("p2", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := env.metrics.AdvanceStreamPosition(ctx, store.StreamPosition{
		NodeID:      "p2",
		Incarnation: "inc-2",
		Seq:         10,
	}); err != nil {
		t.Fatalf("AdvanceStreamPosition p2: %v", err)
	}

	// Subscribe p1 so the session records incarnation "inc-1".
	sub, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe p1: %v", err)
	}
	if _, err := sub.Recv(); err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}

	up, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p1", ""))
	if err != nil {
		t.Fatalf("UploadHeartbeats p1: %v", err)
	}
	if err := up.Send(&pb.UploadHeartbeatsRequest{LostBelow: 5}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	resp, err := up.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if resp.GetPersisted().GetSeq() != 4 {
		t.Fatalf("p1 Persisted.Seq = %d, want 4", resp.GetPersisted().GetSeq())
	}

	positions, err := env.metrics.StreamPositions(ctx)
	if err != nil {
		t.Fatalf("StreamPositions: %v", err)
	}
	got := make(map[string]uint64, len(positions))
	for _, p := range positions {
		got[p.NodeID] = p.Seq
	}
	if got["p1"] != 4 {
		t.Errorf("p1 stream position = %d, want 4", got["p1"])
	}
	if got["p2"] != 10 {
		t.Errorf("p2 stream position = %d, want 10", got["p2"])
	}
}

func TestSecretResolution(t *testing.T) {
	kr := fakeKeyring{}
	env := newTestEnv(t, envConfig{
		tickInterval:     5 * time.Second,
		debounceInterval: 250 * time.Millisecond,
		keyring:          kr,
	})
	env.seedTenant("t1")
	env.seedProbe("p1", "")

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string"},
			"password": {"type": "string", "writeOnly": true}
		}
	}`)
	paths, err := configsecret.PathsOf(schema)
	if err != nil {
		t.Fatalf("PathsOf: %v", err)
	}
	rawCfg := json.RawMessage(`{"url":"https://example.com","password":"super-secret-value"}`)
	maskedCfg, secretsMap, err := configsecret.Split(kr, paths, "m-secret", rawCfg, nil)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if strings.Contains(string(maskedCfg), "super-secret-value") {
		t.Fatalf("masked config unexpectedly contains plaintext: %s", maskedCfg)
	}

	env.seedMonitor(store.Monitor{
		TenantID: "t1",
		ID:       "m-secret",
		Name:     "Secret Monitor",
		Type:     "http",
		Config:   maskedCfg,
		Secrets:  secretsMap,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := env.client.Subscribe(env.probeCtx(ctx, "p1", ""), &pb.SubscribeRequest{
		ProtocolVersion: 1,
		Incarnation:     "inc-1",
		Name:            "p1",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	snap, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv snapshot: %v", err)
	}
	if len(snap.GetUpserts()) != 1 {
		t.Fatalf("Upserts = %d, want 1", len(snap.GetUpserts()))
	}

	var decoded map[string]string
	if err := json.Unmarshal(snap.GetUpserts()[0].GetConfigJson(), &decoded); err != nil {
		t.Fatalf("Unmarshal ConfigJson: %v", err)
	}
	if decoded["password"] != "super-secret-value" {
		t.Errorf("resolved password = %q, want %q", decoded["password"], "super-secret-value")
	}
	if decoded["url"] != "https://example.com" {
		t.Errorf("resolved url = %q, want %q", decoded["url"], "https://example.com")
	}
}

// Keep errors import used if needed.
var _ = errors.Is

func TestOnRejectedCallbacks(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var reasons []string

	env := newTestEnv(t, envConfig{tickInterval: 5 * time.Second, debounceInterval: 50 * time.Millisecond})
	env.srv.opts.OnRejected = func(reason string) {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
	}

	env.seedTenant("t1")
	env.seedTenant("t2")
	env.seedProbe("p-revoked", "")
	if err := env.state.Update(ctx, func(tx store.StateTx) error {
		return tx.PutRevokedNode(ctx, store.RevokedNode{NodeID: "p-revoked", RevokedMs: 1})
	}); err != nil {
		t.Fatal(err)
	}
	env.seedProbe("p-priv", "t1")

	// 1. Non-probe role -> "non_probe_role"
	voterCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs(mdNodeID, "v1", mdRole, string(pki.RoleVoter)))
	stream, err := env.client.Subscribe(voterCtx, &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: "i", Name: "v1"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-probe code = %v, want PermissionDenied", status.Code(err))
	}

	// 2. Revoked probe -> "revoked"
	revStream, err := env.client.Subscribe(env.probeCtx(ctx, "p-revoked", ""), &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: "i", Name: "p-revoked"})
	if err == nil {
		_, err = revStream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked code = %v, want PermissionDenied", status.Code(err))
	}

	// 3. Unregistered probe -> "unregistered"
	unregStream, err := env.client.Subscribe(env.probeCtx(ctx, "p-unknown", ""), &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: "i", Name: "p-unknown"})
	if err == nil {
		_, err = unregStream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unregistered code = %v, want PermissionDenied", status.Code(err))
	}

	// 4. Owner mismatch -> "owner_mismatch"
	mismatchStream, err := env.client.Subscribe(env.probeCtx(ctx, "p-priv", "t2"), &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: "i", Name: "p-priv"})
	if err == nil {
		_, err = mismatchStream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("mismatch code = %v, want PermissionDenied", status.Code(err))
	}

	// 5. D3 foreign_monitor in collectTenantMonitors
	out := snapshot{monitors: map[string]resolvedMonitor{}}
	env.seedMonitor(store.Monitor{TenantID: "t2", ID: "m-foreign", Name: "Foreign", Type: "http", IntervalSeconds: 30, Config: json.RawMessage(`{"url":"https://example.com"}`)})
	if err := env.srv.collectTenantMonitors(ctx, pki.Identity{NodeID: "p-priv", Role: pki.RoleProbe, TenantID: "t1"}, "t2", nil, &out); err != nil {
		t.Fatalf("collectTenantMonitors: %v", err)
	}
	if len(out.monitors) != 0 {
		t.Fatalf("expected foreign monitor to be dropped, got %d", len(out.monitors))
	}

	// 6. D4 upload_identity
	up, err := env.client.UploadHeartbeats(env.probeCtx(ctx, "p-priv", "t1"))
	if err != nil {
		t.Fatalf("UploadHeartbeats: %v", err)
	}
	_ = up.Send(&pb.UploadHeartbeatsRequest{
		Heartbeats: []*pb.Heartbeat{{
			NodeId: "p-priv", Incarnation: "i", Seq: 1, TenantId: "t2", MonitorId: "m1", TsMs: time.Now().UnixMilli(),
		}},
	})
	if _, err := up.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign upload code = %v, want PermissionDenied", status.Code(err))
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"non_probe_role", "revoked", "unregistered", "owner_mismatch", "foreign_monitor", "upload_identity"} {
		found := false
		for _, r := range reasons {
			if r == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing rejection reason %q in %v", want, reasons)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/clusternet"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/probe"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

func createTestTenant(ctx context.Context, t *testing.T, leader *testNode, id, limitsJSON string) {
	t.Helper()
	if limitsJSON == "" {
		limitsJSON = "{}"
	}
	cmd := &pb.Command{
		RequestId: "tenant-" + id + "-" + uuid.NewString(),
		Actor:     &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-platform", Name: "Platform"},
		Payload: &pb.Command_CreateTenant{CreateTenant: &pb.CreateTenant{
			TenantId:   id,
			Name:       id,
			PlanId:     "default",
			LimitsJson: limitsJSON,
			Source:     "admin",
		}},
	}
	res, err := leader.host.Node.Apply(ctx, cmd)
	if err != nil || res.Err != nil {
		t.Fatalf("create tenant %s: err=%v cmdErr=%v", id, err, res.Err)
	}
}

func putTenantHTTPMonitor(ctx context.Context, t *testing.T, leader *testNode, tenantID, id, rawURL string, assign store.Assignment) {
	t.Helper()
	cfgJSON, err := json.Marshal(map[string]any{"url": rawURL})
	if err != nil {
		t.Fatal(err)
	}
	doc := cluster.MonitorDocument{
		Name:              id,
		Type:              "http",
		Config:            cfgJSON,
		IntervalSeconds:   1,
		TimeoutSeconds:    2,
		Retries:           1,
		RecoveryThreshold: 1,
		Assignment:        assign,
		ConsensusPolicy:   "any",
	}
	cmd, err := cluster.NewPutMonitor("put-"+tenantID+"-"+id+"-"+uuid.NewString(), tenantID, id, doc, 0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := leader.host.Node.Apply(ctx, cmd)
	if err != nil || res.Err != nil {
		t.Fatalf("put monitor %s/%s: err=%v cmdErr=%v", tenantID, id, err, res.Err)
	}
}

func dialRawProbeService(t *testing.T, addr string, p *testProbe) pb.ProbeServiceClient {
	t.Helper()
	verifier := &pki.Verifier{
		ClusterIDFunc: func() string {
			if leaf := p.cert.Leaf(); leaf != nil {
				if id, err := pki.IdentityOf(leaf); err == nil {
					return id.ClusterID
				}
			}
			return ""
		},
		Roots: p.cert.Bundle,
	}
	conn, err := clusternet.DialProbe(addr, clusternet.Security{Holder: &p.cert.Holder, Verifier: verifier})
	if err != nil {
		t.Fatalf("dial ProbeService: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewProbeServiceClient(conn)
}

func hasReason(reasons *[]string, mu *sync.Mutex, want string) bool {
	mu.Lock()
	defer mu.Unlock()
	return slices.Contains(*reasons, want)
}

// TestPrivateProbeTenantIsolation verifies ADR 0035 defense-in-depth D1, D3,
// D4, and cert-vs-registry ownership checks across a multi-node cluster.
func TestPrivateProbeTenantIsolation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var rejMu sync.Mutex
	var rejReasons []string
	recordReject := func(reason string) {
		rejMu.Lock()
		rejReasons = append(rejReasons, reason)
		rejMu.Unlock()
	}

	n1, err := launch(ctx, t, &testNode{
		id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(),
		replInterval: 200 * time.Millisecond, onProbeRejected: recordReject,
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 15*time.Second, n1.host.Node.IsLeader)

	joinTok := func() string {
		tok, _, err := n1.host.Authority.CreateJoinToken(ctx, pki.RoleVoter, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	n2, err := launch(ctx, t, &testNode{
		id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(),
		replInterval: 200 * time.Millisecond, onProbeRejected: recordReject,
	}, []string{n1.addr}, joinTok())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n2 voter", 30*time.Second, func() bool { return isVoter(n1, n2.id) })

	n3, err := launch(ctx, t, &testNode{
		id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(),
		replInterval: 200 * time.Millisecond, onProbeRejected: recordReject,
	}, []string{n2.addr}, joinTok())
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n3 voter", 30*time.Second, func() bool { return isVoter(n1, n3.id) })

	voters := []*testNode{n1, n2, n3}
	eventually(t, "voters reach command version", 30*time.Second, func() bool {
		for _, v := range voters {
			lvl, err := v.host.Node.CommandLevel(ctx)
			if err != nil || lvl < cluster.CommandVersion {
				return false
			}
		}
		return true
	})

	createTestTenant(ctx, t, n1, "tenant-a", "")
	createTestTenant(ctx, t, n1, "tenant-b", "")

	var hitsAll, hitsCount, hitsNodes, hitsForeign atomic.Int64
	srvAll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsAll.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvAll.Close()
	srvCount := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvCount.Close()
	srvNodes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsNodes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvNodes.Close()
	srvForeign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitsForeign.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvForeign.Close()

	probeA := startTestProbe(ctx, t, n1, testProbeOptions{name: "probe-a", tenantID: "tenant-a", unit: 100 * time.Millisecond})
	probeB := startTestProbe(ctx, t, n2, testProbeOptions{name: "probe-b", tenantID: "tenant-b", unit: 100 * time.Millisecond})

	eventually(t, "both private probes registered with owner tenants", 15*time.Second, func() bool {
		reg := registry(t, n1)
		return reg[probeA.id].OwnerTenantID == "tenant-a" && reg[probeB.id].OwnerTenantID == "tenant-b"
	})

	// D1: probe-a must NOT execute mode=all or mode=count monitors of tenant-a,
	// nor tenant-b's monitors; it ONLY executes mode=nodes naming probe-a.
	putTenantHTTPMonitor(ctx, t, n1, "tenant-a", "m-all", srvAll.URL, store.Assignment{Mode: "all"})
	putTenantHTTPMonitor(ctx, t, n1, "tenant-a", "m-count", srvCount.URL, store.Assignment{Mode: "count", Count: 1})
	putTenantHTTPMonitor(ctx, t, n1, "tenant-a", "m-nodes", srvNodes.URL, store.Assignment{Mode: "nodes", Nodes: []string{probeA.id}})
	putTenantHTTPMonitor(ctx, t, n1, "tenant-b", "m-foreign", srvForeign.URL, store.Assignment{Mode: "nodes", Nodes: []string{probeA.id}})

	eventually(t, "probe-a executes own assigned monitor", 10*time.Second, func() bool {
		return hitsNodes.Load() >= 3
	})
	if hitsAll.Load() != 0 || hitsCount.Load() != 0 || hitsForeign.Load() != 0 {
		t.Fatalf("unexpected hits: all=%d count=%d foreign=%d (want all 0)", hitsAll.Load(), hitsCount.Load(), hitsForeign.Load())
	}

	// D4: probe-a forging tenant-b heartbeat receives PermissionDenied + OnRejected("upload_identity")
	// without persisting heartbeats or advancing stream position; lost_below stays scoped to probe-a.
	cli := dialRawProbeService(t, n1.addr, probeA)
	up, err := cli.UploadHeartbeats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Send(&pb.UploadHeartbeatsRequest{
		Heartbeats: []*pb.Heartbeat{{
			NodeId: probeA.id, Incarnation: probeA.incarnation, Seq: 999,
			TenantId: "tenant-b", MonitorId: "m-foreign", TsMs: time.Now().UnixMilli(),
		}},
		LostBelow: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged upload status = %v, want PermissionDenied", status.Code(err))
	}
	_ = up.CloseSend()
	if !hasReason(&rejReasons, &rejMu, "upload_identity") {
		t.Errorf("expected OnRejected(upload_identity), got %v", rejReasons)
	}
	// Forged request must NOT have stored seq 999 or advanced stream position via LostBelow=500.
	posList, err := n1.metrics.StreamPositions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pos := range posList {
		if pos.NodeID == probeA.id && pos.Seq >= 500 {
			t.Fatalf("forged request advanced stream position to %d", pos.Seq)
		}
		if pos.NodeID == probeB.id && pos.Seq >= 500 {
			t.Fatalf("forged request touched probe-b stream position %d", pos.Seq)
		}
	}

	// Cert vs registry owner mismatch: tamper probe-a's local registry owner on n1 -> Subscribe rejected.
	if err := n1.state.Update(ctx, func(tx store.StateTx) error {
		return tx.PutNode(ctx, store.Node{
			ID: probeA.id, Name: "probe-a", Role: store.NodeRoleProbe, OwnerTenantID: "tenant-b",
		})
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := cli.Subscribe(ctx, &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: probeA.incarnation, Name: "probe-a"})
	if err == nil {
		_, err = sub.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("owner mismatch Subscribe status = %v, want PermissionDenied", status.Code(err))
	}
	if !hasReason(&rejReasons, &rejMu, "owner_mismatch") {
		t.Errorf("expected OnRejected(owner_mismatch), got %v", rejReasons)
	}
}

// TestPrivateProbeConcurrentLimit verifies ADR 0035 atomic FSM enforcement of
// maxPrivateProbes=1 when two concurrent join-token creations race across voters.
func TestPrivateProbeConcurrentLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 300*time.Millisecond)
	leader := findLeader(voters)
	follower := voters[1]
	if follower == leader {
		follower = voters[2]
	}

	createTestTenant(ctx, t, leader, "t-limited", `{"maxPrivateProbes":1}`)

	actor := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
	errs := make(chan error, 2)
	var startWg sync.WaitGroup
	startWg.Add(1)
	for _, target := range []*testNode{leader, follower} {
		go func(node *testNode) {
			startWg.Wait()
			_, _, err := node.host.Authority.CreateJoinTokenFor(ctx, pki.RoleProbe, "t-limited", time.Hour, actor)
			errs <- err
		}(target)
	}
	startWg.Done()

	err1, err2 := <-errs, <-errs
	var succeeded, limitErr int
	for _, err := range []error{err1, err2} {
		if err == nil {
			succeeded++
			continue
		}
		var ce *cluster.CommandError
		if errors.As(err, &ce) && ce.Code == cluster.CodeLimitExceeded {
			limitErr++
			continue
		}
		t.Fatalf("unexpected concurrent CreateJoinTokenFor error: %v", err)
	}
	if succeeded != 1 || limitErr != 1 {
		t.Fatalf("concurrent limit race: succeeded=%d limitErr=%d (err1=%v err2=%v)", succeeded, limitErr, err1, err2)
	}
}

// TestPrivateProbeDeleteTenantRevocation verifies ADR 0035 D8: DeleteTenant
// revokes all private probes of that tenant, terminates live streams, and
// blocks reconnections while other tenants keep running.
func TestPrivateProbeDeleteTenantRevocation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 200*time.Millisecond)
	leader := voters[0]

	createTestTenant(ctx, t, leader, "tenant-a", "")
	createTestTenant(ctx, t, leader, "tenant-b", "")

	probeA := startTestProbe(ctx, t, leader, testProbeOptions{name: "probe-a", tenantID: "tenant-a", unit: 200 * time.Millisecond})
	probeB := startTestProbe(ctx, t, leader, testProbeOptions{name: "probe-b", tenantID: "tenant-b", unit: 200 * time.Millisecond})

	eventually(t, "both probes connected", 15*time.Second, func() bool {
		sess := leader.host.ProbeSessions()
		return sess[probeA.id].ConnectedVoterID != "" && sess[probeB.id].ConnectedVoterID != ""
	})

	// Suspend and delete tenant-a.
	actor := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-platform", Name: "Platform"}
	suspCmd := &pb.Command{
		RequestId: "susp-a",
		Actor:     actor,
		Payload:   &pb.Command_SetTenantSuspended{SetTenantSuspended: &pb.SetTenantSuspended{TenantId: "tenant-a", Suspended: true}},
	}
	if res, err := leader.host.Node.Apply(ctx, suspCmd); err != nil || res.Err != nil {
		t.Fatalf("suspend tenant-a: err=%v cmdErr=%v", err, res.Err)
	}
	delCmd := &pb.Command{
		RequestId: "del-a",
		Actor:     actor,
		Payload:   &pb.Command_DeleteTenant{DeleteTenant: &pb.DeleteTenant{TenantId: "tenant-a"}},
	}
	if res, err := leader.host.Node.Apply(ctx, delCmd); err != nil || res.Err != nil {
		t.Fatalf("delete tenant-a: err=%v cmdErr=%v", err, res.Err)
	}

	// Reload revocations so TLS/gRPC handshake check sees the revocation too.
	for _, v := range voters {
		if err := v.host.Revocations.Reload(ctx, v.state); err != nil {
			t.Fatal(err)
		}
	}

	// probeA exits promptly with ErrCertRejected; probeB remains alive.
	select {
	case err := <-probeA.errCh:
		if !errors.Is(err, probe.ErrCertRejected) {
			t.Fatalf("probeA exit err = %v, want ErrCertRejected", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for probeA revocation close")
	}

	// Registry drops probeA while keeping probeB; probeB keeps active session.
	eventually(t, "probeA deleted from registry and revoked", 15*time.Second, func() bool {
		reg := registry(t, leader)
		_, aExists := reg[probeA.id]
		_, bExists := reg[probeB.id]
		return !aExists && bExists
	})
	if sess := leader.host.ProbeSessions(); sess[probeB.id].ConnectedVoterID == "" {
		t.Errorf("probeB session disappeared after tenant-a delete")
	}

	// Direct gRPC Subscribe attempt with probeA cert is rejected.
	cli := dialRawProbeService(t, leader.addr, probeA)
	sub, err := cli.Subscribe(ctx, &pb.SubscribeRequest{ProtocolVersion: 1, Incarnation: probeA.incarnation, Name: "probe-a"})
	if err == nil {
		_, err = sub.Recv()
	}
	if status.Code(err) != codes.PermissionDenied && err == nil {
		t.Errorf("expected revoked probeA reconnect error, got %v", err)
	}
}

// TestPrivateProbeSSRFSplit verifies ADR 0030 section 5 / ADR 0035 section 4:
// private probes check loopback targets when local SSRF is off even when the
// voter enables SSRF protection, while public probes obey the voter's SSRF flag.
func TestPrivateProbeSSRFSplit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Single voter with SSRFProtection enabled and fast ProbeAssigner.
	n1, err := launch(ctx, t, &testNode{
		id: uuid.NewString(), addr: freeAddr(t), dir: t.TempDir(),
		replInterval: 200 * time.Millisecond, ssrfProtection: true,
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "n1 leader", 15*time.Second, n1.host.Node.IsLeader)
	eventually(t, "n1 command level reaches probe level", 15*time.Second, func() bool {
		lvl, err := n1.host.Node.CommandLevel(ctx)
		return err == nil && lvl >= cluster.ProbeCommandLevel
	})
	createTestTenant(ctx, t, n1, "tenant-a", "")

	internalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer internalSrv.Close()

	priv := startTestProbe(ctx, t, n1, testProbeOptions{
		name: "priv-probe", tenantID: "tenant-a", localSSRF: false, unit: 100 * time.Millisecond,
	})
	pub := startTestProbe(ctx, t, n1, testProbeOptions{
		name: "pub-probe", tenantID: "", localSSRF: false, unit: 100 * time.Millisecond,
	})

	eventually(t, "both probes registered", 15*time.Second, func() bool {
		reg := registry(t, n1)
		return reg[priv.id].Role == store.NodeRoleProbe && reg[pub.id].Role == store.NodeRoleProbe
	})

	putTenantHTTPMonitor(ctx, t, n1, "tenant-a", "m-ssrf-priv", internalSrv.URL, store.Assignment{
		Mode: "nodes", Nodes: []string{priv.id},
	})
	putTenantHTTPMonitor(ctx, t, n1, "tenant-a", "m-ssrf-pub", internalSrv.URL, store.Assignment{
		Mode: "nodes", Nodes: []string{pub.id},
	})

	// Private probe stores UP heartbeat for loopback target.
	eventually(t, "private probe records UP for internal target", 15*time.Second, func() bool {
		hbs, err := readAllHeartbeats(ctx, n1.metrics, priv.id, priv.incarnation)
		if err != nil {
			return false
		}
		for _, hb := range hbs {
			if hb.MonitorID == "m-ssrf-priv" && hb.Status == store.CheckUp {
				return true
			}
		}
		return false
	})

	// Public probe receives ssrf_protection=true from voter and blocks loopback target.
	eventually(t, "public probe blocks internal target with DOWN", 15*time.Second, func() bool {
		hbs, err := readAllHeartbeats(ctx, n1.metrics, pub.id, pub.incarnation)
		if err != nil {
			return false
		}
		for _, hb := range hbs {
			if hb.MonitorID == "m-ssrf-pub" && hb.Status == store.CheckDown {
				return true
			}
		}
		return false
	})
}

type capturedWebhook struct {
	mu     sync.Mutex
	events []string
	bodies []map[string]any
}

func startWebhookSink(t *testing.T) (string, *capturedWebhook) {
	t.Helper()
	sink := &capturedWebhook{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		sink.mu.Lock()
		if ev, ok := m["event"].(string); ok {
			sink.events = append(sink.events, ev)
		}
		sink.bodies = append(sink.bodies, m)
		sink.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, sink
}

func (c *capturedWebhook) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *capturedWebhook) hasProbeAlert(probeID string, active bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.bodies {
		if b["event"] != store.EventProbeOffline {
			continue
		}
		p, _ := b["probe"].(map[string]any)
		if p != nil && p["id"] == probeID && p["active"] == active {
			return true
		}
	}
	return false
}

func (c *capturedWebhook) hasSystemProbeAlert(probeID string, active bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range c.bodies {
		if b["event"] != store.EventSystem {
			continue
		}
		s, _ := b["system"].(map[string]any)
		if s != nil && s["kind"] == sysalert.KindProbeOffline && s["subject"] == probeID && s["active"] == active {
			return true
		}
	}
	return false
}

// TestPrivateProbeOfflineAlerts covers FA-PRB-06 end-to-end: public probe offline
// alerts go only to _system; private probe offline alerts go only to the owning
// tenant after PROBE_OFFLINE_ALERT; short disconnects raise nothing; recovery
// clears and notifies; RevokeNode clears silently without notifications.
func TestPrivateProbeOfflineAlerts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	voters := startThreeVotersForProbes(ctx, t, 200*time.Millisecond)
	leader := voters[0]

	createTestTenant(ctx, t, leader, "tenant-a", "")
	createTestTenant(ctx, t, leader, "tenant-b", "")

	sysURL, sysHook := startWebhookSink(t)
	aURL, aHook := startWebhookSink(t)
	bURL, bHook := startWebhookSink(t)

	discard := slog.New(slog.DiscardHandler)
	sender := alert.NewSender(&checker.Dialer{}, discard)
	for _, n := range voters {
		jobs := sysalert.NewJobs(sysalert.JobOptions{
			IsLeader: n.host.Node.IsLeader, CommandLevel: n.host.Node.CommandLevel, State: n.state, Cluster: n.host.Node,
			CheckpointInterval: 5 * time.Second, Logger: discard,
		})
		col := sysalert.New(sysalert.Options{
			NodeID: n.id, IsLeader: n.host.Node.IsLeader, State: n.state, CommandLevel: n.host.Node.CommandLevel,
			Cluster: n.host.Node, Self: n.host.SelfSignals, Peers: n.host.PeerSignals, ClockOffsets: n.host.ClockOffsets,
			Thresholds: sysalert.Thresholds{ProbeOffline: 800 * time.Millisecond},
			Interval:   150 * time.Millisecond, Logger: discard,
		})
		disp := alert.NewDispatcher(alert.DispatcherOptions{
			State: n.state, Cluster: n.host.Node, Keyring: n.keyring, Sender: sender,
			IsLeader: n.host.Node.IsLeader, Logger: discard, Interval: 20 * time.Millisecond, ReminderInterval: time.Hour,
		})
		go jobs.Run(ctx)
		go col.Run(ctx)
		go disp.Run(ctx)
	}

	eventually(t, "system tenant created", 20*time.Second, func() bool {
		_, err := leader.state.GetTenant(ctx, store.SystemTenantID)
		return err == nil
	})

	putWebhookChannel := func(tenantID, id, rawURL string, events []string) {
		enc, err := alert.EncryptURL(leader.keyring, id, rawURL)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := cluster.NewPutChannel("ch-"+id, tenantID, id, cluster.ChannelDocument{
			Name: id, Kind: store.ChannelWebhook, URLEnc: enc, Events: events,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := leader.host.Node.Apply(ctx, cmd); err != nil || res.Err != nil {
			t.Fatalf("put channel %s/%s: err=%v cmdErr=%v", tenantID, id, err, res.Err)
		}
	}
	putWebhookChannel(store.SystemTenantID, "sys-ch", sysURL, []string{store.EventSystem})
	putWebhookChannel("tenant-a", "a-ch", aURL, []string{store.EventProbeOffline})
	putWebhookChannel("tenant-b", "b-ch", bURL, []string{store.EventProbeOffline})

	regProbe := func(id, name, tenantID string) {
		cmd := cluster.NewRegisterProbe("reg-"+id, store.Node{
			ID: id, Name: name, Role: store.NodeRoleProbe, OwnerTenantID: tenantID,
		})
		if _, err := leader.host.Node.Apply(ctx, cmd); err != nil {
			t.Fatal(err)
		}
		if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h0-"+id, id, true)); err != nil {
			t.Fatal(err)
		}
	}
	regProbe("pub-probe-1", "pub-1", "")
	regProbe("priv-probe-a", "edge-a", "tenant-a")

	// 1. Public probe goes offline >= 400ms -> _system channel notifies & recovery clears.
	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-off-pub", "pub-probe-1", false)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "public probe_offline webhook on _system", 15*time.Second, func() bool {
		return sysHook.hasSystemProbeAlert("pub-probe-1", true)
	})
	if aHook.count() != 0 || bHook.count() != 0 {
		t.Fatalf("tenant channels received alert for public probe: a=%d b=%d", aHook.count(), bHook.count())
	}

	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-on-pub", "pub-probe-1", true)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "public probe_offline clear webhook on _system", 15*time.Second, func() bool {
		return sysHook.hasSystemProbeAlert("pub-probe-1", false)
	})

	// 2. Short disconnect (< 800ms) raises no alert.
	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-short-off", "priv-probe-a", false)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-short-on", "priv-probe-a", true)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if aHook.count() != 0 {
		t.Fatalf("short disconnect unexpectedly notified tenant-a: %+v", aHook.bodies)
	}

	// 3. Prolonged private probe offline -> ONLY tenant-a notifies, recovery clears and notifies.
	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-off-priv", "priv-probe-a", false)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "private probe_offline webhook on tenant-a", 15*time.Second, func() bool {
		return aHook.hasProbeAlert("priv-probe-a", true)
	})
	if sysHook.hasProbeAlert("priv-probe-a", true) || sysHook.hasSystemProbeAlert("priv-probe-a", true) {
		t.Fatalf("_system unexpectedly received alert for private probe")
	}
	if bHook.count() != 0 {
		t.Fatalf("tenant-b unexpectedly received alert for tenant-a's private probe")
	}

	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-on-priv", "priv-probe-a", true)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "private probe_offline clear webhook on tenant-a", 15*time.Second, func() bool {
		return aHook.hasProbeAlert("priv-probe-a", false)
	})

	// 4. Active alert + RevokeNode -> clears silently without emitting another webhook.
	if _, err := leader.host.Node.Apply(ctx, cluster.NewSetNodeHealth("h-off-priv-2", "priv-probe-a", false)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "second probe_offline alert delivered before revoke", 15*time.Second, func() bool {
		if aHook.count() < 3 {
			return false
		}
		alerts, err := leader.state.ListProbeAlerts(ctx, "tenant-a", true)
		if err != nil {
			return false
		}
		for _, pa := range alerts {
			if pa.ProbeID == "priv-probe-a" && pa.Active {
				return true
			}
		}
		return false
	})
	beforeRevoke := aHook.count()

	revCmd := cluster.NewRevokeTenantProbe("rev-priv-a", "tenant-a", "priv-probe-a")
	revCmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Admin"}
	if res, err := leader.host.Node.Apply(ctx, revCmd); err != nil || res.Err != nil {
		t.Fatalf("revoke private probe: err=%v cmdErr=%v", err, res.Err)
	}
	eventually(t, "probe alert silently removed on RevokeNode", 15*time.Second, func() bool {
		alerts, err := leader.state.ListProbeAlerts(ctx, "tenant-a", false)
		if err != nil {
			return false
		}
		for _, pa := range alerts {
			if pa.ProbeID == "priv-probe-a" {
				return false
			}
		}
		return true
	})
	time.Sleep(300 * time.Millisecond)
	if aHook.count() != beforeRevoke {
		t.Fatalf("RevokeNode emitted unexpected notification: before=%d after=%d", beforeRevoke, aHook.count())
	}
}

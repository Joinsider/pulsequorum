// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probegw

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/configsecret"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/scheduler"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// gapIdentityKey carries the caller identity in the context so the upload and
// subscribe helpers can be exercised directly, without a running gRPC server.
type gapIdentityKey struct{}

func gapCtx(id pki.Identity) context.Context {
	return context.WithValue(context.Background(), gapIdentityKey{}, id)
}

func gapProbe(nodeID, tenantID string) pki.Identity {
	return pki.Identity{ClusterID: "cluster-1", NodeID: nodeID, Role: pki.RoleProbe, TenantID: tenantID}
}

// faultState fails the named State methods with the configured error.
type faultState struct {
	store.State
	errs map[string]error
}

func (f *faultState) ListTenants(ctx context.Context) ([]store.Tenant, error) {
	if err := f.errs["ListTenants"]; err != nil {
		return nil, err
	}
	return f.State.ListTenants(ctx)
}

func (f *faultState) GetMonitor(ctx context.Context, tenantID, id string) (store.Monitor, error) {
	if err := f.errs["GetMonitor"]; err != nil {
		return store.Monitor{}, err
	}
	return f.State.GetMonitor(ctx, tenantID, id)
}

// faultMetrics fails the named Metrics methods with the configured error.
type faultMetrics struct {
	store.Metrics
	errs map[string]error
}

func (f *faultMetrics) InsertHeartbeats(ctx context.Context, hbs []store.Heartbeat) (int, error) {
	if err := f.errs["InsertHeartbeats"]; err != nil {
		return 0, err
	}
	return f.Metrics.InsertHeartbeats(ctx, hbs)
}

func (f *faultMetrics) AdvanceStreamPosition(ctx context.Context, pos store.StreamPosition) error {
	if err := f.errs["AdvanceStreamPosition"]; err != nil {
		return err
	}
	return f.Metrics.AdvanceStreamPosition(ctx, pos)
}

func (f *faultMetrics) HeartbeatsAfter(ctx context.Context, nodeID, incarnation string, afterSeq, untilSeq uint64, limit int) ([]store.Heartbeat, bool, error) {
	if err := f.errs["HeartbeatsAfter"]; err != nil {
		return nil, false, err
	}
	return f.Metrics.HeartbeatsAfter(ctx, nodeID, incarnation, afterSeq, untilSeq, limit)
}

// gapSubStream records the responses sent on a Subscribe stream and fails
// every Send with err when set.
type gapSubStream struct {
	grpc.ServerStream
	sent []*pb.SubscribeResponse
	err  error
}

func (s *gapSubStream) Send(resp *pb.SubscribeResponse) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, resp)
	return nil
}

type gapEnv struct {
	t            *testing.T
	state        *sqlite.StateStore
	metrics      *sqlite.MetricsStore
	stateFault   *faultState
	metricsFault *faultMetrics
	srv          *Server
	now          time.Time
	rejected     []string
}

func newGapEnv(t *testing.T, keyring configsecret.Keyring) *gapEnv {
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

	env := &gapEnv{
		t:            t,
		state:        state,
		metrics:      metrics,
		stateFault:   &faultState{State: state},
		metricsFault: &faultMetrics{Metrics: metrics},
		now:          time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
	}
	fc := &fakeCluster{
		level:   cluster.ProbeCommandLevel,
		members: []cluster.Member{{ID: "voter-1", Address: "127.0.0.1:9001", Voter: true}},
	}
	env.srv = New(Options{
		NodeID:     "voter-1",
		State:      env.stateFault,
		Metrics:    env.metricsFault,
		Cluster:    fc,
		Assigner:   scheduler.Assigner{},
		Keyring:    keyring,
		Clock:      func() time.Time { return env.now },
		OnRejected: func(reason string) { env.rejected = append(env.rejected, reason) },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity: func(ctx context.Context) (pki.Identity, bool) {
			id, ok := ctx.Value(gapIdentityKey{}).(pki.Identity)
			return id, ok
		},
	})
	return env
}

func (e *gapEnv) seedTenant(id string, suspended bool) {
	e.t.Helper()
	err := e.state.Update(context.Background(), func(tx store.StateTx) error {
		return tx.PutTenant(context.Background(), store.Tenant{ID: id, Name: id, Suspended: suspended})
	})
	if err != nil {
		e.t.Fatalf("PutTenant(%s): %v", id, err)
	}
}

func (e *gapEnv) seedProbe(id, ownerTenantID string) {
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

func (e *gapEnv) seedMonitor(m store.Monitor) {
	e.t.Helper()
	if m.Type == "" {
		m.Type = "http"
	}
	if len(m.Config) == 0 {
		m.Config = []byte(`{"url":"https://example.com"}`)
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

// seededGapEnv returns an env with tenant t1, probe p1 (public) and one
// monitor m1 in t1.
func seededGapEnv(t *testing.T) *gapEnv {
	t.Helper()
	env := newGapEnv(t, nil)
	env.seedTenant("t1", false)
	env.seedProbe("p1", "")
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1"})
	return env
}

func (e *gapEnv) hb(seq uint64, monitorID string) *pb.Heartbeat {
	return &pb.Heartbeat{
		NodeId:      "p1",
		Incarnation: "inc-1",
		Seq:         seq,
		TenantId:    "t1",
		MonitorId:   monitorID,
		TsMs:        e.now.UnixMilli(),
		Status:      pb.CheckStatus_CHECK_STATUS_UP,
	}
}

func (e *gapEnv) upload(hbs ...*pb.Heartbeat) (*pb.UploadHeartbeatsResponse, error) {
	return e.srv.processBatch(context.Background(), gapProbe("p1", ""), &pb.UploadHeartbeatsRequest{Heartbeats: hbs})
}

func (e *gapEnv) storedSeq(t *testing.T) uint64 {
	t.Helper()
	seq, err := e.metrics.LastSeq(context.Background(), "p1", "inc-1")
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	return seq
}

func TestUploadBatchValidation(t *testing.T) {
	env := seededGapEnv(t)
	nowMs := env.now.UnixMilli()
	tooOld := nowMs - (defaultBufferMaxAge + pastWindowGrace).Milliseconds() - 1
	tooNew := nowMs + maxFutureSkew.Milliseconds() + 1

	tests := []struct {
		name   string
		mutate func(*pb.Heartbeat)
	}{
		{"missing incarnation", func(h *pb.Heartbeat) { h.Incarnation = "" }},
		{"missing monitor id", func(h *pb.Heartbeat) { h.MonitorId = "" }},
		{"zero seq", func(h *pb.Heartbeat) { h.Seq = 0 }},
		{"timestamp older than buffer window", func(h *pb.Heartbeat) { h.TsMs = tooOld }},
		{"timestamp too far in the future", func(h *pb.Heartbeat) { h.TsMs = tooNew }},
		{"message too large", func(h *pb.Heartbeat) { h.Message = strings.Repeat("m", maxMessageBytes+1) }},
		{"details too large", func(h *pb.Heartbeat) {
			h.DetailsJson = []byte(`"` + strings.Repeat("a", maxDetailsBytes) + `"`)
		}},
		{"details not valid JSON", func(h *pb.Heartbeat) { h.DetailsJson = []byte("{bad") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := env.hb(1, "m1")
			tc.mutate(h)
			_, err := env.upload(h)
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", got, err)
			}
			if seq := env.storedSeq(t); seq != 0 {
				t.Fatalf("stored seq = %d, want 0 after rejected batch", seq)
			}
		})
	}
}

func TestUploadIdentityRejections(t *testing.T) {
	env := seededGapEnv(t)
	tests := []struct {
		name     string
		caller   pki.Identity
		hbNode   string
		hbTenant string
	}{
		{"empty tenant", gapProbe("p1", ""), "p1", ""},
		{"system tenant", gapProbe("p1", ""), "p1", store.SystemTenantID},
		{"node id mismatch", gapProbe("p1", ""), "p2", "t1"},
		{"private probe cross-tenant", gapProbe("p1", "t1"), "p1", "t2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := env.hb(1, "m1")
			h.NodeId = tc.hbNode
			h.TenantId = tc.hbTenant
			before := len(env.rejected)
			_, err := env.srv.processBatch(context.Background(), tc.caller, &pb.UploadHeartbeatsRequest{Heartbeats: []*pb.Heartbeat{h}})
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, err)
			}
			if len(env.rejected) != before+1 || env.rejected[len(env.rejected)-1] != "upload_identity" {
				t.Fatalf("rejected = %v, want upload_identity appended", env.rejected)
			}
			if seq := env.storedSeq(t); seq != 0 {
				t.Fatalf("stored seq = %d, want 0", seq)
			}
		})
	}
}

func TestUploadMonitorLookupUnavailable(t *testing.T) {
	env := seededGapEnv(t)
	env.stateFault.errs = map[string]error{"GetMonitor": errors.New("db offline")}

	_, err := env.upload(env.hb(1, "m1"))
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable (err=%v)", got, err)
	}
	if seq := env.storedSeq(t); seq != 0 {
		t.Fatalf("stored seq = %d, want 0", seq)
	}
}

func TestUploadStoreFailures(t *testing.T) {
	tests := []struct {
		name    string
		faults  map[string]error
		lost    uint64
		monitor string
	}{
		{"advance lost position fails", map[string]error{"AdvanceStreamPosition": errors.New("disk full")}, 5, "m1"},
		{"insert heartbeats fails", map[string]error{"InsertHeartbeats": errors.New("disk full")}, 0, "m1"},
		{"advance discarded position fails", map[string]error{"AdvanceStreamPosition": errors.New("disk full")}, 0, "m-gone"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := seededGapEnv(t)
			env.metricsFault.errs = tc.faults
			req := &pb.UploadHeartbeatsRequest{
				Heartbeats: []*pb.Heartbeat{env.hb(1, tc.monitor)},
				LostBelow:  tc.lost,
			}
			_, err := env.srv.processBatch(context.Background(), gapProbe("p1", ""), req)
			if got := status.Code(err); got != codes.Unavailable {
				t.Fatalf("code = %v, want Unavailable (err=%v)", got, err)
			}
		})
	}
}

func TestUploadStoresValidHeartbeats(t *testing.T) {
	env := seededGapEnv(t)
	var stored, published int
	env.srv.opts.OnStored = func(hbs []store.Heartbeat) { stored += len(hbs) }
	env.srv.opts.Publish = func(hbs []store.Heartbeat) { published += len(hbs) }

	resp, err := env.upload(env.hb(1, "m1"), env.hb(2, "m1"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := resp.GetPersisted().GetSeq(); got != 2 {
		t.Fatalf("persisted seq = %d, want 2", got)
	}
	if stored != 2 || published != 2 {
		t.Fatalf("OnStored=%d Publish=%d, want 2 and 2", stored, published)
	}
}

// TestUploadDiscardedPositionWalk covers how dropped heartbeats (monitor deleted)
// move the stream position, with and without stored heartbeats around the gap.
func TestUploadDiscardedPositionWalk(t *testing.T) {
	stored := func(seqs ...uint64) func(*gapEnv) {
		return func(e *gapEnv) {
			var hbs []store.Heartbeat
			for _, seq := range seqs {
				hbs = append(hbs, store.Heartbeat{
					NodeID:      "p1",
					Incarnation: "inc-1",
					Seq:         seq,
					TenantID:    "t1",
					MonitorID:   "m1",
					TsMs:        e.now.UnixMilli(),
					Details:     []byte("{}"),
				})
			}
			if _, err := e.metrics.InsertHeartbeats(context.Background(), hbs); err != nil {
				e.t.Fatalf("InsertHeartbeats: %v", err)
			}
		}
	}
	tests := []struct {
		name      string
		setup     func(*gapEnv)
		discarded []uint64
		want      uint64
	}{
		{
			name: "discarded seq at or below stream position is ignored",
			setup: func(e *gapEnv) {
				if err := e.metrics.AdvanceStreamPosition(context.Background(), store.StreamPosition{NodeID: "p1", Incarnation: "inc-1", Seq: 5}); err != nil {
					e.t.Fatalf("AdvanceStreamPosition: %v", err)
				}
			},
			discarded: []uint64{3},
			want:      5,
		},
		{
			name:      "gap without stored heartbeats holds the position",
			discarded: []uint64{3},
			want:      0,
		},
		{
			name:      "gap closed by a stored heartbeat",
			setup:     stored(2),
			discarded: []uint64{1, 3},
			want:      3,
		},
		{
			name:      "unfilled gap before stored heartbeat stops the walk",
			setup:     stored(3),
			discarded: []uint64{1},
			want:      1,
		},
		{
			name:      "contiguous stored heartbeats after discarded seq are skipped",
			setup:     stored(2, 3),
			discarded: []uint64{1},
			want:      3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := seededGapEnv(t)
			if tc.setup != nil {
				tc.setup(env)
			}
			hbs := make([]*pb.Heartbeat, 0, len(tc.discarded))
			for _, seq := range tc.discarded {
				hbs = append(hbs, env.hb(seq, "m-gone"))
			}
			resp, err := env.upload(hbs...)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			if got := resp.GetPersisted().GetSeq(); got != tc.want {
				t.Fatalf("persisted seq = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestUploadDiscardedWalkLookupFails(t *testing.T) {
	tests := []struct {
		name      string
		discarded uint64
	}{
		{"gap lookup fails", 3},
		{"trailing lookup fails", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := seededGapEnv(t)
			env.metricsFault.errs = map[string]error{"HeartbeatsAfter": errors.New("read failed")}
			_, err := env.upload(env.hb(tc.discarded, "m-gone"))
			if got := status.Code(err); got != codes.Unavailable {
				t.Fatalf("code = %v, want Unavailable (err=%v)", got, err)
			}
		})
	}
}

func TestResetDebounceTimer(t *testing.T) {
	const d = time.Millisecond
	awaitFire := func(t *testing.T, ch <-chan time.Time) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("debounce timer did not fire")
		}
	}

	t.Run("creates a timer when none exists", func(t *testing.T) {
		tm, ch := resetDebounceTimer(nil, d)
		if tm == nil || ch == nil {
			t.Fatal("expected a new timer and channel")
		}
		awaitFire(t, ch)
	})

	t.Run("resets a pending timer", func(t *testing.T) {
		pending := time.NewTimer(time.Hour)
		tm, ch := resetDebounceTimer(pending, d)
		if tm != pending {
			t.Fatal("expected the pending timer to be reused")
		}
		awaitFire(t, ch)
	})

	t.Run("resets a timer whose value was already received", func(t *testing.T) {
		old := time.NewTimer(0)
		<-old.C
		_, ch := resetDebounceTimer(old, d)
		awaitFire(t, ch)
	})
}

func TestHandleRecomputeGuards(t *testing.T) {
	caller := gapProbe("p1", "")
	prevHashes := map[string]string{"stale": "h"}

	t.Run("unauthenticated caller is rejected", func(t *testing.T) {
		env := seededGapEnv(t)
		stream := &gapSubStream{}
		hashes, voters, err := env.srv.handleRecompute(context.Background(), caller, prevHashes, "v", stream)
		if got := status.Code(err); got != codes.Unauthenticated {
			t.Fatalf("code = %v, want Unauthenticated (err=%v)", got, err)
		}
		if len(hashes) != 1 || hashes["stale"] != "h" || voters != "v" || len(stream.sent) != 0 {
			t.Fatalf("state changed: hashes=%v voters=%q sent=%d", hashes, voters, len(stream.sent))
		}
	})

	t.Run("snapshot failure keeps previous state silently", func(t *testing.T) {
		env := seededGapEnv(t)
		env.stateFault.errs = map[string]error{"ListTenants": errors.New("db offline")}
		stream := &gapSubStream{}
		hashes, voters, err := env.srv.handleRecompute(gapCtx(caller), caller, prevHashes, "v", stream)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if hashes["stale"] != "h" || voters != "v" || len(stream.sent) != 0 {
			t.Fatalf("state changed: hashes=%v voters=%q sent=%d", hashes, voters, len(stream.sent))
		}
	})

	t.Run("unchanged state sends nothing", func(t *testing.T) {
		env := seededGapEnv(t)
		ctx := gapCtx(caller)
		snap, _, err := env.srv.buildSnapshot(ctx, caller)
		if err != nil {
			t.Fatalf("buildSnapshot: %v", err)
		}
		current := make(map[string]string)
		for id, rm := range snap.monitors {
			current[id] = rm.hash
		}
		voters := voterSignature(env.srv.currentVoters())
		stream := &gapSubStream{}
		hashes, _, err := env.srv.handleRecompute(ctx, caller, current, voters, stream)
		if err != nil || len(stream.sent) != 0 {
			t.Fatalf("err=%v sent=%d, want nil and 0", err, len(stream.sent))
		}
		if len(hashes) != len(current) {
			t.Fatalf("hashes = %v, want %v", hashes, current)
		}
	})

	t.Run("added and removed monitors are sent as one delta", func(t *testing.T) {
		env := seededGapEnv(t)
		ctx := gapCtx(caller)
		voters := voterSignature(env.srv.currentVoters())
		stream := &gapSubStream{}
		hashes, _, err := env.srv.handleRecompute(ctx, caller, map[string]string{"gone": "h"}, voters, stream)
		if err != nil {
			t.Fatalf("handleRecompute: %v", err)
		}
		if len(stream.sent) != 1 {
			t.Fatalf("sent %d responses, want 1", len(stream.sent))
		}
		resp := stream.sent[0]
		if len(resp.GetRemovedMonitorIds()) != 1 || resp.GetRemovedMonitorIds()[0] != "gone" {
			t.Fatalf("removed = %v, want [gone]", resp.GetRemovedMonitorIds())
		}
		if len(resp.GetUpserts()) != 1 || resp.GetUpserts()[0].GetId() != "m1" {
			t.Fatalf("upserts = %v, want [m1]", resp.GetUpserts())
		}
		if _, ok := hashes["m1"]; !ok || len(hashes) != 1 {
			t.Fatalf("next hashes = %v, want only m1", hashes)
		}
	})

	t.Run("voter change alone is delivered", func(t *testing.T) {
		env := seededGapEnv(t)
		ctx := gapCtx(caller)
		snap, _, err := env.srv.buildSnapshot(ctx, caller)
		if err != nil {
			t.Fatalf("buildSnapshot: %v", err)
		}
		current := make(map[string]string)
		for id, rm := range snap.monitors {
			current[id] = rm.hash
		}
		stream := &gapSubStream{}
		_, voters, err := env.srv.handleRecompute(ctx, caller, current, "", stream)
		if err != nil {
			t.Fatalf("handleRecompute: %v", err)
		}
		if voters == "" || len(stream.sent) != 1 {
			t.Fatalf("voters=%q sent=%d, want signature and 1 response", voters, len(stream.sent))
		}
		if got := len(stream.sent[0].GetVoters()); got != 1 || len(stream.sent[0].GetUpserts()) != 0 {
			t.Fatalf("voters=%d upserts=%d, want 1 voter and no upserts", got, len(stream.sent[0].GetUpserts()))
		}
	})

	t.Run("send failure is returned", func(t *testing.T) {
		env := seededGapEnv(t)
		ctx := gapCtx(caller)
		stream := &gapSubStream{err: errors.New("broken pipe")}
		hashes, _, err := env.srv.handleRecompute(ctx, caller, map[string]string{}, "", stream)
		if err == nil || !errors.Is(err, stream.err) {
			t.Fatalf("err = %v, want broken pipe", err)
		}
		if len(hashes) != 0 {
			t.Fatalf("hashes = %v, want previous (empty) state", hashes)
		}
	})
}

func TestHandleSubscribeTick(t *testing.T) {
	caller := gapProbe("p1", "")
	t.Run("changed voters are included in the tick", func(t *testing.T) {
		env := seededGapEnv(t)
		stream := &gapSubStream{}
		voters, err := env.srv.handleSubscribeTick(gapCtx(caller), "p1", "", stream)
		if err != nil {
			t.Fatalf("handleSubscribeTick: %v", err)
		}
		if voters == "" || len(stream.sent) != 1 || len(stream.sent[0].GetVoters()) != 1 {
			t.Fatalf("voters=%q sent=%+v, want signature and one voter", voters, stream.sent)
		}
	})

	t.Run("unchanged voters are omitted from the tick", func(t *testing.T) {
		env := seededGapEnv(t)
		sig := voterSignature(env.srv.currentVoters())
		stream := &gapSubStream{}
		voters, err := env.srv.handleSubscribeTick(gapCtx(caller), "p1", sig, stream)
		if err != nil || voters != sig {
			t.Fatalf("voters=%q err=%v, want %q and nil", voters, err, sig)
		}
		if len(stream.sent) != 1 || len(stream.sent[0].GetVoters()) != 0 {
			t.Fatalf("tick carried voters: %+v", stream.sent)
		}
	})

	t.Run("send failure is returned", func(t *testing.T) {
		env := seededGapEnv(t)
		stream := &gapSubStream{err: errors.New("broken pipe")}
		_, err := env.srv.handleSubscribeTick(gapCtx(caller), "p1", "", stream)
		if !errors.Is(err, stream.err) {
			t.Fatalf("err = %v, want broken pipe", err)
		}
	})
}

func TestAddAssignedMonitorFilters(t *testing.T) {
	registry := []store.Node{
		{ID: "p1", Role: store.NodeRoleProbe},
		{ID: "p2", Role: store.NodeRoleProbe},
	}
	base := store.Monitor{TenantID: "t1", ID: "m", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10, Config: []byte(`{"url":"https://example.com"}`)}
	tests := []struct {
		name     string
		caller   pki.Identity
		registry []store.Node
		mutate   func(*store.Monitor)
		want     bool
		reason   string
	}{
		{name: "eligible monitor is added", caller: gapProbe("p1", ""), want: true},
		{name: "paused monitor is skipped", caller: gapProbe("p1", ""), mutate: func(m *store.Monitor) { m.Paused = true }},
		{name: "zero interval is skipped", caller: gapProbe("p1", ""), mutate: func(m *store.Monitor) { m.IntervalSeconds = 0 }},
		{name: "negative interval is skipped", caller: gapProbe("p1", ""), mutate: func(m *store.Monitor) { m.IntervalSeconds = -5 }},
		{
			name:     "monitor assigned to another probe is skipped",
			caller:   gapProbe("p1", ""),
			registry: registry,
			mutate:   func(m *store.Monitor) { m.Assignment = store.Assignment{Mode: "nodes", Nodes: []string{"p2"}} },
		},
		{
			name:   "foreign tenant monitor is dropped for private probe",
			caller: gapProbe("p-priv", "t1"),
			mutate: func(m *store.Monitor) { m.TenantID = "t2" },
			reason: "foreign_monitor",
		},
		{
			name:   "secret resolution failure drops the monitor",
			caller: gapProbe("p1", ""),
			mutate: func(m *store.Monitor) {
				m.Config = []byte(`{"url":"https://example.com","password":"masked"}`)
				m.Secrets = map[string]string{"/password": "not-a-ciphertext"}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newGapEnv(t, fakeKeyring{})
			m := base
			if tc.mutate != nil {
				tc.mutate(&m)
			}
			out := snapshot{monitors: make(map[string]resolvedMonitor)}
			env.srv.addAssignedMonitor(context.Background(), tc.caller, m, tc.registry, &out)

			rm, ok := out.monitors[m.ID]
			if ok != tc.want {
				t.Fatalf("monitor present = %v, want %v", ok, tc.want)
			}
			if tc.want {
				if rm.rate != 1.0/60 || out.totalRate != rm.rate {
					t.Fatalf("rate = %v total = %v, want 1/60", rm.rate, out.totalRate)
				}
				if rm.proto.GetTenantId() != "t1" || rm.proto.GetIntervalSeconds() != 60 {
					t.Fatalf("proto = %+v", rm.proto)
				}
			} else if out.totalRate != 0 {
				t.Fatalf("totalRate = %v, want 0", out.totalRate)
			}
			if tc.reason != "" && (len(env.rejected) != 1 || env.rejected[0] != tc.reason) {
				t.Fatalf("rejected = %v, want [%s]", env.rejected, tc.reason)
			}
		})
	}
}

// TestBuildSnapshotScope checks which monitors end up in the snapshot for a
// public probe and for a private probe, with suspended tenants excluded.
func TestBuildSnapshotScope(t *testing.T) {
	env := newGapEnv(t, nil)
	env.seedTenant("t1", false)
	env.seedTenant("t2", false)
	env.seedTenant("t3", true)
	env.seedMonitor(store.Monitor{TenantID: "t1", ID: "m1", Name: "m1"})
	env.seedMonitor(store.Monitor{TenantID: "t2", ID: "m2", Name: "m2"})
	env.seedMonitor(store.Monitor{TenantID: "t3", ID: "m3", Name: "m3"})

	tests := []struct {
		name   string
		caller pki.Identity
		want   map[string]bool
	}{
		{"public probe sees active tenants only", gapProbe("p1", ""), map[string]bool{"m1": true, "m2": true}},
		{"private probe sees its own tenant only", gapProbe("p-priv", "t1"), map[string]bool{"m1": true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap, _, err := env.srv.buildSnapshot(context.Background(), tc.caller)
			if err != nil {
				t.Fatalf("buildSnapshot: %v", err)
			}
			if len(snap.monitors) != len(tc.want) {
				t.Fatalf("monitors = %v, want %v", snap.monitors, tc.want)
			}
			for id := range tc.want {
				if _, ok := snap.monitors[id]; !ok {
					t.Fatalf("monitor %s missing from snapshot", id)
				}
			}
		})
	}
}

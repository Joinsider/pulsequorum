// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sysalert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

const testInterval = 30 * time.Second

// fakeState is the replicated state the collector reads. The fake applier
// writes the commands into it, as the FSM would.
type fakeState struct {
	nodes   []store.Node
	alerts  map[key]store.SystemAlert
	incs    map[string]string
	cas     []store.ClusterCA
	tenants map[string]bool
}

func (s *fakeState) ListNodes(context.Context) ([]store.Node, error) {
	return append([]store.Node(nil), s.nodes...), nil
}

func (s *fakeState) ListSystemAlerts(_ context.Context, activeOnly bool) ([]store.SystemAlert, error) {
	var out []store.SystemAlert
	for _, a := range s.alerts {
		if activeOnly && !a.Active {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *fakeState) ListNodeIncarnations(context.Context) ([]store.NodeIncarnation, error) {
	var out []store.NodeIncarnation
	for id, inc := range s.incs {
		out = append(out, store.NodeIncarnation{NodeID: id, Incarnation: inc})
	}
	return out, nil
}

func (s *fakeState) ListClusterCAs(context.Context) ([]store.ClusterCA, error) {
	return s.cas, nil
}

func (s *fakeState) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	if s.tenants[id] {
		return store.Tenant{ID: id}, nil
	}
	return store.Tenant{}, store.ErrNotFound
}

// fakeApplier records the commands and applies them to the fake state.
type fakeApplier struct {
	state *fakeState
	cmds  []*pb.Command
}

func (a *fakeApplier) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	a.cmds = append(a.cmds, cmd)
	switch p := cmd.GetPayload().(type) {
	case *pb.Command_SetSystemAlert:
		s := p.SetSystemAlert
		a.state.alerts[key{s.GetKind(), s.GetSubject()}] = store.SystemAlert{
			Kind: s.GetKind(), Subject: s.GetSubject(), Active: s.GetActive(), Level: int(s.GetLevel()),
		}
	case *pb.Command_RecordIncarnation:
		a.state.incs[p.RecordIncarnation.GetNodeId()] = p.RecordIncarnation.GetIncarnation()
	case *pb.Command_PutEntity:
		if p.PutEntity.GetKind() == pb.EntityKind_ENTITY_KIND_TENANT {
			a.state.tenants[p.PutEntity.GetId()] = true
		}
	}
	return cluster.ApplyResult{}, nil
}

// countKind counts the SetSystemAlert commands of a kind.
func (a *fakeApplier) countKind(kind string) int {
	n := 0
	for _, cmd := range a.cmds {
		if s := cmd.GetSetSystemAlert(); s != nil && s.GetKind() == kind {
			n++
		}
	}
	return n
}

// world drives one collector with a fake clock and a fake cluster.
type world struct {
	t       *testing.T
	now     time.Time
	state   *fakeState
	applier *fakeApplier
	self    Signals
	peers   map[string]Signals
	silent  map[string]bool
	offsets map[string]int64
	leader  bool
	level   uint32
	logs    *bytes.Buffer
	col     *Collector
}

// newWorld creates a cluster of three voters: n1 is this node, n2 and n3 are
// peers. All of them report the ADR 0025 fields with healthy values.
func newWorld(t *testing.T) *world {
	t.Helper()
	st := &fakeState{
		nodes: []store.Node{
			{ID: "n1", Role: store.NodeRoleVoter, Healthy: true, MaxCommandVersion: 6},
			{ID: "n2", Role: store.NodeRoleVoter, Healthy: true, MaxCommandVersion: 6},
			{ID: "n3", Role: store.NodeRoleVoter, Healthy: true, MaxCommandVersion: 6},
			{ID: "p1", Role: store.NodeRoleProbe, Healthy: false},
		},
		alerts:  map[key]store.SystemAlert{},
		incs:    map[string]string{},
		tenants: map[string]bool{},
	}
	w := &world{
		t: t, now: time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC), state: st,
		applier: &fakeApplier{state: st}, level: cluster.SystemAlertCommandLevel,
		peers: map[string]Signals{}, silent: map[string]bool{}, offsets: map[string]int64{},
		logs: &bytes.Buffer{},
	}
	w.self = healthySignals("n1")
	w.peers["n2"] = healthySignals("n2")
	w.peers["n3"] = healthySignals("n3")
	w.col = w.newCollector()
	return w
}

// healthySignals are the signals of a voter without any alert condition.
func healthySignals(id string) Signals {
	return Signals{
		NodeID: id, RaftState: "Follower", LeaderID: "n1",
		AppliedIndex: 5000, CommitIndex: 5000, LastContactMs: 100,
		Incarnation: "inc-" + id, DiskFreeRatio: 0.5, StateWALBytes: 1 << 20, MetricsWALBytes: 1 << 20,
		CertNotAfter: time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), ReplicationLagMs: 0,
		FingerprintIndex: 0, Fingerprint: "",
	}
}

// newCollector creates a collector on the world, as a fresh leader would.
func (w *world) newCollector() *Collector {
	logger := slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return New(Options{
		NodeID:       "n1",
		IsLeader:     func() bool { return w.leader },
		State:        w.state,
		CommandLevel: func(context.Context) (uint32, error) { return w.level, nil },
		Cluster:      w.applier,
		Self:         func() Signals { return w.self },
		Peers: func() map[string]Signals {
			out := map[string]Signals{}
			for id, s := range w.peers {
				if w.silent[id] {
					continue
				}
				s.AnsweredAt = w.now
				out[id] = s
			}
			return out
		},
		ClockOffsets: func() map[string]int64 { return w.offsets },
		Interval:     testInterval,
		Now:          func() time.Time { return w.now },
		Logger:       logger,
	})
}

// lead makes this node the leader; the first tick only starts the leadership.
func (w *world) lead() {
	w.leader = true
	w.col.Tick(context.Background())
}

// steps runs n evaluations, one interval apart.
func (w *world) steps(n int) {
	for range n {
		w.now = w.now.Add(testInterval)
		w.col.Tick(context.Background())
	}
}

// alert returns the stored state of an alert.
func (w *world) alert(kind, subject string) (active bool, level int) {
	a, ok := w.state.alerts[key{kind, subject}]
	return ok && a.Active, a.Level
}

// expectAlert checks the stored state of an alert.
func (w *world) expectAlert(kind, subject string, active bool, level int) {
	w.t.Helper()
	gotActive, gotLevel := w.alert(kind, subject)
	if gotActive != active || (active && gotLevel != level) {
		w.t.Fatalf("%s/%s: active=%v level=%d, want active=%v level=%d", kind, subject, gotActive, gotLevel, active, level)
	}
}

// TestNoEvaluationBeforeFullRound: the first evaluation of a leadership waits
// for one full interval, so that the first answers of all peers are in.
func TestNoEvaluationBeforeFullRound(t *testing.T) {
	w := newWorld(t)
	w.state.nodes[1].Healthy = false // n2 is offline
	w.lead()
	w.now = w.now.Add(testInterval / 2)
	w.col.Tick(context.Background())
	if len(w.applier.cmds) != 0 {
		t.Fatalf("evaluated before a full round: %d commands", len(w.applier.cmds))
	}
	w.now = w.now.Add(testInterval / 2)
	w.col.Tick(context.Background())
	w.expectAlert(KindVoterOffline, "n2", true, 0)
}

// TestHysteresisPerKind checks each kind: the condition must hold for the
// confirm count of evaluations before the alert is raised, and it must be
// gone for as long before it clears (ADR 0025, section 3).
func TestHysteresisPerKind(t *testing.T) {
	setNode := func(f func(*store.Node)) func(*world) {
		return func(w *world) { f(&w.state.nodes[1]) }
	}
	setPeer := func(f func(*Signals)) func(*world) {
		return func(w *world) { p := w.peers["n2"]; f(&p); w.peers["n2"] = p }
	}
	tests := []struct {
		name      string
		kind      string
		confirm   int
		level     int
		on, off   func(*world)
		setupOnly bool
	}{
		{
			name: "voter offline is immediate", kind: KindVoterOffline, confirm: 1,
			on:  setNode(func(n *store.Node) { n.Healthy = false }),
			off: setNode(func(n *store.Node) { n.Healthy = true }),
		},
		{
			name: "node impaired is immediate", kind: KindNodeImpaired, confirm: 1,
			on:  setNode(func(n *store.Node) { n.ConnectivityImpaired = true }),
			off: setNode(func(n *store.Node) { n.ConnectivityImpaired = false }),
		},
		{
			name: "replication lag at 5 min", kind: KindReplicationLag, confirm: 2,
			on:  setPeer(func(s *Signals) { s.ReplicationLagMs = 6 * 60 * 1000 }),
			off: setPeer(func(s *Signals) { s.ReplicationLagMs = 0 }),
		},
		{
			name: "replication lag never caught up for 5 min", kind: KindReplicationLag, confirm: 12,
			on:  setPeer(func(s *Signals) { s.ReplicationLagMs = -1 }),
			off: setPeer(func(s *Signals) { s.ReplicationLagMs = 0 }),
		},
		{
			name: "clock skew above 2 s", kind: KindClockSkew, confirm: 2,
			on:  func(w *world) { w.offsets["n2"] = 3000 },
			off: func(w *world) { w.offsets["n2"] = 0 },
		},
		{
			name: "disk below 15 percent", kind: KindDiskLow, confirm: 2,
			on:  setPeer(func(s *Signals) { s.DiskFreeRatio = 0.10 }),
			off: setPeer(func(s *Signals) { s.DiskFreeRatio = 0.50 }),
		},
		{
			name: "wal above 512 MiB", kind: KindWALLarge, confirm: 2,
			on:  setPeer(func(s *Signals) { s.MetricsWALBytes = 600 << 20 }),
			off: setPeer(func(s *Signals) { s.MetricsWALBytes = 1 << 20 }),
		},
		{
			name: "raft stale by applied gap", kind: KindRaftStale, confirm: 2,
			on:  setPeer(func(s *Signals) { s.AppliedIndex = 1000 }),
			off: setPeer(func(s *Signals) { s.AppliedIndex = 5000 }),
		},
		{
			name: "raft stale by last contact", kind: KindRaftStale, confirm: 2,
			on:  setPeer(func(s *Signals) { s.LastContactMs = 11000 }),
			off: setPeer(func(s *Signals) { s.LastContactMs = 100 }),
		},
		{
			name: "node certificate in 10 days is stage 30", kind: KindCertExpiry, confirm: 2, level: 30,
			on: func(w *world) {
				p := w.peers["n2"]
				p.CertNotAfter = w.now.Add(10 * 24 * time.Hour)
				w.peers["n2"] = p
			},
			off: func(w *world) {
				p := w.peers["n2"]
				p.CertNotAfter = w.now.Add(400 * 24 * time.Hour)
				w.peers["n2"] = p
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.lead()
			w.steps(1)
			w.expectAlert(tt.kind, "n2", false, 0)
			if strings.HasPrefix(tt.name, "replication lag never") {
				// The never-caught-up clock starts at the first evaluation that sees it.
				tt.on(w)
				w.steps(tt.confirm)
				w.expectAlert(tt.kind, "n2", true, 0)
				return
			}
			tt.on(w)
			w.steps(tt.confirm - 1)
			w.expectAlert(tt.kind, "n2", false, 0)
			w.steps(1)
			w.expectAlert(tt.kind, "n2", true, tt.level)
			tt.off(w)
			w.steps(tt.confirm - 1)
			w.expectAlert(tt.kind, "n2", true, tt.level)
			w.steps(1)
			w.expectAlert(tt.kind, "n2", false, 0)
		})
	}
}

// TestSingleFlipDoesNotAlert: a condition that holds for one evaluation only
// changes nothing.
func TestSingleFlipDoesNotAlert(t *testing.T) {
	w := newWorld(t)
	w.lead()
	w.steps(1)
	p := w.peers["n2"]
	p.DiskFreeRatio = 0.01
	w.peers["n2"] = p
	w.steps(1)
	p.DiskFreeRatio = 0.5
	w.peers["n2"] = p
	w.steps(3)
	if n := w.applier.countKind(KindDiskLow); n != 0 {
		t.Fatalf("disk_low commands = %d, want 0 for a single evaluation", n)
	}
}

// TestUnreachableNodeOnlyVoterOffline: a node that does not answer gets no
// signal alerts, only voter_offline from the register (ADR 0025, section 3).
func TestUnreachableNodeOnlyVoterOffline(t *testing.T) {
	w := newWorld(t)
	w.state.nodes[1].Healthy = false
	w.state.nodes[1].ConnectivityImpaired = true
	w.silent["n2"] = true
	w.lead()
	w.steps(3)
	w.expectAlert(KindVoterOffline, "n2", true, 0)
	for _, a := range w.state.alerts {
		if a.Subject == "n2" && a.Kind != KindVoterOffline {
			t.Errorf("unreachable node got %s", a.Kind)
		}
	}
}

// TestNoDuplicateAfterLeaderChange: the state of the alerts is replicated, so
// a new leader that finds the alert active does not propose it again.
func TestNoDuplicateAfterLeaderChange(t *testing.T) {
	w := newWorld(t)
	w.state.nodes[1].Healthy = false
	w.lead()
	w.steps(1)
	w.expectAlert(KindVoterOffline, "n2", true, 0)
	before := len(w.applier.cmds)

	w.col = w.newCollector()
	w.leader = false
	w.col.Tick(context.Background())
	w.lead()
	w.steps(2)
	if got := len(w.applier.cmds); got != before {
		t.Fatalf("new leader proposed %d commands, want none", got-before)
	}
}

// TestBelowLevelOnlyLogs: below command level 6 nothing is written, the
// transitions are logged.
func TestBelowLevelOnlyLogs(t *testing.T) {
	w := newWorld(t)
	w.level = cluster.SystemAlertCommandLevel - 1
	w.state.nodes[1].Healthy = false
	w.lead()
	w.steps(2)
	if len(w.applier.cmds) != 0 {
		t.Fatalf("commands below level 6: %d", len(w.applier.cmds))
	}
	if !strings.Contains(w.logs.String(), "kind=voter_offline") {
		t.Errorf("transition not logged: %s", w.logs.String())
	}
}

// TestCertExpiryStages: the CA is warned once per stage 30, 7 and 1 days
// before its expiry, and the warning clears after the renewal.
func TestCertExpiryStages(t *testing.T) {
	w := newWorld(t)
	setCA := func(notAfter time.Time) {
		w.state.cas = []store.ClusterCA{{CertDER: caCert(t, notAfter), CreatedMs: 1}}
	}
	setCA(w.now.Add(60 * 24 * time.Hour))
	w.lead()
	w.steps(2)
	w.expectAlert(KindCertExpiry, SubjectCA, false, 0)

	stages := []struct {
		after time.Duration
		level int
	}{{25 * 24 * time.Hour, 30}, {6 * 24 * time.Hour, 7}, {12 * time.Hour, 1}}
	for _, s := range stages {
		setCA(w.now.Add(s.after))
		w.steps(2)
		w.expectAlert(KindCertExpiry, SubjectCA, true, s.level)
		w.steps(3) // no repeats within a stage
	}
	if n := w.applier.countKind(KindCertExpiry); n != 3 {
		t.Errorf("cert_expiry commands = %d, want one per stage (3)", n)
	}
	setCA(w.now.Add(365 * 24 * time.Hour))
	w.steps(2)
	w.expectAlert(KindCertExpiry, SubjectCA, false, 0)
}

// TestIncarnation: a new node is recorded without alert, a changed
// incarnation is recorded and alerted once, and an older node is ignored.
func TestIncarnation(t *testing.T) {
	w := newWorld(t)
	w.state.nodes = append(w.state.nodes, store.Node{ID: "old", Role: store.NodeRoleVoter, Healthy: true})
	w.peers["old"] = Signals{NodeID: "old", RaftState: "Follower"} // no ADR 0025 fields
	w.lead()
	w.steps(1)
	if got := w.state.incs["n2"]; got != "inc-n2" {
		t.Fatalf("new node not recorded: %q", got)
	}
	if n := w.applier.countKind(KindIncarnation); n != 0 {
		t.Fatalf("new node alerted: %d", n)
	}
	if _, ok := w.state.incs["old"]; ok {
		t.Fatalf("older node recorded")
	}

	p := w.peers["n2"]
	p.Incarnation = "inc-n2-new"
	w.peers["n2"] = p
	w.steps(1)
	if got := w.state.incs["n2"]; got != "inc-n2-new" {
		t.Fatalf("changed incarnation not recorded: %q", got)
	}
	w.expectAlert(KindIncarnation, "n2", true, 1)
	w.steps(2)
	if n := w.applier.countKind(KindIncarnation); n != 1 {
		t.Fatalf("incarnation alerts = %d, want 1", n)
	}

	// A second change counts up, so it notifies again.
	p.Incarnation = "inc-n2-third"
	w.peers["n2"] = p
	w.steps(1)
	w.expectAlert(KindIncarnation, "n2", true, 2)
}

// TestFingerprintMismatchOncePerIndex: nodes that report different
// fingerprints at the same checkpoint index raise one alert per index.
func TestFingerprintMismatchOncePerIndex(t *testing.T) {
	w := newWorld(t)
	w.self.FingerprintIndex, w.self.Fingerprint = 10, "aaa"
	p := w.peers["n2"]
	p.FingerprintIndex, p.Fingerprint = 10, "bbb"
	w.peers["n2"] = p
	w.lead()
	w.steps(1)
	w.expectAlert(KindFingerprint, "10", true, 0)
	w.steps(2)
	if n := w.applier.countKind(KindFingerprint); n != 1 {
		t.Fatalf("fingerprint alerts = %d, want 1", n)
	}
	var details map[string]any
	for _, cmd := range w.applier.cmds {
		if s := cmd.GetSetSystemAlert(); s != nil && s.GetKind() == KindFingerprint {
			if err := json.Unmarshal(s.GetDetailsJson(), &details); err != nil {
				t.Fatal(err)
			}
		}
	}
	if details["index"] != float64(10) {
		t.Errorf("details index = %v", details["index"])
	}
	if nodes, ok := details["nodes"].([]any); !ok || len(nodes) != 2 {
		t.Errorf("details nodes = %v, want the two reporting nodes", details["nodes"])
	}

	// Matching fingerprints at the next index raise nothing.
	w.self.FingerprintIndex, w.self.Fingerprint = 11, "ccc"
	p.FingerprintIndex, p.Fingerprint = 11, "ccc"
	w.peers["n2"] = p
	w.steps(2)
	if n := w.applier.countKind(KindFingerprint); n != 1 {
		t.Fatalf("fingerprint alerts after a matching index = %d, want 1", n)
	}
}

// TestMixedVersionsOncePerDay: voters that differ in their command versions
// for more than 24 h raise mixed_versions, and once more each day.
func TestMixedVersionsOncePerDay(t *testing.T) {
	w := newWorld(t)
	w.state.nodes[2].MaxCommandVersion = 5
	w.lead()
	w.steps(2)
	w.expectAlert(KindMixedVersions, SubjectCluster, false, 0)

	w.now = w.now.Add(25 * time.Hour)
	w.steps(2)
	day := int(w.now.Unix() / 86400)
	w.expectAlert(KindMixedVersions, SubjectCluster, true, day)
	w.steps(4)
	if n := w.applier.countKind(KindMixedVersions); n != 1 {
		t.Fatalf("mixed_versions commands on one day = %d, want 1", n)
	}

	w.now = w.now.Add(24 * time.Hour)
	w.steps(2)
	w.expectAlert(KindMixedVersions, SubjectCluster, true, day+1)

	w.state.nodes[2].MaxCommandVersion = 6
	w.steps(2)
	w.expectAlert(KindMixedVersions, SubjectCluster, false, 0)
}

// caCert returns a self-signed certificate that expires at notAfter.
func caCert(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("test-ca-%d", notAfter.Unix())},
		NotBefore:    notAfter.Add(-365 * 24 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// nodeFSM applies commands to a fresh FSM over a fresh state.db. Request IDs
// are generated, so no command is mistaken for a duplicate.
type nodeFSM struct {
	t     *testing.T
	st    *sqlite.StateStore
	fsm   *FSM
	index uint64
	reqs  int
}

func newNodeFSM(t *testing.T) *nodeFSM {
	t.Helper()
	st := openStore(t, t.TempDir())
	fsm, err := NewFSM(context.Background(), st, slog.New(slog.DiscardHandler), func(err error) { t.Errorf("unexpected halt: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return &nodeFSM{t: t, st: st, fsm: fsm}
}

// apply sets the command time, replicates it through the log and returns the result.
func (f *nodeFSM) apply(c *pb.Command, atMs int64) ApplyResult {
	f.t.Helper()
	f.index++
	f.reqs++
	c.RequestId = fmt.Sprintf("req-%d", f.reqs)
	c.IssuedAtMs = atMs
	data, err := proto.Marshal(c)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.fsm.Apply(&raft.Log{Index: f.index, Type: raft.LogCommand, Data: data}).(ApplyResult)
}

func (f *nodeFSM) node(id string) (store.Node, bool) {
	f.t.Helper()
	list, err := f.st.ListNodes(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	for _, n := range list {
		if n.ID == id {
			return n, true
		}
	}
	return store.Node{}, false
}

func validNodeInfo() *pb.NodeInfo {
	return &pb.NodeInfo{
		Id:                "node-1",
		Name:              "one",
		Role:              pb.NodeInfo_ROLE_VOTER,
		Region:            "eu",
		Labels:            map[string]string{"env": "prod"},
		Capabilities:      []string{"icmp"},
		Version:           "1.0.0",
		MaxCommandVersion: 2,
	}
}

func TestRegisterNodeValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(n *pb.NodeInfo)
		wantErr bool
	}{
		{"valid", func(*pb.NodeInfo) {}, false},
		{"name at limit", func(n *pb.NodeInfo) { n.Name = strings.Repeat("n", 200) }, false},
		{"region and version at limit", func(n *pb.NodeInfo) {
			n.Region = strings.Repeat("r", 64)
			n.Version = strings.Repeat("v", 64)
		}, false},
		{"32 capabilities at limit", func(n *pb.NodeInfo) {
			n.Capabilities = nil
			for i := range 32 {
				n.Capabilities = append(n.Capabilities, fmt.Sprintf("cap-%02d", i))
			}
		}, false},
		{"32 labels with maximal key and value", func(n *pb.NodeInfo) {
			n.Labels = map[string]string{}
			for i := range 32 {
				n.Labels[fmt.Sprintf("%0*d", 64, i)] = strings.Repeat("v", 256)
			}
		}, false},
		{"empty ID", func(n *pb.NodeInfo) { n.Id = "" }, true},
		{"ID with slash", func(n *pb.NodeInfo) { n.Id = "node/1" }, true},
		{"unspecified role", func(n *pb.NodeInfo) { n.Role = pb.NodeInfo_ROLE_UNSPECIFIED }, true},
		{"unknown role", func(n *pb.NodeInfo) { n.Role = pb.NodeInfo_Role(9) }, true},
		{"name too long", func(n *pb.NodeInfo) { n.Name = strings.Repeat("n", 201) }, true},
		{"region too long", func(n *pb.NodeInfo) { n.Region = strings.Repeat("r", 65) }, true},
		{"version too long", func(n *pb.NodeInfo) { n.Version = strings.Repeat("v", 65) }, true},
		{"33 capabilities", func(n *pb.NodeInfo) {
			n.Capabilities = nil
			for i := range 33 {
				n.Capabilities = append(n.Capabilities, fmt.Sprintf("cap-%02d", i))
			}
		}, true},
		{"capability with upper case", func(n *pb.NodeInfo) { n.Capabilities = []string{"ICMP"} }, true},
		{"capability with space", func(n *pb.NodeInfo) { n.Capabilities = []string{"ic mp"} }, true},
		{"empty capability", func(n *pb.NodeInfo) { n.Capabilities = []string{""} }, true},
		{"capability too long", func(n *pb.NodeInfo) { n.Capabilities = []string{strings.Repeat("c", 33)} }, true},
		{"33 labels", func(n *pb.NodeInfo) {
			n.Labels = map[string]string{}
			for i := range 33 {
				n.Labels[fmt.Sprintf("k%02d", i)] = "v"
			}
		}, true},
		{"label key too long", func(n *pb.NodeInfo) { n.Labels = map[string]string{strings.Repeat("k", 65): "v"} }, true},
		{"label value too long", func(n *pb.NodeInfo) { n.Labels = map[string]string{"k": strings.Repeat("v", 257)} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFSM(t)
			info := validNodeInfo()
			tt.mutate(info)
			cmd := stamp(&pb.Command{Payload: &pb.Command_RegisterNode{RegisterNode: &pb.RegisterNode{Node: info}}})
			res := f.apply(cmd, 1_000)
			if tt.wantErr {
				if !errors.Is(res.Err, ErrInvalidCommand) {
					t.Fatalf("err = %v, want ErrInvalidCommand", res.Err)
				}
				if _, ok := f.node("node-1"); ok {
					t.Error("rejected node was registered")
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("err = %v, want success", res.Err)
			}
		})
	}
}

func TestRegisterNodeUpsertKeepsHealth(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	first := f.apply(NewRegisterNode("", store.Node{
		ID: "node-1", Name: "one", Role: store.NodeRoleVoter,
		Capabilities: []string{"icmp", "http", "icmp"}, Labels: map[string]string{"env": "prod"},
		Region: "eu", Version: "1.0.0", MaxCommandVersion: 2,
	}), 1_000)
	if first.Err != nil {
		t.Fatal(first.Err)
	}
	n, ok := f.node("node-1")
	if !ok {
		t.Fatal("node not registered")
	}
	wantFirst := store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleVoter,
		Capabilities: []string{"http", "icmp"}, Labels: map[string]string{"env": "prod"},
		Region: "eu", Version: "1.0.0", MaxCommandVersion: 2,
		Healthy: true, HealthChangedMs: 1_000, RegisteredMs: 1_000, UpdatedMs: 1_000}
	if !nodeEqual(n, wantFirst) {
		t.Fatalf("first registration = %+v, want %+v", n, wantFirst)
	}

	if r := f.apply(NewSetNodeHealth("", "node-1", false), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	// An update replaces the descriptive fields, keeps health and registration time.
	update := f.apply(NewRegisterNode("", store.Node{
		ID: "node-1", Name: "renamed", Role: store.NodeRoleVoter,
		Capabilities: []string{"icmp"}, Version: "1.1.0", MaxCommandVersion: 2,
	}), 3_000)
	if update.Err != nil {
		t.Fatal(update.Err)
	}
	n, _ = f.node("node-1")
	wantUpdate := store.Node{ID: "node-1", Name: "renamed", Role: store.NodeRoleVoter,
		Capabilities: []string{"icmp"}, Labels: map[string]string{},
		Version: "1.1.0", MaxCommandVersion: 2,
		Healthy: false, HealthChangedMs: 2_000, RegisteredMs: 1_000, UpdatedMs: 3_000}
	if !nodeEqual(n, wantUpdate) {
		t.Errorf("updated registration = %+v, want %+v", n, wantUpdate)
	}
	var res NodeResult
	if err := json.Unmarshal(update.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ID != "node-1" || res.Healthy || res.RegisteredMs != 1_000 || res.UpdatedMs != 3_000 {
		t.Errorf("result = %+v", res)
	}
}

func TestSetNodeHealth(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewSetNodeHealth("", "node-x", false), 500); !errors.Is(r.Err, store.ErrNotFound) {
		t.Errorf("unknown node: err = %v, want ErrNotFound", r.Err)
	}
	if r := f.apply(NewSetNodeHealth("", "bad/id", false), 500); !errors.Is(r.Err, ErrInvalidCommand) {
		t.Errorf("invalid ID: err = %v, want ErrInvalidCommand", r.Err)
	}
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleProbe}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}

	// The same value is a success without a change.
	if r := f.apply(NewSetNodeHealth("", "node-1", true), 2_000); r.Err != nil {
		t.Fatalf("same health: %v", r.Err)
	}
	n, _ := f.node("node-1")
	if !n.Healthy || n.HealthChangedMs != 1_000 || n.UpdatedMs != 1_000 {
		t.Errorf("unchanged health modified the node: %+v", n)
	}

	if r := f.apply(NewSetNodeHealth("", "node-1", false), 3_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	n, _ = f.node("node-1")
	if n.Healthy || n.HealthChangedMs != 3_000 || n.UpdatedMs != 3_000 || n.RegisteredMs != 1_000 {
		t.Errorf("health change = %+v, want unhealthy at 3000", n)
	}
}

func TestRevokeNodeRemovesRegistration(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRevokeNode("", "node-1"), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if _, ok := f.node("node-1"); ok {
		t.Fatal("revoked node still in the registry")
	}

	// A revoked node cannot register again, and is not found for health changes.
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 3_000); !errors.Is(r.Err, store.ErrConflict) {
		t.Errorf("revoked node registration: err = %v, want ErrConflict", r.Err)
	}
	if _, ok := f.node("node-1"); ok {
		t.Error("revoked node was registered")
	}
	if r := f.apply(NewSetNodeHealth("", "node-1", false), 4_000); !errors.Is(r.Err, store.ErrNotFound) {
		t.Errorf("health of revoked node: err = %v, want ErrNotFound", r.Err)
	}

	// Other nodes are not affected.
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-2", Role: store.NodeRoleVoter}), 5_000); r.Err != nil {
		t.Errorf("other node registration: %v", r.Err)
	}
}

func nodeEqual(a, b store.Node) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Role == b.Role &&
		slices.Equal(a.Capabilities, b.Capabilities) && maps.Equal(a.Labels, b.Labels) &&
		a.Region == b.Region && a.Version == b.Version && a.MaxCommandVersion == b.MaxCommandVersion &&
		a.Healthy == b.Healthy && a.HealthChangedMs == b.HealthChangedMs &&
		a.RegisteredMs == b.RegisteredMs && a.UpdatedMs == b.UpdatedMs
}

// ADR 0022: SetNodeConnectivity validates the reason, changes only on a change
// and reports unknown nodes.
func TestSetNodeConnectivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		nodeID  string
		impair  bool
		reason  string
		atMs    int64
		wantErr error
		// wantChanged is the change time expected after the command, 0 for none.
		wantChanged int64
		wantImpair  bool
		wantReason  string
	}{
		{"unknown node", "node-x", true, "reference", 3_000, store.ErrNotFound, 0, false, ""},
		{"invalid node ID", "bad/id", true, "reference", 3_000, ErrInvalidCommand, 0, false, ""},
		{"impaired without reason", "node-1", true, "", 3_000, ErrInvalidCommand, 0, false, ""},
		{"impaired with unknown reason", "node-1", true, "other", 3_000, ErrInvalidCommand, 0, false, ""},
		{"not impaired with reason", "node-1", false, "reference", 3_000, ErrInvalidCommand, 0, false, ""},
		{"impaired by reference", "node-1", true, "reference", 3_000, nil, 3_000, true, "reference"},
		{"impaired by deviation", "node-1", true, "deviation", 3_000, nil, 3_000, true, "deviation"},
		{"same state is no change", "node-1", true, "reference", 4_000, nil, 3_000, true, "reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFSM(t)
			if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 1_000); r.Err != nil {
				t.Fatal(r.Err)
			}
			// The first valid impairment sets the state at 3000; the test row
			// then checks a repeat or the invalid input against it.
			if tt.name == "same state is no change" {
				if r := f.apply(NewSetNodeConnectivity("node-1", true, "reference"), 3_000); r.Err != nil {
					t.Fatal(r.Err)
				}
			}
			r := f.apply(NewSetNodeConnectivity(tt.nodeID, tt.impair, tt.reason), tt.atMs)
			if tt.wantErr != nil {
				if !errors.Is(r.Err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", r.Err, tt.wantErr)
				}
				return
			}
			if r.Err != nil {
				t.Fatalf("unexpected error: %v", r.Err)
			}
			n, ok := f.node("node-1")
			if !ok {
				t.Fatal("node-1 missing")
			}
			if n.ConnectivityImpaired != tt.wantImpair || n.ConnectivityReason != tt.wantReason {
				t.Errorf("connectivity = %v/%q, want %v/%q", n.ConnectivityImpaired, n.ConnectivityReason, tt.wantImpair, tt.wantReason)
			}
			if n.ConnectivityChangedMs != tt.wantChanged {
				t.Errorf("connectivity changed at %d, want %d", n.ConnectivityChangedMs, tt.wantChanged)
			}
			if n.Healthy != true || n.HealthChangedMs != 1_000 {
				t.Errorf("connectivity changed the health: %+v", n)
			}
		})
	}
}

// ADR 0022: recovery clears the reason, and the result shows the new state.
func TestSetNodeConnectivityRecovery(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewSetNodeConnectivity("node-1", true, "deviation"), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	r := f.apply(NewSetNodeConnectivity("node-1", false, ""), 3_000)
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	var res NodeResult
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.ConnectivityImpaired || res.ConnectivityReason != "" || res.ConnectivityChangedMs != 3_000 {
		t.Errorf("result = %+v, want recovered at 3000", res)
	}
	n, _ := f.node("node-1")
	if n.ConnectivityImpaired || n.ConnectivityReason != "" || n.ConnectivityChangedMs != 3_000 || n.UpdatedMs != 3_000 {
		t.Errorf("node after recovery = %+v", n)
	}
}

// ADR 0022: the connectivity columns are replicated state, so a change moves the fingerprint.
func TestSetNodeConnectivityChangesFingerprint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newNodeFSM(t)
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	before, err := f.st.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.apply(NewSetNodeConnectivity("node-1", true, "reference"), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	after, err := f.st.Fingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("fingerprint did not change with the connectivity")
	}
}

// ADR 0022: a registration keeps the connectivity the leader decided.
func TestRegisterNodeKeepsConnectivity(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter, Version: "1.0.0"}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewSetNodeConnectivity("node-1", true, "reference"), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRegisterNode("", store.Node{ID: "node-1", Role: store.NodeRoleVoter, Version: "1.1.0"}), 3_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	n, _ := f.node("node-1")
	if n.Version != "1.1.0" {
		t.Errorf("version = %q, want 1.1.0", n.Version)
	}
	if !n.ConnectivityImpaired || n.ConnectivityReason != "reference" || n.ConnectivityChangedMs != 2_000 {
		t.Errorf("registration reset the connectivity: %+v", n)
	}
}

func TestRegisterProbePublicAndPrivate(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	putTenantCmd, err := NewPutTenant("", "t1", TenantDocument{Name: "t1", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if r := f.apply(putTenantCmd, 500); r.Err != nil {
		t.Fatal(r.Err)
	}

	if r := f.apply(NewRegisterProbe("", store.Node{
		ID:                "probe-pub",
		Name:              "pub",
		Region:            "eu",
		Labels:            map[string]string{"env": "prod"},
		Capabilities:      []string{"http", "icmp"},
		Version:           "1.0.0",
		MaxCommandVersion: ProbeCommandLevel,
		Incarnation:       "inc-pub",
	}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	pub, ok := f.node("probe-pub")
	if !ok {
		t.Fatal("probe-pub missing")
	}
	if pub.Role != store.NodeRoleProbe || !pub.Healthy || pub.HealthChangedMs != 1_000 ||
		pub.RegisteredMs != 1_000 || pub.UpdatedMs != 1_000 ||
		pub.OwnerTenantID != "" || pub.Incarnation != "inc-pub" {
		t.Errorf("public probe = %+v", pub)
	}

	if r := f.apply(NewRegisterProbe("", store.Node{
		ID:            "probe-priv",
		Name:          "priv",
		OwnerTenantID: "t1",
		Incarnation:   "inc-priv",
	}), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	priv, ok := f.node("probe-priv")
	if !ok {
		t.Fatal("probe-priv missing")
	}
	if priv.Role != store.NodeRoleProbe || !priv.Healthy || priv.HealthChangedMs != 2_000 ||
		priv.RegisteredMs != 2_000 || priv.UpdatedMs != 2_000 ||
		priv.OwnerTenantID != "t1" || priv.Incarnation != "inc-priv" {
		t.Errorf("private probe = %+v", priv)
	}
}

func TestRegisterProbeUpdatePreservesHealthAndConnectivity(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewRegisterProbe("", store.Node{
		ID:          "probe-1",
		Name:        "one",
		Version:     "1.0.0",
		Incarnation: "inc-1",
	}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewSetNodeHealth("", "probe-1", false), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewSetNodeConnectivity("probe-1", true, ConnectivityReasonReference), 3_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRegisterProbe("", store.Node{
		ID:          "probe-1",
		Name:        "renamed",
		Version:     "1.1.0",
		Incarnation: "inc-2",
	}), 4_000); r.Err != nil {
		t.Fatal(r.Err)
	}

	n, ok := f.node("probe-1")
	if !ok {
		t.Fatal("probe-1 missing")
	}
	if n.Name != "renamed" || n.Version != "1.1.0" || n.Incarnation != "inc-2" || n.UpdatedMs != 4_000 {
		t.Errorf("updated metadata = %+v", n)
	}
	if n.Healthy || n.HealthChangedMs != 2_000 {
		t.Errorf("health not preserved: %+v", n)
	}
	if !n.ConnectivityImpaired || n.ConnectivityReason != ConnectivityReasonReference || n.ConnectivityChangedMs != 3_000 {
		t.Errorf("connectivity not preserved: %+v", n)
	}
	if n.RegisteredMs != 1_000 {
		t.Errorf("RegisteredMs = %d, want 1000", n.RegisteredMs)
	}
}

func TestRegisterProbeRejections(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewSetFeatureLevel("", TenantAdminCommandLevel), 100); r.Err != nil {
		t.Fatal(r.Err)
	}
	for _, id := range []string{"t1", "t2", "t-del"} {
		cmd, err := NewPutTenant("", id, TenantDocument{Name: id, Plan: "default"})
		if err != nil {
			t.Fatal(err)
		}
		if r := f.apply(cmd, 200); r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if r := f.apply(adminCommand("", pb.Actor_KIND_SYSTEM, &pb.SetTenantSuspended{TenantId: "t-del", Suspended: true}), 300); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(adminCommand("", pb.Actor_KIND_SYSTEM, &pb.DeleteTenant{TenantId: "t-del"}), 400); r.Err != nil {
		t.Fatal(r.Err)
	}

	if r := f.apply(NewRegisterProbe("", store.Node{ID: "probe-revoked"}), 500); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRevokeNode("", "probe-revoked"), 600); r.Err != nil {
		t.Fatal(r.Err)
	}

	if r := f.apply(NewRegisterProbe("", store.Node{ID: "probe-owned", OwnerTenantID: "t1"}), 700); r.Err != nil {
		t.Fatal(r.Err)
	}

	if r := f.apply(NewRegisterNode("", store.Node{ID: "voter-1", Role: store.NodeRoleVoter}), 800); r.Err != nil {
		t.Fatal(r.Err)
	}

	tests := []struct {
		name     string
		cmd      *pb.Command
		wantCode string
	}{
		{
			name: "non-probe role",
			cmd: stamp(&pb.Command{Payload: &pb.Command_RegisterProbe{RegisterProbe: &pb.RegisterProbe{
				Node: &pb.NodeInfo{Id: "probe-voter", Role: pb.NodeInfo_ROLE_VOTER},
			}}}),
			wantCode: CodeInvalid,
		},
		{
			name:     "incarnation too long",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-inc", Incarnation: strings.Repeat("a", maxNodeIncarnationLen+1)}),
			wantCode: CodeInvalid,
		},
		{
			name:     "revoked node",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-revoked"}),
			wantCode: CodeConflict,
		},
		{
			name:     "unknown owner tenant",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-unknown", OwnerTenantID: "no-such-tenant"}),
			wantCode: CodeInvalid,
		},
		{
			name:     "deleted owner tenant",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-del", OwnerTenantID: "t-del"}),
			wantCode: CodeInvalid,
		},
		{
			name:     "system owner tenant",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-sys", OwnerTenantID: store.SystemTenantID}),
			wantCode: CodeInvalid,
		},
		{
			name:     "change owner tenant to another tenant",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-owned", OwnerTenantID: "t2"}),
			wantCode: CodeConflict,
		},
		{
			name:     "change owner tenant to public",
			cmd:      NewRegisterProbe("", store.Node{ID: "probe-owned", OwnerTenantID: ""}),
			wantCode: CodeConflict,
		},
		{
			name:     "existing voter id",
			cmd:      NewRegisterProbe("", store.Node{ID: "voter-1"}),
			wantCode: CodeConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := f.apply(tt.cmd, 1_000)
			checkCode(t, r.Err, tt.wantCode)
		})
	}
}

func TestSubscribeChanges(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	ch1, unsub1 := f.fsm.SubscribeChanges()
	ch2, unsub2 := f.fsm.SubscribeChanges()
	defer unsub2()

	if r := f.apply(NewRegisterNode("", store.Node{ID: "n1", Role: store.NodeRoleVoter}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRegisterNode("", store.Node{ID: "n2", Role: store.NodeRoleVoter}), 2_000); r.Err != nil {
		t.Fatal(r.Err)
	}

	for name, ch := range map[string]<-chan struct{}{"sub1": ch1, "sub2": ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s did not receive change notification", name)
		}
		select {
		case <-ch:
			t.Fatalf("%s received uncoalesced extra notification", name)
		default:
		}
	}

	unsub1()
	if r := f.apply(NewRegisterNode("", store.Node{ID: "n3", Role: store.NodeRoleVoter}), 3_000); r.Err != nil {
		t.Fatal(r.Err)
	}

	select {
	case <-ch2:
	case <-time.After(time.Second):
		t.Fatal("sub2 did not receive change notification after sub1 unsubscribed")
	}
	select {
	case <-ch1:
		t.Fatal("sub1 received notification after unsubscribe")
	default:
	}
}

func TestRegisterNodePublicURL(t *testing.T) {
	t.Parallel()
	voter := func(publicURL string) store.Node {
		return store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleVoter, Version: "1.0.0",
			MaxCommandVersion: CommandVersion, PublicURL: publicURL}
	}
	tests := []struct {
		name    string
		level   uint32 // command level set before the registration; 0 means none
		node    store.Node
		wantErr bool
		want    string // stored public URL
	}{
		{"level 10 ignores the public URL", 10, voter("https://n1.example.com"), false, ""},
		{"level 11 stores the normalized origin", 11, voter("HTTPS://N1.Example.com:443/"), false, "https://n1.example.com"},
		{"level 11 keeps an empty URL empty", 11, voter(""), false, ""},
		{"level 11 rejects http for a public host", 11, voter("http://n1.example.com"), true, ""},
		{"level 11 rejects a path", 11, voter("https://n1.example.com/path"), true, ""},
		{"level 11 rejects a probe with a public URL", 11,
			store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleProbe, PublicURL: "https://p.example.com"}, true, ""},
		{"no level set ignores the public URL", 0, voter("https://n1.example.com"), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFSM(t)
			if tt.level != 0 {
				if r := f.apply(NewSetFeatureLevel("", tt.level), 500); r.Err != nil {
					t.Fatal(r.Err)
				}
			}
			r := f.apply(NewRegisterNode("", tt.node), 1_000)
			if tt.wantErr {
				if !errors.Is(r.Err, ErrInvalidCommand) {
					t.Fatalf("err = %v, want ErrInvalidCommand", r.Err)
				}
				if _, ok := f.node(tt.node.ID); ok {
					t.Error("rejected registration was stored")
				}
				return
			}
			if r.Err != nil {
				t.Fatalf("err = %v, want success", r.Err)
			}
			n, ok := f.node(tt.node.ID)
			if !ok {
				t.Fatal("node missing")
			}
			if n.PublicURL != tt.want {
				t.Errorf("public URL = %q, want %q", n.PublicURL, tt.want)
			}
		})
	}
}

func TestRegisterProbeNeverStoresPublicURL(t *testing.T) {
	t.Parallel()
	f := newNodeFSM(t)
	if r := f.apply(NewSetFeatureLevel("", PublicURLCommandLevel), 500); r.Err != nil {
		t.Fatal(r.Err)
	}
	if r := f.apply(NewRegisterProbe("", store.Node{
		ID: "probe-pub", Name: "pub", PublicURL: "https://p.example.com", MaxCommandVersion: ProbeCommandLevel,
	}), 1_000); r.Err != nil {
		t.Fatal(r.Err)
	}
	n, ok := f.node("probe-pub")
	if !ok {
		t.Fatal("probe-pub missing")
	}
	if n.PublicURL != "" {
		t.Errorf("probe public URL = %q, want empty", n.PublicURL)
	}
}

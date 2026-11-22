// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/membership"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// putProbe writes a registered probe straight into the node registry.
func putProbe(t *testing.T, e *env, n store.Node) {
	t.Helper()
	ctx := context.Background()
	n.Role = store.NodeRoleProbe
	if err := e.state.Update(ctx, func(tx store.StateTx) error { return tx.PutNode(ctx, n) }); err != nil {
		t.Fatal(err)
	}
}

// TestListClusterNodesProbes covers ADR 0034/0035: registered probes are
// listed with their role, region, owner, session and buffer, but never count
// as voters (FA-PRB-07).
func TestListClusterNodesProbes(t *testing.T) {
	admin := &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1"}
	sessions := map[string]ProbeSessionView{
		"probe-pub": {
			ConnectedVoterID: "n2",
			Buffer:           &pb.ProbeBufferStatus{Bytes: 4096, Unacked: 7, OldestUnackedMs: 1500, DroppedTotal: 2},
		},
	}
	e := newEnvTweaked(t, false, func(d *Deps) {
		d.ClusterAdmin = admin
		d.ProbeSessions = func() map[string]ProbeSessionView { return sessions }
	})

	baseline := decode[gen.ClusterNodeList](t, e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil))

	putProbe(t, e, store.Node{ID: "probe-pub", Name: "fra-1", Region: "eu-central", Version: "1.0.0", MaxCommandVersion: 10, Healthy: true})
	putProbe(t, e, store.Node{ID: "probe-priv", Name: "lan-1", OwnerTenantID: "tenant-a", Version: "0.9.0", MaxCommandVersion: 1})

	res := e.do("GET", "/api/v1/admin/nodes", platformAdmin, nil)
	if res.code != http.StatusOK {
		t.Fatalf("= %d %s", res.code, res.body)
	}
	got := decode[gen.ClusterNodeList](t, res)

	byID := map[string]gen.ClusterNode{}
	voterCount := 0
	for _, n := range got.Nodes {
		byID[n.Id] = n
		if n.Voter {
			voterCount++
			if n.Role == nil || *n.Role != gen.ClusterNodeRoleVoter {
				t.Errorf("voter %s role = %v, want voter", n.Id, n.Role)
			}
		}
	}
	if voterCount != 3 {
		t.Errorf("voters = %d, want 3 (probes must not count)", voterCount)
	}
	if len(got.Nodes) != len(baseline.Nodes)+2 {
		t.Errorf("nodes = %d, want %d", len(got.Nodes), len(baseline.Nodes)+2)
	}
	if len(got.Warnings) != len(baseline.Warnings) {
		t.Errorf("warnings = %v, want unchanged %v", got.Warnings, baseline.Warnings)
	}
	// A probe with an old command version must not mark a rolling update.
	if got.UpgradePending == nil || *got.UpgradePending != *baseline.UpgradePending {
		t.Errorf("upgradePending = %v, want %v", got.UpgradePending, baseline.UpgradePending)
	}

	pub, ok := byID["probe-pub"]
	if !ok {
		t.Fatal("public probe missing from listing")
	}
	if pub.Role == nil || *pub.Role != gen.ClusterNodeRoleProbe || pub.Voter || pub.Leader {
		t.Errorf("public probe role/voter/leader = %v/%v/%v", pub.Role, pub.Voter, pub.Leader)
	}
	if pub.Region == nil || *pub.Region != "eu-central" {
		t.Errorf("region = %v, want eu-central", pub.Region)
	}
	if pub.OwnerTenantId != nil {
		t.Errorf("public probe ownerTenantId = %q, want none", *pub.OwnerTenantId)
	}
	if pub.ConnectedVoterId == nil || *pub.ConnectedVoterId != "n2" {
		t.Errorf("connectedVoterId = %v, want n2", pub.ConnectedVoterId)
	}
	if b := pub.ProbeBuffer; b == nil || b.Bytes != 4096 || b.Unacked != 7 || b.DroppedTotal != 2 ||
		b.OldestUnackedMs == nil || *b.OldestUnackedMs != 1500 {
		t.Errorf("probeBuffer = %+v", pub.ProbeBuffer)
	}
	if pub.Name == nil || *pub.Name != "fra-1" {
		t.Errorf("name = %v, want fra-1", pub.Name)
	}

	priv, ok := byID["probe-priv"]
	if !ok {
		t.Fatal("private probe missing from listing")
	}
	if priv.OwnerTenantId == nil || *priv.OwnerTenantId != "tenant-a" {
		t.Errorf("private probe ownerTenantId = %v, want tenant-a", priv.OwnerTenantId)
	}
	if priv.ConnectedVoterId != nil || priv.ProbeBuffer != nil {
		t.Errorf("probe without session shows connectedVoterId=%v buffer=%v", priv.ConnectedVoterId, priv.ProbeBuffer)
	}
}

// TestRemoveClusterNodeProbe checks that a probe is removed through the same
// endpoint as a voter.
func TestRemoveClusterNodeProbe(t *testing.T) {
	admin := &fakeClusterAdmin{members: voters(3), leaderID: "n1", selfID: "n1", removal: membership.Removal{VotersLeft: 3}}
	e := clusterEnv(t, admin)
	putProbe(t, e, store.Node{ID: "probe-pub", Name: "fra-1"})

	res := e.do("DELETE", "/api/v1/admin/nodes/probe-pub", platformAdmin, nil)
	if res.code != http.StatusOK {
		t.Fatalf("= %d %s", res.code, res.body)
	}
	if admin.removeCall != 1 || admin.removedID != "probe-pub" {
		t.Errorf("remove calls = %d id = %q, want 1 probe-pub", admin.removeCall, admin.removedID)
	}
	got := decode[gen.NodeRemoval](t, res)
	if got.VotersLeft != 3 {
		t.Errorf("votersLeft = %d, want 3", got.VotersLeft)
	}
}

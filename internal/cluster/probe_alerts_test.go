// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

func setupProbeAlertFSM(t *testing.T, level uint32) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(level)
	h.putSystemTenant(1000)
	h.putTenant(1000, "t1")
	h.putTenant(1000, "t2")
	return h
}

func (h *alertFSM) putPlan(issuedAt int64, id string, limitsJSON string) {
	h.t.Helper()
	cmd := adminCommand("plan-"+id+"-"+fmt.Sprint(issuedAt), actorSystem, &pb.PutPlan{
		PlanId: id, Name: id, LimitsJson: limitsJSON,
	})
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(cmd, issuedAt, nil)
}

func (h *alertFSM) createTenant(issuedAt int64, id, planID string) {
	h.t.Helper()
	cmd := adminCommand("create-"+id, actorSystem, &pb.CreateTenant{
		TenantId: id, Name: id, PlanId: planID, Source: tenant.SourceAdmin,
	})
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(cmd, issuedAt, nil)
}

func (h *alertFSM) suspendTenant(issuedAt int64, id string, suspended bool) {
	h.t.Helper()
	cmd := adminCommand("suspend-"+id+"-"+fmt.Sprint(suspended), actorSystem, &pb.SetTenantSuspended{
		TenantId: id, Suspended: suspended,
	})
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(cmd, issuedAt, nil)
}

func TestSetProbeAlertLevelAndValidation(t *testing.T) {
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u1", Name: "Ada"}

	t.Run("rejected below level 10", func(t *testing.T) {
		h := setupProbeAlertFSM(t, ProbeCommandLevel-1)
		h.putChannel(1100, "t1", "ch1", channel(store.EventProbeOffline), ErrInvalidCommand)
		h.do(NewRegisterNode("v1", store.Node{ID: "v1", Role: store.NodeRoleVoter}), 1100, nil)
		h.do(NewSetProbeAlert("t1", "p1", true, "v1"), 1200, ErrInvalidCommand)
	})

	t.Run("validation and ownership", func(t *testing.T) {
		h := setupProbeAlertFSM(t, ProbeCommandLevel)
		h.do(NewRegisterNode("v1", store.Node{ID: "v1", Role: store.NodeRoleVoter}), 1100, nil)
		h.do(NewRegisterProbe("rp-pub", store.Node{ID: "p-pub", Name: "pub"}), 1100, nil)
		h.do(NewRegisterProbe("rp-t1", store.Node{ID: "p1", Name: "edge-1", OwnerTenantID: "t1"}), 1100, nil)
		h.do(NewRegisterProbe("rp-t2", store.Node{ID: "p2", Name: "edge-2", OwnerTenantID: "t2"}), 1100, nil)

		// Non-system actor rejected
		badActor := NewSetProbeAlert("t1", "p1", true, "v1")
		badActor.Actor = user
		h.do(badActor, 1200, ErrInvalidCommand)

		// System tenant rejected
		h.do(NewSetProbeAlert(store.SystemTenantID, "p1", true, "v1"), 1200, ErrInvalidCommand)
		// Empty tenant rejected
		h.do(NewSetProbeAlert("", "p1", true, "v1"), 1200, ErrInvalidCommand)
		// Invalid probe ID
		h.do(NewSetProbeAlert("t1", "bad id", true, "v1"), 1200, ErrInvalidCommand)
		// Unknown probe -> NotFound
		h.do(NewSetProbeAlert("t1", "missing", true, "v1"), 1200, store.ErrNotFound)
		// Voter -> invalid
		h.do(NewSetProbeAlert("t1", "v1", true, "v1"), 1200, ErrInvalidCommand)
		// Public probe -> invalid
		h.do(NewSetProbeAlert("t1", "p-pub", true, "v1"), 1200, ErrInvalidCommand)
		// Foreign private probe -> invalid
		h.do(NewSetProbeAlert("t1", "p2", true, "v1"), 1200, ErrInvalidCommand)
	})
}

func TestSetProbeAlertLifecycleAndOutbox(t *testing.T) {
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	h.do(NewRegisterNode("v1", store.Node{ID: "v1", Role: store.NodeRoleVoter}), 1100, nil)
	h.do(NewRegisterNode("v2", store.Node{ID: "v2", Role: store.NodeRoleVoter}), 1100, nil)
	h.do(NewSetNodeHealth("v2-down", "v2", false), 1150, nil)
	h.do(NewRegisterProbe("rp-t1", store.Node{ID: "p1", Name: "edge-1", OwnerTenantID: "t1"}), 1100, nil)

	h.putChannel(1200, "t1", "ch-sub", channel(store.EventDown, store.EventProbeOffline), nil)
	h.putChannel(1200, "t1", "ch-other", channel(store.EventDown), nil)
	h.putChannel(1200, "t2", "ch-t2", channel(store.EventProbeOffline), nil)
	h.putChannel(1200, store.SystemTenantID, "ch-sys", channel(store.EventSystem), nil)

	// Clearing a non-existent alert is a no-op.
	res := h.apply(NewSetProbeAlert("t1", "p1", false, "v1"), 1500)
	checkCode(t, res.Err, "")
	var r ProbeAlertResult
	if err := json.Unmarshal(res.Result, &r); err != nil || r.Changed {
		t.Fatalf("initial clear = %s, %v, want changed=false", res.Result, err)
	}

	// Raising active alert stores probe_alerts and enqueues only t1's subscribed channel.
	res = h.apply(NewSetProbeAlert("t1", "p1", true, "v1"), 2000)
	checkCode(t, res.Err, "")
	if err := json.Unmarshal(res.Result, &r); err != nil || !r.Changed {
		t.Fatalf("activate = %s, %v, want changed=true", res.Result, err)
	}

	// Repeated active is a no-op.
	res = h.apply(NewSetProbeAlert("t1", "p1", true, "v1"), 2100)
	checkCode(t, res.Err, "")
	if err := json.Unmarshal(res.Result, &r); err != nil || r.Changed {
		t.Fatalf("repeat activate = %s, %v, want changed=false", res.Result, err)
	}

	due, err := h.st.DueAlerts(h.ctx, 10_000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].TenantID != "t1" || due[0].ChannelID != "ch-sub" || due[0].MonitorID != "" {
		t.Fatalf("due alerts = %+v, want single t1/ch-sub entry", due)
	}
	var payload AlertPayload
	if err := json.Unmarshal(due[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != store.EventProbeOffline || payload.SinceMs != 2000 ||
		payload.Probe == nil || payload.Probe.ID != "p1" || payload.Probe.Name != "edge-1" || !payload.Probe.Active ||
		payload.Sender == nil || payload.Sender.NodeID != "v1" || payload.Sender.ReachableVoters != 1 || payload.Sender.TotalVoters != 2 {
		t.Fatalf("payload = %+v", payload)
	}

	// Clearing keeps since_ms = 2000 and enqueues clear delivery.
	res = h.apply(NewSetProbeAlert("t1", "p1", false, "v1"), 5000)
	checkCode(t, res.Err, "")
	if err := json.Unmarshal(res.Result, &r); err != nil || !r.Changed {
		t.Fatalf("clear = %s, %v, want changed=true", res.Result, err)
	}
	stored, err := h.st.ListProbeAlerts(h.ctx, "t1", false)
	if err != nil || len(stored) != 1 || stored[0].Active || stored[0].SinceMs != 2000 || stored[0].UpdatedMs != 5000 {
		t.Fatalf("stored probe alerts = %+v, %v", stored, err)
	}
	due, err = h.st.DueAlerts(h.ctx, 10_000, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("due after clear = %+v, %v, want 2", due, err)
	}

	// Suspended tenant ignores SetProbeAlert.
	h.suspendTenant(6000, "t1", true)
	res = h.apply(NewSetProbeAlert("t1", "p1", true, "v1"), 7000)
	checkCode(t, res.Err, "")
	if err := json.Unmarshal(res.Result, &r); err != nil || r.Changed {
		t.Fatalf("suspended activate = %s, %v, want changed=false", res.Result, err)
	}
}

func TestPrivateProbeTokenLimitsAndActors(t *testing.T) {
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
	apiToken := &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: "tok_1", Name: "ci"}
	system := &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}

	h.putPlan(1100, "two-probes", `{"maxPrivateProbes":2}`)
	h.createTenant(1100, "t-lim", "two-probes")

	mkTok := func(req, id, tenantID string, expires int64, actor *pb.Actor) *pb.Command {
		cmd, err := NewPutJoinToken(req, id, JoinTokenDocument{
			SecretHash: pki.HashSecret("secret-" + id),
			Role:       "probe",
			TenantID:   tenantID,
			ExpiresMs:  expires,
		})
		if err != nil {
			t.Fatal(err)
		}
		cmd.Actor = actor
		return cmd
	}

	// API token and system actor cannot create tenant-bound probe tokens at level 10.
	h.do(mkTok("r-tok", "jt-api", "t-lim", 50_000, apiToken), 2000, ErrInvalidCommand)
	h.do(mkTok("r-sys", "jt-sys", "t-lim", 50_000, system), 2000, ErrInvalidCommand)
	// Platform-level token (no tenant) still works with system/user actor.
	h.do(mkTok("r-pub", "jt-pub", "", 50_000, system), 2000, nil)

	// Create 1st open token (expires at 5000) and 2nd open token (expires at 50_000).
	h.do(mkTok("r1", "jt-1", "t-lim", 5000, user), 2000, nil)
	h.do(mkTok("r2", "jt-2", "t-lim", 50_000, user), 2000, nil)

	// 3rd token hits maxPrivateProbes = 2 (CodeLimitExceeded).
	res := h.apply(mkTok("r3", "jt-3", "t-lim", 50_000, user), 2500)
	checkCode(t, res.Err, CodeLimitExceeded)

	// At t=5000, jt-1 is expired, so only jt-2 counts -> slot frees up!
	h.do(mkTok("r4", "jt-4", "t-lim", 50_000, user), 5000, nil)

	// Redeem jt-2 and register probe p1; now 1 registered + 1 open (jt-4) = 2 -> limit reached again.
	h.do(NewRedeemJoinToken("rd-2", "jt-2", pki.HashSecret("secret-jt-2"), "p1"), 5100, nil)
	h.do(NewRegisterProbe("rp-1", store.Node{ID: "p1", Name: "p1", OwnerTenantID: "t-lim"}), 5200, nil)
	res = h.apply(mkTok("r5", "jt-5", "t-lim", 50_000, user), 5300)
	checkCode(t, res.Err, CodeLimitExceeded)

	// Audit entries recorded probe.token.create without secrets.
	var tokenAudits int
	for _, e := range auditEntries(t, h) {
		if e.TenantID == "t-lim" && e.Action == "probe.token.create" {
			tokenAudits++
		}
	}
	if tokenAudits != 3 {
		t.Errorf("probe.token.create audit entries = %d, want 3", tokenAudits)
	}
}

func TestRevokeTenantProbeAndDeleteTenant(t *testing.T) {
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}

	h.do(NewRegisterNode("v1", store.Node{ID: "v1", Role: store.NodeRoleVoter}), 1100, nil)
	h.do(NewRegisterProbe("rp-pub", store.Node{ID: "p-pub", Name: "pub"}), 1100, nil)
	h.do(NewRegisterProbe("rp-t1a", store.Node{ID: "p1a", Name: "p1a", OwnerTenantID: "t1"}), 1100, nil)
	h.do(NewRegisterProbe("rp-t1b", store.Node{ID: "p1b", Name: "p1b", OwnerTenantID: "t1"}), 1100, nil)
	h.do(NewRegisterProbe("rp-t2", store.Node{ID: "p2", Name: "p2", OwnerTenantID: "t2"}), 1100, nil)

	// Raise offline alerts on p-pub and p1a.
	h.putChannel(1150, "t1", "ch1", channel(store.EventProbeOffline), nil)
	h.putChannel(1150, store.SystemTenantID, "ch-sys", channel(store.EventSystem), nil)
	h.do(NewSetSystemAlert("probe_offline", "p-pub", true, 0, nil), 1200, nil)
	h.do(NewSetProbeAlert("t1", "p1a", true, "v1"), 1200, nil)
	dueBefore, _ := h.st.DueAlerts(h.ctx, 10_000, 100)

	// Tenant t1 cannot revoke t2's probe, public probe, voter, or missing node -> CodeNotFound.
	for _, target := range []string{"p2", "p-pub", "v1", "missing"} {
		res := h.apply(withActor(NewRevokeTenantProbe("rev-"+target, "t1", target), user), 1300)
		checkCode(t, res.Err, CodeNotFound)
	}
	// API token cannot revoke tenant probe.
	res := h.apply(withActor(NewRevokeTenantProbe("rev-tok", "t1", "p1a"), &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: "tok_1"}), 1300)
	checkCode(t, res.Err, CodeInvalid)

	// t1 revokes p1a -> succeeds, deletes probe alert silently (no new outbox entries), writes audit.
	h.do(withActor(NewRevokeTenantProbe("rev-p1a", "t1", "p1a"), user), 1400, nil)
	dueAfter, _ := h.st.DueAlerts(h.ctx, 10_000, 100)
	if len(dueAfter) != len(dueBefore) {
		t.Errorf("revoking p1a enqueued notification: before=%d after=%d", len(dueBefore), len(dueAfter))
	}
	if alerts, _ := h.st.ListProbeAlerts(h.ctx, "t1", false); len(alerts) != 0 {
		t.Errorf("probe_alerts not cleared on revoke: %+v", alerts)
	}
	var foundAudit bool
	for _, e := range auditEntries(t, h) {
		if e.TenantID == "t1" && e.Action == "probe.revoke" && e.ObjectID == "p1a" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Error("missing probe.revoke audit entry")
	}

	// Revoking public probe silently clears its system alert without notification.
	h.do(NewRevokeNode("rev-pub", "p-pub"), 1450, nil)
	dueAfterPub, _ := h.st.DueAlerts(h.ctx, 10_000, 100)
	if len(dueAfterPub) != len(dueBefore) {
		t.Errorf("revoking public probe enqueued notification")
	}
	if sysAlerts, _ := h.st.ListSystemAlerts(h.ctx, true); len(sysAlerts) != 0 {
		t.Errorf("public probe system alert still active after revoke: %+v", sysAlerts)
	}

	// DeleteTenant on t1 revokes p1b and keeps p2 intact (ADR 0035 D8).
	h.suspendTenant(1500, "t1", true)
	delCmd := adminCommand("del-t1", actorSystem, &pb.DeleteTenant{TenantId: "t1"})
	delCmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(delCmd, 1600, nil)

	revoked, _ := h.st.ListRevokedNodes(h.ctx)
	revMap := map[string]bool{}
	for _, r := range revoked {
		revMap[r.NodeID] = true
	}
	if !revMap["p1b"] {
		t.Errorf("p1b not revoked after DeleteTenant: %+v", revoked)
	}
	nodes, _ := h.st.ListNodes(h.ctx)
	var hasP1b, hasP2 bool
	for _, n := range nodes {
		if n.ID == "p1b" {
			hasP1b = true
		}
		if n.ID == "p2" {
			hasP2 = true
		}
	}
	if hasP1b || !hasP2 {
		t.Errorf("nodes after DeleteTenant: hasP1b=%v hasP2=%v", hasP1b, hasP2)
	}
}

func TestProbeCommandsAreDeterministic(t *testing.T) {
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-1", Name: "Ada"}
	system := &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	tokCmd, err := NewPutJoinToken("tok-1", "jt-1", JoinTokenDocument{
		SecretHash: pki.HashSecret("sec"), Role: "probe", TenantID: "t1", ExpiresMs: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	tokCmd.Actor = user

	steps := []struct {
		cmd *pb.Command
		at  int64
	}{
		{NewRegisterNode("v1", store.Node{ID: "v1", Role: store.NodeRoleVoter}), 1200},
		{tokCmd, 1300},
		{NewRedeemJoinToken("rd-1", "jt-1", pki.HashSecret("sec"), "p1"), 1400},
		{NewRegisterProbe("rp-1", store.Node{ID: "p1", Name: "edge", OwnerTenantID: "t1", Incarnation: "inc-1"}), 1500},
		{NewSetProbeAlert("t1", "p1", true, "v1"), 2000},
		{NewSetProbeAlert("t1", "p1", false, "v1"), 3000},
		{NewSetProbeAlert("t1", "p1", true, "v1"), 4000},
		{withActor(adminCommand("susp-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}), system), 4500},
		{withActor(adminCommand("del-t1", actorSystem, &pb.DeleteTenant{TenantId: "t1"}), system), 5000},
	}

	var prints []string
	for range 2 {
		h := setupProbeAlertFSM(t, ProbeCommandLevel)
		h.putChannel(1100, "t1", "ch1", channel(store.EventProbeOffline), nil)
		var out strings.Builder
		for _, s := range steps {
			res := h.apply(s.cmd, s.at)
			fmt.Fprintf(&out, "%v|%s\n", res.Err, res.Result)
		}
		fp, err := h.st.Fingerprint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp+"\n"+out.String())
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged:\n%s\nvs\n%s", prints[0], prints[1])
	}
	// Ensure unused time import isn't needed if not used
	_ = time.Second
}

func TestIsNodeIDAllowsUUID(t *testing.T) {
	if errors.Is(ErrInvalidCommand, store.ErrNotFound) {
		t.Fatal("sentinel errors collided")
	}
}

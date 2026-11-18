// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"fmt"
	"reflect"
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/tenant"
)

// withActor sets the actor of cmd and returns cmd.
func withActor(cmd *pb.Command, actor *pb.Actor) *pb.Command {
	cmd.Actor = actor
	return cmd
}

// hinted sets the audit_action of cmd and returns cmd.
func hinted(cmd *pb.Command, hint string) *pb.Command {
	cmd.AuditAction = hint
	return cmd
}

// TestAuditEntryFields checks every field of one audit entry (ADR 0031,
// section 2): index, command time, tenant, actor, action, object and request ID.
func TestAuditEntryFields(t *testing.T) {
	h := tokenFSM(t)
	cmd := withActor(must(NewPutMonitor("req-create-m1", "t1", "m1", monitorDoc("Web m1"), 0)), &pb.Actor{
		Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada",
	})
	res := h.apply(cmd, 4242)
	checkCode(t, res.Err, "")

	var found *store.AuditEntry
	for _, e := range auditEntries(t, h) {
		if e.Index == res.Index {
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatalf("no audit entry at index %d", res.Index)
	}
	want := store.AuditEntry{
		Index: res.Index, Sub: 0, AtMs: 4242, TenantID: "t1",
		ActorKind: "user", ActorID: "u-admin", ActorName: "Ada",
		Action: "monitor.create", ObjectKind: "monitor", ObjectID: "m1", RequestID: "req-create-m1",
	}
	if *found != want {
		t.Errorf("entry = %+v\nwant %+v", *found, want)
	}
}

// TestAuditActions covers the action and object of the representative commands.
// Each step is applied and the newest audit entry is compared with the expected
// values.
func TestAuditActions(t *testing.T) {
	h := tokenFSM(t)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
	system := &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}

	paused := monitorDoc("Web m1")
	paused.Paused = true
	tokenPayload := validTokenPayload(3)
	steps := []struct {
		name   string
		cmd    *pb.Command
		tenant string
		action string
		kind   string
		object string
		actor  string
	}{
		{"monitor create", withActor(must(NewPutMonitor("s1", "t1", "m1", monitorDoc("Web m1"), 0)), user),
			"t1", "monitor.create", "monitor", "m1", "user"},
		{"monitor update", withActor(must(NewPutMonitor("s2", "t1", "m1", monitorDoc("Web m1"), 1)), user),
			"t1", "monitor.update", "monitor", "m1", "user"},
		{"monitor tags hint", hinted(withActor(must(NewPutMonitor("s3", "t1", "m1", monitorDoc("Web m1"), 2)), user), "tags"),
			"t1", "monitor.tags", "monitor", "m1", "user"},
		{"monitor clone hint", hinted(withActor(must(NewPutMonitor("s4", "t1", "m2", monitorDoc("Web m2"), 0)), user), "clone"),
			"t1", "monitor.clone", "monitor", "m2", "user"},
		{"monitor pause hint", hinted(withActor(must(NewPutMonitor("s5", "t1", "m1", paused, 3)), user), "pause"),
			"t1", "monitor.pause", "monitor", "m1", "user"},
		{"monitor resume hint", hinted(withActor(must(NewPutMonitor("s6", "t1", "m1", monitorDoc("Web m1"), 4)), user), "resume"),
			"t1", "monitor.resume", "monitor", "m1", "user"},
		{"monitor delete", withActor(NewDeleteMonitor("s7", "t1", "m2"), user),
			"t1", "monitor.delete", "monitor", "m2", "user"},
		{"channel create", withActor(must(NewPutChannel("s8", "t1", "ch1", channel("down"), 0)), user),
			"t1", "channel.create", "channel", "ch1", "user"},
		{"channel delete", withActor(NewDeleteChannel("s9", "t1", "ch1"), user),
			"t1", "channel.delete", "channel", "ch1", "user"},
		{"api token create", NewCreateAPIToken("s10", "t1", user, tokenPayload),
			"t1", "api_token.create", "api_token", tokID(3), "user"},
		{"api token revoke", NewRevokeAPIToken("s11", "t1", user, tokID(3)),
			"t1", "api_token.revoke", "api_token", tokID(3), "user"},
		{"tenant create", withActor(adminCommand("s12", actorSystem, &pb.CreateTenant{
			TenantId: "t3", Name: "Three", PlanId: "default", Source: tenant.SourceAdmin,
		}), system), store.SystemTenantID, "tenant.create", "tenant", "t3", "system"},
		{"tenant suspend", withActor(adminCommand("s13", actorSystem, &pb.SetTenantSuspended{TenantId: "t3", Suspended: true}), system),
			store.SystemTenantID, "tenant.suspend", "tenant", "t3", "system"},
		{"tenant resume", withActor(adminCommand("s14", actorSystem, &pb.SetTenantSuspended{TenantId: "t3", Suspended: false}), system),
			store.SystemTenantID, "tenant.resume", "tenant", "t3", "system"},
		{"plan put", withActor(adminCommand("s15", actorSystem, &pb.PutPlan{PlanId: "starter", Name: "Starter", LimitsJson: "{}"}), system),
			store.SystemTenantID, "plan.put", "plan", "starter", "system"},
		{"plan delete", withActor(adminCommand("s16", actorSystem, &pb.DeletePlan{PlanId: "starter"}), system),
			store.SystemTenantID, "plan.delete", "plan", "starter", "system"},
		{"node register", withActor(NewRegisterNode("s17", store.Node{ID: "node-1", Role: store.NodeRoleVoter}), system),
			store.SystemTenantID, "node.register", "node", "node-1", "system"},
		{"node revoke", withActor(&pb.Command{RequestId: "s18", Payload: &pb.Command_RevokeNode{RevokeNode: &pb.RevokeNode{NodeId: "node-1"}}}, system),
			store.SystemTenantID, "node.revoke", "node", "node-1", "system"},
		{"feature level", NewSetFeatureLevel("s19", AuditCommandLevel),
			store.SystemTenantID, "cluster.set_feature_level", "cluster", fmt.Sprint(AuditCommandLevel), "unknown"},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			res := h.apply(s.cmd, 5000)
			checkCode(t, res.Err, "")
			entries := auditEntries(t, h)
			if len(entries) == 0 || entries[0].Index != res.Index {
				t.Fatalf("newest audit entry = %+v, want index %d", entries, res.Index)
			}
			e := entries[0]
			if e.Sub != 0 || e.TenantID != s.tenant || e.Action != s.action || e.ObjectKind != s.kind ||
				e.ObjectID != s.object || e.ActorKind != s.actor {
				t.Errorf("entry = %+v, want tenant %s, action %s, object %s/%s, actor %s",
					e, s.tenant, s.action, s.kind, s.object, s.actor)
			}
		})
	}
}

// TestAuditNotAtLowerLevels covers ADR 0031, section 4: below level 9 no audit
// entries are written, not even for user changes.
func TestAuditNotAtLowerLevels(t *testing.T) {
	h := tokenFSMAt(t, AuditCommandLevel-1)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
	res := h.apply(withActor(must(NewPutMonitor("l8", "t1", "m1", monitorDoc("Web m1"), 0)), user), 2000)
	checkCode(t, res.Err, "")
	if n := len(auditEntries(t, h)); n != 0 {
		t.Errorf("audit entries at level %d = %d, want 0", AuditCommandLevel-1, n)
	}
}

// TestAuditNeverPolicies covers the operational commands: they change state,
// but they write no audit entry.
func TestAuditNeverPolicies(t *testing.T) {
	h := batchFSM(t)
	before := len(auditEntries(t, h))
	cmds := []*pb.Command{
		NewCheckpoint(),
		NewRecordIncarnation("node-1", "inc-1"),
		NewPruneAudit(1, 1),
	}
	for _, cmd := range cmds {
		checkCode(t, h.apply(cmd, 3000).Err, "")
	}
	if n := len(auditEntries(t, h)); n != before {
		t.Errorf("audit entries = %d after operational commands, want %d", n, before)
	}
}

// TestAuditNotForRejectedOrReplayed covers the rules "rejected commands write
// nothing" and "a deduplicated replay writes nothing".
func TestAuditNotForRejectedOrReplayed(t *testing.T) {
	h := batchFSM(t)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
	before := len(auditEntries(t, h))

	stale := withActor(must(NewPutMonitor("stale", "t1", "m1", monitorDoc("Web m1"), 9)), user)
	checkCode(t, h.apply(stale, 2000).Err, CodeConflict)
	badToken := NewCreateAPIToken("bad-token", "t1", user, &pb.CreateAPIToken{Id: "nope"})
	checkCode(t, h.apply(badToken, 2000).Err, CodeInvalid)
	if n := len(auditEntries(t, h)); n != before {
		t.Fatalf("audit entries after rejected commands = %d, want %d", n, before)
	}

	first := withActor(must(NewPutMonitor("replay-1", "t1", "m4", monitorDoc("Web m4"), 0)), user)
	res := h.apply(first, 2000)
	checkCode(t, res.Err, "")
	afterFirst := len(auditEntries(t, h))
	if afterFirst != before+1 {
		t.Fatalf("audit entries after the first apply = %d, want %d", afterFirst, before+1)
	}
	replay := withActor(must(NewPutMonitor("replay-1", "t1", "m4", monitorDoc("Web m4"), 0)), user)
	again := h.apply(replay, 2000)
	if !again.Duplicate {
		t.Fatal("replay was not deduplicated")
	}
	if n := len(auditEntries(t, h)); n != afterFirst {
		t.Errorf("audit entries after a replay = %d, want %d", n, afterFirst)
	}
}

// TestAuditBatchEntries covers one entry per operation of a batch, numbered by
// the position, with the shared index and request ID and the hints per operation.
func TestAuditBatchEntries(t *testing.T) {
	h := batchFSM(t)
	actor := &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(1), Name: "ci"}
	paused := monitorDoc("Web m1")
	paused.Paused = true
	ops := []*pb.BatchOp{
		withHint(putOp(pb.EntityKind_ENTITY_KIND_MONITOR, "m1", paused, 1), "pause"),
		monitorOp("m2", 120, 1),
		monitorDeleteOp("m3"),
	}
	res := h.apply(NewBatch("batch-audit", "t1", actor, ops), 2000)
	checkCode(t, res.Err, "")

	got := map[int]store.AuditEntry{}
	for _, e := range auditEntries(t, h) {
		if e.Index == res.Index {
			got[e.Sub] = e
		}
	}
	want := []struct{ action, object string }{
		{"monitor.pause", "m1"}, {"monitor.update", "m2"}, {"monitor.delete", "m3"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries of the batch = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		e, ok := got[i]
		if !ok {
			t.Fatalf("no entry with sub %d", i)
		}
		if e.Action != w.action || e.ObjectID != w.object || e.ObjectKind != "monitor" ||
			e.TenantID != "t1" || e.ActorKind != "api_token" || e.ActorID != tokID(1) ||
			e.RequestID != "batch-audit" || e.AtMs != 2000 {
			t.Errorf("entry %d = %+v, want %s on %s", i, e, w.action, w.object)
		}
	}
}

// TestAuditHints covers the audit_action rules: the known values refine the
// verb of a monitor put, and any other value rejects the command before it
// changes state.
func TestAuditHints(t *testing.T) {
	tests := []struct {
		name string
		cmd  func(*testing.T) *pb.Command
		code string
	}{
		{"unknown hint", func(*testing.T) *pb.Command {
			return hinted(must(NewPutMonitor("h1", "t1", "m1", monitorDoc("Web m1"), 1)), "archive")
		}, CodeInvalid},
		{"hint on a channel", func(*testing.T) *pb.Command {
			return hinted(must(NewPutChannel("h2", "t1", "ch1", channel("down"), 0)), "pause")
		}, CodeInvalid},
		{"hint on a delete", func(*testing.T) *pb.Command {
			return hinted(NewDeleteMonitor("h3", "t1", "m1"), "delete")
		}, CodeInvalid},
		{"hint on a tenant", func(*testing.T) *pb.Command {
			return hinted(adminCommand("h4", actorSystem, &pb.CreateTenant{TenantId: "t4", Name: "Four", PlanId: "default", Source: tenant.SourceAdmin}), "tags")
		}, CodeInvalid},
		{"hint on a batch", func(*testing.T) *pb.Command {
			return hinted(NewBatch("h5", "t1", &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u"}, []*pb.BatchOp{monitorOp("m1", 60, 1)}), "pause")
		}, CodeInvalid},
		{"unknown hint in a batch operation", func(*testing.T) *pb.Command {
			return NewBatch("h6", "t1", &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u"},
				[]*pb.BatchOp{monitorOp("m1", 60, 1), withHint(monitorOp("m1", 60, 1), "archive")})
		}, CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := batchFSM(t)
			before := len(auditEntries(t, h))
			res := h.apply(tt.cmd(t), 2000)
			checkCode(t, res.Err, tt.code)
			if iv := h.intervalOf("m1"); iv != 60 {
				t.Errorf("m1 interval = %d after a rejected hint, want 60", iv)
			}
			if n := len(auditEntries(t, h)); n != before {
				t.Errorf("audit entries = %d after a rejected hint, want %d", n, before)
			}
		})
	}
}

// TestAuditTenantDeleteKeepsSystemEntry covers ADR 0031, section 2: deleting a
// tenant removes its audit entries, and the entry of the deletion itself is
// recorded under the system tenant and survives.
func TestAuditTenantDeleteKeepsSystemEntry(t *testing.T) {
	h := tokenFSM(t)
	system := &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin", Name: "Ada"}
	checkCode(t, h.apply(adminCommand("t3-create", actorSystem, &pb.CreateTenant{
		TenantId: "t3", Name: "Three", PlanId: "default", Source: tenant.SourceAdmin,
	}), 2000).Err, "")
	checkCode(t, h.apply(withActor(must(NewPutMonitor("t3-monitor", "t3", "m1", monitorDoc("Web m1"), 0)), user), 2100).Err, "")
	checkCode(t, h.apply(withActor(adminCommand("t3-suspend", actorSystem, &pb.SetTenantSuspended{TenantId: "t3", Suspended: true}), system), 2200).Err, "")
	res := h.apply(withActor(adminCommand("t3-delete", actorSystem, &pb.DeleteTenant{TenantId: "t3"}), system), 2300)
	checkCode(t, res.Err, "")

	var inT3, deletes int
	for _, e := range auditEntries(t, h) {
		if e.TenantID == "t3" {
			inT3++
		}
		if e.Action == "tenant.delete" && e.ObjectID == "t3" && e.TenantID == store.SystemTenantID && e.Index == res.Index {
			deletes++
		}
	}
	if inT3 != 0 {
		t.Errorf("entries of the deleted tenant = %d, want 0", inT3)
	}
	if deletes != 1 {
		t.Errorf("tenant.delete entries under %s = %d, want 1", store.SystemTenantID, deletes)
	}
}

// TestAuditPolicyCoversEveryPayload fails for a payload of the Command oneof
// without an audit policy, the same way TestMinVersionCoversEveryPayload does
// for versions (ADR 0031, section 2).
func TestAuditPolicyCoversEveryPayload(t *testing.T) {
	oneof := (&pb.Command{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")
	if oneof == nil {
		t.Fatal("Command has no payload oneof")
	}
	fields := oneof.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		t.Run(string(fd.Name()), func(t *testing.T) {
			m := (&pb.Command{}).ProtoReflect()
			m.Set(fd, m.NewField(fd))
			cmd := m.Interface().(*pb.Command)
			if _, ok := auditPolicies[reflect.TypeOf(cmd.GetPayload())]; !ok {
				t.Errorf("payload %s has no audit policy", fd.Name())
			}
		})
	}
	if n := len(auditPolicies); n != fields.Len() {
		t.Errorf("audit policies = %d, payloads = %d; remove the entries of unknown payloads", n, fields.Len())
	}
}

// TestLevel9RulesOffBelowLevel9 covers ADR 0018: binaries of level 8 do not
// know audit_action and the token actor rules and apply such commands, so a
// level-9 binary must do the same below level 9, or mixed voters diverge.
func TestLevel9RulesOffBelowLevel9(t *testing.T) {
	h := tokenFSMAt(t, AuditCommandLevel-1)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-admin"}
	res := h.apply(hinted(withActor(must(NewPutMonitor("l8h", "t1", "m1", monitorDoc("Web m1"), 0)), user), "archive"), 2000)
	checkCode(t, res.Err, "")
	token := &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: "tok_x"}
	// At level 9 this is CodeInvalid (checkEntityActor); below it the delete
	// runs and finds no user.
	res = h.apply(withActor(NewDeleteUser("l8u", "t1", "nobody"), token), 2100)
	if res.Err == nil {
		t.Fatal("delete of a missing user succeeded")
	}
	checkCode(t, res.Err, CodeNotFound)
	if n := len(auditEntries(t, h)); n != 0 {
		t.Errorf("audit entries at level %d = %d, want 0", AuditCommandLevel-1, n)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/tenant"
)

// Actors of the tenant admin tests (ADR 0029).
const (
	actorSystem = pb.Actor_KIND_SYSTEM
	actorUser   = pb.Actor_KIND_USER
	actorToken  = pb.Actor_KIND_API_TOKEN
	actorNone   = pb.Actor_KIND_UNSPECIFIED
)

// adminCommand builds one of the six tenant and plan commands of ADR 0029. The
// payload is a *pb.CreateTenant, *pb.UpdateTenant, *pb.SetTenantSuspended,
// *pb.DeleteTenant, *pb.PutPlan or *pb.DeletePlan. There are no production
// constructors yet, so the tests build the commands here.
func adminCommand(requestID string, actor pb.Actor_Kind, payload any) *pb.Command {
	cmd := &pb.Command{RequestId: requestID, Actor: &pb.Actor{Kind: actor}}
	switch p := payload.(type) {
	case *pb.CreateTenant:
		cmd.Payload = &pb.Command_CreateTenant{CreateTenant: p}
	case *pb.UpdateTenant:
		cmd.Payload = &pb.Command_UpdateTenant{UpdateTenant: p}
	case *pb.SetTenantSuspended:
		cmd.Payload = &pb.Command_SetTenantSuspended{SetTenantSuspended: p}
	case *pb.DeleteTenant:
		cmd.Payload = &pb.Command_DeleteTenant{DeleteTenant: p}
	case *pb.PutPlan:
		cmd.Payload = &pb.Command_PutPlan{PutPlan: p}
	case *pb.DeletePlan:
		cmd.Payload = &pb.Command_DeletePlan{DeletePlan: p}
	default:
		panic(fmt.Sprintf("adminCommand: unsupported payload %T", payload))
	}
	return stamp(cmd)
}

// checkCode fails the test unless err is a CommandError with the code; an empty
// code means success.
func checkCode(t *testing.T, err error, code string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v, want code %q", err, code)
	}
}

// adminDo applies a tenant or plan command and checks its outcome.
func (h *alertFSM) adminDo(issuedAt int64, requestID string, actor pb.Actor_Kind, payload any, code string) ApplyResult {
	h.t.Helper()
	res := h.apply(adminCommand(requestID, actor, payload), issuedAt)
	checkCode(h.t, res.Err, code)
	return res
}

// putTenantDoc applies a PutEntity for a tenant with the given actor.
func (h *alertFSM) putTenantDoc(issuedAt int64, requestID, id string, doc TenantDocument, actor pb.Actor_Kind, code string) {
	h.t.Helper()
	cmd, err := NewPutTenant(requestID, id, doc)
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Actor = &pb.Actor{Kind: actor}
	checkCode(h.t, h.apply(cmd, issuedAt).Err, code)
}

// putMonitorAt creates (rev 0) or replaces (rev = current revision) a monitor
// with the given interval and checks the outcome.
func (h *alertFSM) putMonitorAt(issuedAt int64, requestID, tenantID, id string, interval int, rev uint64, code string) {
	h.t.Helper()
	doc := monitorDoc("Web " + id)
	doc.IntervalSeconds = interval
	cmd, err := NewPutMonitor(requestID, tenantID, id, doc, rev)
	if err != nil {
		h.t.Fatal(err)
	}
	checkCode(h.t, h.apply(cmd, issuedAt).Err, code)
}

// putChannelAt creates (rev 0) or replaces (rev = current revision) a webhook
// channel subscribed to events and checks the outcome.
func (h *alertFSM) putChannelAt(issuedAt int64, requestID, tenantID, id string, events []string, rev uint64, code string) {
	h.t.Helper()
	cmd, err := NewPutChannel(requestID, tenantID, id, channel(events...), rev)
	if err != nil {
		h.t.Fatal(err)
	}
	checkCode(h.t, h.apply(cmd, issuedAt).Err, code)
}

// userWithSession creates a user of the tenant with one refresh session and
// returns the token hash of the session.
func (h *alertFSM) userWithSession(issuedAt int64, tenantID, userID string) string {
	h.t.Helper()
	cmd, err := NewPutUser("user-"+tenantID+"-"+userID, tenantID, userID, userDoc(userID, "tenant-viewer"), 0)
	if err != nil {
		h.t.Fatal(err)
	}
	checkCode(h.t, h.apply(cmd, issuedAt).Err, "")
	hash := sha256Hex("token-" + tenantID + "-" + userID)
	sess := NewCreateSession("session-"+tenantID+"-"+userID, tenantID, userID, hash, "family-"+tenantID+"-"+userID, 1<<40)
	checkCode(h.t, h.apply(sess, issuedAt).Err, "")
	return hash
}

// session reads a refresh session by its token hash or fails the test.
func (h *alertFSM) session(hash string) store.Session {
	h.t.Helper()
	s, err := h.st.GetSession(h.ctx, hash)
	if err != nil {
		h.t.Fatalf("GetSession: %v", err)
	}
	return s
}

// plan reads a plan or fails the test.
func (h *alertFSM) plan(id string) store.Plan {
	h.t.Helper()
	p, err := h.st.GetPlan(h.ctx, id)
	if err != nil {
		h.t.Fatalf("GetPlan(%s): %v", id, err)
	}
	return p
}

// monitorInterval returns the stored interval of a monitor or fails the test.
func (h *alertFSM) monitorInterval(tenantID, id string) int {
	h.t.Helper()
	m, err := h.st.GetMonitor(h.ctx, tenantID, id)
	if err != nil {
		h.t.Fatalf("GetMonitor(%s, %s): %v", tenantID, id, err)
	}
	return m.IntervalSeconds
}

// wantAbsentTenant checks that the tenant does not exist.
func (h *alertFSM) wantAbsentTenant(id string) {
	h.t.Helper()
	if _, err := h.st.GetTenant(h.ctx, id); !errors.Is(err, store.ErrNotFound) {
		h.t.Errorf("GetTenant(%s) err = %v, want ErrNotFound", id, err)
	}
}

// wantAbsentMonitor checks that the monitor does not exist.
func (h *alertFSM) wantAbsentMonitor(tenantID, id string) {
	h.t.Helper()
	if _, err := h.st.GetMonitor(h.ctx, tenantID, id); !errors.Is(err, store.ErrNotFound) {
		h.t.Errorf("GetMonitor(%s, %s) err = %v, want ErrNotFound", tenantID, id, err)
	}
}

// catalogJSON returns the tenants, plans and tombstones as JSON. A rejected
// command must leave it unchanged.
func (h *alertFSM) catalogJSON() string {
	h.t.Helper()
	tenants, err := h.st.ListTenants(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	plans, err := h.st.ListPlans(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	deleted, err := h.st.ListDeletedTenants(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	b, err := json.Marshal(struct {
		Tenants []store.Tenant
		Plans   []store.Plan
		Deleted []store.DeletedTenant
	}{tenants, plans, deleted})
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// adminFSM returns an FSM at command level 8 with:
//   - the system tenant and the default tenant bound to org-platform;
//   - the plan starter (maxMonitors 2, minIntervalSeconds 60, maxChannels 1)
//     and the plan pro (no limits);
//   - the tenant t1 on starter, bound to org-1 and created by the system.
func adminFSM(t *testing.T) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(TenantAdminCommandLevel)
	h.putSystemTenant(1000)
	h.putTenantDoc(1000, "tenant-default", tenant.DefaultTenantID,
		TenantDocument{Name: "Default", Plan: tenant.DefaultPlanID, ZitadelOrgID: "org-platform"}, actorSystem, "")
	h.adminDo(1000, "plan-starter", actorSystem, &pb.PutPlan{PlanId: "starter", Name: "Starter",
		LimitsJson: `{"maxMonitors":2,"minIntervalSeconds":60,"maxChannels":1}`}, "")
	h.adminDo(1000, "plan-pro", actorSystem, &pb.PutPlan{PlanId: "pro", Name: "Pro", LimitsJson: "{}"}, "")
	h.adminDo(1000, "create-t1", actorSystem, &pb.CreateTenant{TenantId: "t1", Name: "Tenant One",
		PlanId: "starter", OrgId: "org-1", Source: tenant.SourceAdmin}, "")
	return h
}

// createTenantPayload returns a CreateTenant of t2 on pro, bound to org-2.
func createTenantPayload(source string) *pb.CreateTenant {
	return &pb.CreateTenant{TenantId: "t2", Name: "Two", PlanId: "pro", OrgId: "org-2", Source: source}
}

// TestTenantAdminNeedsLevel8 covers ADR 0029 and ADR 0018: below command level 8
// all six commands are rejected with CodeUnsupported and nothing is written.
func TestTenantAdminNeedsLevel8(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload any
	}{
		{"CreateTenant", createTenantPayload(tenant.SourceAdmin)},
		{"UpdateTenant", &pb.UpdateTenant{TenantId: "t1", Name: proto.String("Renamed")}},
		{"SetTenantSuspended", &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}},
		{"DeleteTenant", &pb.DeleteTenant{TenantId: "t1"}},
		{"PutPlan", &pb.PutPlan{PlanId: "starter", Name: "Starter", LimitsJson: `{"maxMonitors":2}`}},
		{"DeletePlan", &pb.DeletePlan{PlanId: "default"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newAlertFSM(t, t.TempDir())
			h.setLevel(TenantAdminCommandLevel - 1)
			h.putSystemTenant(1000)
			h.putTenant(1000, "t1")
			before := h.catalogJSON()
			h.adminDo(2000, "level-"+tt.name, actorSystem, tt.payload, CodeUnsupported)
			if after := h.catalogJSON(); after != before {
				t.Errorf("rejected command changed the catalog:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// TestTenantAdminActors covers the actor rule of ADR 0029: only users and the
// system may run the tenant and plan commands, only the system provisions
// tenants automatically, and a rejected command writes nothing.
func TestTenantAdminActors(t *testing.T) {
	t.Parallel()
	suspendT1 := func(h *alertFSM) {
		h.adminDo(1500, "pre-suspend-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
	}
	auto := func() *pb.CreateTenant {
		return &pb.CreateTenant{TenantId: "a1", Name: "Auto", PlanId: "pro", OrgId: "org-9", Source: tenant.SourceAuto, MaxAutoTenants: 5}
	}
	tests := []struct {
		name    string
		actor   pb.Actor_Kind
		pre     func(*alertFSM)
		payload any
		code    string
	}{
		{"unspecified actor cannot create a tenant", actorNone, nil, createTenantPayload(tenant.SourceAdmin), CodeInvalid},
		{"API token cannot create a tenant", actorToken, nil, createTenantPayload(tenant.SourceAdmin), CodeInvalid},
		{"API token cannot create an automatic tenant", actorToken, nil, auto(), CodeInvalid},
		{"API token cannot update a tenant", actorToken, nil, &pb.UpdateTenant{TenantId: "t1", Name: proto.String("Renamed")}, CodeInvalid},
		{"API token cannot suspend a tenant", actorToken, nil, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, CodeInvalid},
		{"API token cannot delete a tenant", actorToken, suspendT1, &pb.DeleteTenant{TenantId: "t1"}, CodeInvalid},
		{"API token cannot put a plan", actorToken, nil, &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: "{}"}, CodeInvalid},
		{"API token cannot delete a plan", actorToken, nil, &pb.DeletePlan{PlanId: "pro"}, CodeInvalid},
		{"user creates an administered tenant", actorUser, nil, createTenantPayload(tenant.SourceAdmin), ""},
		{"system creates an administered tenant", actorSystem, nil, createTenantPayload(tenant.SourceAdmin), ""},
		{"user cannot create an automatic tenant", actorUser, nil, auto(), CodeInvalid},
		{"system creates an automatic tenant", actorSystem, nil, auto(), ""},
		{"user updates a tenant", actorUser, nil, &pb.UpdateTenant{TenantId: "t1", Name: proto.String("Renamed")}, ""},
		{"user suspends a tenant", actorUser, nil, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, ""},
		{"user deletes a suspended tenant", actorUser, suspendT1, &pb.DeleteTenant{TenantId: "t1"}, ""},
		{"user puts a plan", actorUser, nil, &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: "{}"}, ""},
		{"user deletes an unused plan", actorUser, nil, &pb.DeletePlan{PlanId: "pro"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			if tt.pre != nil {
				tt.pre(h)
			}
			before := h.catalogJSON()
			h.adminDo(2000, "cmd", tt.actor, tt.payload, tt.code)
			if tt.code != "" {
				if after := h.catalogJSON(); after != before {
					t.Errorf("rejected command changed the catalog:\nbefore %s\nafter  %s", before, after)
				}
			}
		})
	}
}

// TestCreateTenantRules covers the validation of CreateTenant in the order of
// the FSM: IDs, names, plans, overrides, organizations.
func TestCreateTenantRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(c *pb.CreateTenant)
		code string
	}{
		{"valid tenant", func(*pb.CreateTenant) {}, ""},
		{"uppercase ID", func(c *pb.CreateTenant) { c.TenantId = "T2" }, CodeInvalid},
		{"reserved system ID", func(c *pb.CreateTenant) { c.TenantId = store.SystemTenantID }, CodeInvalid},
		{"empty ID", func(c *pb.CreateTenant) { c.TenantId = "" }, CodeInvalid},
		{"65 character ID", func(c *pb.CreateTenant) { c.TenantId = strings.Repeat("a", 65) }, CodeInvalid},
		{"64 character ID", func(c *pb.CreateTenant) { c.TenantId = strings.Repeat("a", 64) }, ""},
		{"empty name", func(c *pb.CreateTenant) { c.Name = "" }, CodeInvalid},
		{"101 character name", func(c *pb.CreateTenant) { c.Name = strings.Repeat("n", 101) }, CodeInvalid},
		{"100 character name", func(c *pb.CreateTenant) { c.Name = strings.Repeat("n", 100) }, ""},
		{"name with leading space", func(c *pb.CreateTenant) { c.Name = " Two" }, CodeInvalid},
		{"name with control character", func(c *pb.CreateTenant) { c.Name = "Tw\to" }, CodeInvalid},
		{"name with line separator", func(c *pb.CreateTenant) { c.Name = "Two\u2028Three" }, CodeInvalid},
		{"name with format character", func(c *pb.CreateTenant) { c.Name = "Two\u200bThree" }, CodeInvalid},
		{"unknown plan", func(c *pb.CreateTenant) { c.PlanId = "gold" }, CodeInvalid},
		{"system plan", func(c *pb.CreateTenant) { c.PlanId = tenant.SystemPlanID }, CodeInvalid},
		{"overrides with unknown field", func(c *pb.CreateTenant) { c.LimitsJson = `{"maxUsers":3}` }, CodeInvalid},
		{"negative override", func(c *pb.CreateTenant) { c.LimitsJson = `{"maxMonitors":-1}` }, CodeInvalid},
		{"override as string", func(c *pb.CreateTenant) { c.LimitsJson = `{"maxMonitors":"5"}` }, CodeInvalid},
		{"override above maximum", func(c *pb.CreateTenant) { c.LimitsJson = `{"maxMonitors":1000001}` }, CodeInvalid},
		{"valid overrides", func(c *pb.CreateTenant) { c.LimitsJson = `{"maxMonitors":5}` }, ""},
		{"invalid organization", func(c *pb.CreateTenant) { c.OrgId = "org 2" }, CodeInvalid},
		{"organization of another tenant", func(c *pb.CreateTenant) { c.OrgId = "org-1" }, CodeConflict},
		{"existing tenant ID", func(c *pb.CreateTenant) { c.TenantId = "t1" }, CodeConflict},
		{"empty source", func(c *pb.CreateTenant) { c.Source = "" }, CodeInvalid},
		{"bootstrap source", func(c *pb.CreateTenant) { c.Source = tenant.SourceBootstrap }, CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			c := createTenantPayload(tenant.SourceAdmin)
			tt.edit(c)
			before := h.catalogJSON()
			h.adminDo(2000, "create", actorSystem, c, tt.code)
			if tt.code != "" {
				if after := h.catalogJSON(); after != before {
					t.Errorf("rejected command changed the catalog:\nbefore %s\nafter  %s", before, after)
				}
			}
		})
	}
}

// TestCreateTenantStoresFields checks what a successful CreateTenant stores.
func TestCreateTenantStoresFields(t *testing.T) {
	t.Parallel()
	h := adminFSM(t)
	res := h.adminDo(4242, "create-t2", actorSystem, &pb.CreateTenant{TenantId: "t2", Name: "Two", PlanId: "pro",
		OrgId: "org-2", LimitsJson: `{"maxMonitors":5,"maxChannels":null}`, Source: tenant.SourceAdmin}, "")
	if got := string(res.Result); got != `{"tenant_id":"t2"}` {
		t.Errorf("result = %s, want {\"tenant_id\":\"t2\"}", got)
	}
	got := h.tenantOrg("t2")
	if got.Name != "Two" || got.Plan != "pro" || got.ZitadelOrgID != "org-2" || got.Source != tenant.SourceAdmin || got.Suspended {
		t.Errorf("tenant = %+v", got)
	}
	if string(got.Limits) != `{"maxChannels":null,"maxMonitors":5}` {
		t.Errorf("limits = %s, want canonical overrides", got.Limits)
	}
	if got.CreatedMs != 4242 || got.UpdatedMs != 4242 {
		t.Errorf("created = %d, updated = %d; want both 4242", got.CreatedMs, got.UpdatedMs)
	}

	h.adminDo(4243, "create-t3", actorSystem, &pb.CreateTenant{TenantId: "t3", Name: "Three", PlanId: "pro", OrgId: "org-3", Source: tenant.SourceAdmin}, "")
	if l := string(h.tenantOrg("t3").Limits); l != "{}" {
		t.Errorf("limits without overrides = %s, want {}", l)
	}
}

// TestCreateTenantTombstone covers ADR 0029: the ID of a deleted tenant is never
// reused, but its organization is free again.
func TestCreateTenantTombstone(t *testing.T) {
	t.Parallel()
	h := adminFSM(t)
	h.adminDo(1100, "create-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
	h.adminDo(1200, "suspend-t2", actorSystem, &pb.SetTenantSuspended{TenantId: "t2", Suspended: true}, "")
	h.adminDo(1300, "delete-t2", actorSystem, &pb.DeleteTenant{TenantId: "t2"}, "")

	h.adminDo(1400, "recreate-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), CodeConflict)
	h.wantAbsentTenant("t2")
	h.adminDo(1500, "create-t3", actorSystem, &pb.CreateTenant{TenantId: "t3", Name: "Three", PlanId: "pro", OrgId: "org-2", Source: tenant.SourceAdmin}, "")
	if got := h.tenantOrg("t3").ZitadelOrgID; got != "org-2" {
		t.Errorf("t3 org = %q, want org-2 (freed by the deletion)", got)
	}
}

// TestAutoProvisioning covers the automatic creation of tenants (ADR 0029, 4A).
func TestAutoProvisioning(t *testing.T) {
	t.Parallel()
	autoPayload := func(id, org string, maxTenants uint32) *pb.CreateTenant {
		return &pb.CreateTenant{TenantId: id, Name: "Org " + org, PlanId: "pro", OrgId: org, Source: tenant.SourceAuto, MaxAutoTenants: maxTenants}
	}

	t.Run("bound organization returns its tenant and creates nothing", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		before := h.catalogJSON()
		res := h.adminDo(2000, "auto-existing", actorSystem, autoPayload("p1", "org-1", 10), "")
		if got := string(res.Result); got != `{"tenant_id":"t1","existing":true}` {
			t.Errorf("result = %s, want the existing tenant t1", got)
		}
		if after := h.catalogJSON(); after != before {
			t.Errorf("catalog changed:\nbefore %s\nafter  %s", before, after)
		}
		h.wantAbsentTenant("p1")
	})

	t.Run("parallel first logins end at the first tenant", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		r1 := h.adminDo(2000, "prov-1", actorSystem, autoPayload("p1", "org-7", 10), "")
		r2 := h.adminDo(2001, "prov-2", actorSystem, autoPayload("p2", "org-7", 10), "")
		if got := string(r1.Result); got != `{"tenant_id":"p1"}` {
			t.Errorf("first result = %s, want p1", got)
		}
		if got := string(r2.Result); got != `{"tenant_id":"p1","existing":true}` {
			t.Errorf("second result = %s, want p1 existing", got)
		}
		h.wantAbsentTenant("p2")
	})

	t.Run("the maximum counts automatic tenants only", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "auto-a1", actorSystem, autoPayload("a1", "org-a", 1), "")
		h.adminDo(2001, "auto-a2", actorSystem, autoPayload("a2", "org-b", 1), CodeLimitExceeded)
		h.wantAbsentTenant("a2")
		// t1 is an administered tenant and does not count; administered tenants are not capped.
		h.adminDo(2002, "admin-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
	})

	t.Run("a maximum of zero admits no automatic tenant", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "auto-zero", actorSystem, autoPayload("a1", "org-a", 0), CodeLimitExceeded)
		h.wantAbsentTenant("a1")
	})

	rejections := []struct {
		name  string
		actor pb.Actor_Kind
		edit  func(c *pb.CreateTenant)
		code  string
	}{
		{"missing organization", actorSystem, func(c *pb.CreateTenant) { c.OrgId = "" }, CodeInvalid},
		{"invalid organization", actorSystem, func(c *pb.CreateTenant) { c.OrgId = "org 1" }, CodeInvalid},
		{"user cannot provision", actorUser, func(*pb.CreateTenant) {}, CodeInvalid},
		{"API token cannot provision", actorToken, func(*pb.CreateTenant) {}, CodeInvalid},
		{"existing tenant ID with a free organization", actorSystem, func(c *pb.CreateTenant) { c.TenantId = "t1" }, CodeConflict},
		{"unknown plan", actorSystem, func(c *pb.CreateTenant) { c.PlanId = "gold" }, CodeInvalid},
		{"system plan", actorSystem, func(c *pb.CreateTenant) { c.PlanId = tenant.SystemPlanID }, CodeInvalid},
		{"invalid name", actorSystem, func(c *pb.CreateTenant) { c.Name = "" }, CodeInvalid},
	}
	for _, tt := range rejections {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			c := autoPayload("p1", "org-free", 10)
			tt.edit(c)
			before := h.catalogJSON()
			h.adminDo(2000, "auto", tt.actor, c, tt.code)
			if after := h.catalogJSON(); after != before {
				t.Errorf("rejected command changed the catalog:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// TestPutEntityTenantAtLevel8 covers ADR 0029 for PutEntity on tenants from
// command level 8: only the system creates tenants that way, the plan must be
// assignable, and the organization is validated and must be free.
func TestPutEntityTenantAtLevel8(t *testing.T) {
	t.Parallel()
	h := adminFSM(t)
	doc := TenantDocument{Name: "Five", Plan: "pro"}
	h.putTenantDoc(2000, "put-t5-user", "t5", doc, actorUser, CodeInvalid)
	h.putTenantDoc(2000, "put-t5-unspecified", "t5", doc, actorNone, CodeInvalid)
	h.putTenantDoc(2000, "put-t5-bad-org", "t5", TenantDocument{Name: "Five", Plan: "pro", ZitadelOrgID: "bad org"}, actorSystem, CodeInvalid)
	h.putTenantDoc(2000, "put-t5-taken", "t5", TenantDocument{Name: "Five", Plan: "pro", ZitadelOrgID: "org-1"}, actorSystem, CodeConflict)
	h.putTenantDoc(2000, "put-t5-system-plan", "t5", TenantDocument{Name: "Five", Plan: "system"}, actorSystem, CodeInvalid)
	h.putTenantDoc(2000, "put-t5-unknown-plan", "t5", TenantDocument{Name: "Five", Plan: "gold"}, actorSystem, CodeInvalid)
	h.wantAbsentTenant("t5")

	h.putTenantDoc(2001, "put-t5", "t5", TenantDocument{Name: "Five", Plan: "pro", ZitadelOrgID: "org-5"}, actorSystem, "")
	if org, err := h.st.GetTenantByOrg(h.ctx, "org-5"); err != nil || org.ID != "t5" {
		t.Errorf("GetTenantByOrg(org-5) = %+v, %v; want t5", org, err)
	}
	h.putTenantDoc(2002, "put-t6", "t6", TenantDocument{Name: "Six", Plan: "pro"}, actorSystem, "")
	// A tenant may be put again with its own organization.
	h.putTenantDoc(2003, "put-t5-again", "t5", TenantDocument{Name: "Five", Plan: "pro", ZitadelOrgID: "org-5"}, actorSystem, "")
}

// TestUpdateTenant covers UpdateTenant: set fields only, the protected tenants,
// rebinding and unbinding of organizations.
func TestUpdateTenant(t *testing.T) {
	t.Parallel()
	withT2 := func(h *alertFSM) {
		h.adminDo(1100, "create-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
	}
	tests := []struct {
		name    string
		setup   func(*alertFSM)
		payload *pb.UpdateTenant
		code    string
		check   func(*alertFSM)
	}{
		{
			name:    "name only keeps the other fields",
			payload: &pb.UpdateTenant{TenantId: "t1", Name: proto.String("Renamed")},
			check: func(h *alertFSM) {
				tn := h.tenantOrg("t1")
				if tn.Name != "Renamed" || tn.Plan != "starter" || tn.ZitadelOrgID != "org-1" || string(tn.Limits) != "{}" {
					t.Errorf("tenant = %+v; only the name may change", tn)
				}
				if tn.CreatedMs != 1000 || tn.UpdatedMs != 2000 {
					t.Errorf("created = %d, updated = %d; want 1000 and 2000", tn.CreatedMs, tn.UpdatedMs)
				}
			},
		},
		{
			name:    "plan only",
			payload: &pb.UpdateTenant{TenantId: "t1", PlanId: proto.String("pro")},
			check: func(h *alertFSM) {
				if tn := h.tenantOrg("t1"); tn.Plan != "pro" || tn.Name != "Tenant One" {
					t.Errorf("tenant = %+v; want plan pro and the name kept", tn)
				}
			},
		},
		{
			name:    "limits only",
			payload: &pb.UpdateTenant{TenantId: "t1", LimitsJson: proto.String(`{"maxMonitors":7}`)},
			check: func(h *alertFSM) {
				if got := string(h.tenantOrg("t1").Limits); got != `{"maxMonitors":7}` {
					t.Errorf("limits = %s", got)
				}
			},
		},
		{
			name:    "explicit null override is kept canonically",
			payload: &pb.UpdateTenant{TenantId: "t1", LimitsJson: proto.String(`{"maxMonitors":null,"maxChannels":3}`)},
			check: func(h *alertFSM) {
				if got := string(h.tenantOrg("t1").Limits); got != `{"maxChannels":3,"maxMonitors":null}` {
					t.Errorf("limits = %s", got)
				}
			},
		},
		{
			name: "empty overrides remove them",
			setup: func(h *alertFSM) {
				h.adminDo(1500, "limits-first", actorSystem, &pb.UpdateTenant{TenantId: "t1", LimitsJson: proto.String(`{"maxMonitors":7}`)}, "")
			},
			payload: &pb.UpdateTenant{TenantId: "t1", LimitsJson: proto.String("{}")},
			check: func(h *alertFSM) {
				if got := string(h.tenantOrg("t1").Limits); got != "{}" {
					t.Errorf("limits = %s, want {}", got)
				}
			},
		},
		{name: "unknown plan", payload: &pb.UpdateTenant{TenantId: "t1", PlanId: proto.String("gold")}, code: CodeInvalid},
		{name: "system plan", payload: &pb.UpdateTenant{TenantId: "t1", PlanId: proto.String(tenant.SystemPlanID)}, code: CodeInvalid},
		{name: "empty name", payload: &pb.UpdateTenant{TenantId: "t1", Name: proto.String("")}, code: CodeInvalid},
		{name: "invalid limits", payload: &pb.UpdateTenant{TenantId: "t1", LimitsJson: proto.String(`{"maxUsers":1}`)}, code: CodeInvalid},
		{name: "system tenant is reserved", payload: &pb.UpdateTenant{TenantId: store.SystemTenantID, Name: proto.String("x")}, code: CodeInvalid},
		{name: "unknown tenant", payload: &pb.UpdateTenant{TenantId: "ghost", Name: proto.String("x")}, code: CodeNotFound},
		{
			name:    "unbinding removes the organization",
			payload: &pb.UpdateTenant{TenantId: "t1", OrgId: proto.String("")},
			check: func(h *alertFSM) {
				if got := h.tenantOrg("t1").ZitadelOrgID; got != "" {
					t.Errorf("org = %q, want unbound", got)
				}
				if _, err := h.st.GetTenantByOrg(h.ctx, "org-1"); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("GetTenantByOrg(org-1) err = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name:    "rebinding to a free organization",
			payload: &pb.UpdateTenant{TenantId: "t1", OrgId: proto.String("org-3")},
			check: func(h *alertFSM) {
				if got := h.tenantOrg("t1").ZitadelOrgID; got != "org-3" {
					t.Errorf("org = %q, want org-3", got)
				}
			},
		},
		{
			name:    "rebinding to the organization of another tenant conflicts",
			setup:   withT2,
			payload: &pb.UpdateTenant{TenantId: "t1", OrgId: proto.String("org-2")},
			code:    CodeConflict,
			check: func(h *alertFSM) {
				if got := h.tenantOrg("t1").ZitadelOrgID; got != "org-1" {
					t.Errorf("org = %q, want org-1 kept", got)
				}
			},
		},
		{
			name:    "the same organization is accepted",
			payload: &pb.UpdateTenant{TenantId: "t1", OrgId: proto.String("org-1")},
			check: func(h *alertFSM) {
				if got := h.tenantOrg("t1").ZitadelOrgID; got != "org-1" {
					t.Errorf("org = %q, want org-1", got)
				}
			},
		},
		{name: "invalid organization", payload: &pb.UpdateTenant{TenantId: "t1", OrgId: proto.String("org 3")}, code: CodeInvalid},
		{
			name:    "default tenant keeps its organization while name, plan and limits change",
			payload: &pb.UpdateTenant{TenantId: tenant.DefaultTenantID, Name: proto.String("Renamed"), PlanId: proto.String("pro"), LimitsJson: proto.String(`{"maxMonitors":9}`)},
			check: func(h *alertFSM) {
				tn := h.tenantOrg(tenant.DefaultTenantID)
				if tn.Name != "Renamed" || tn.Plan != "pro" || string(tn.Limits) != `{"maxMonitors":9}` || tn.ZitadelOrgID != "org-platform" {
					t.Errorf("default tenant = %+v", tn)
				}
			},
		},
		{
			name:    "default tenant cannot be rebound",
			payload: &pb.UpdateTenant{TenantId: tenant.DefaultTenantID, OrgId: proto.String("org-other")},
			code:    CodeInvalid,
		},
		{
			name:    "default tenant cannot be unbound",
			payload: &pb.UpdateTenant{TenantId: tenant.DefaultTenantID, OrgId: proto.String("")},
			code:    CodeInvalid,
		},
		{
			name:    "default tenant with its own organization again is accepted",
			payload: &pb.UpdateTenant{TenantId: tenant.DefaultTenantID, OrgId: proto.String("org-platform")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			if tt.setup != nil {
				tt.setup(h)
			}
			before := h.catalogJSON()
			h.adminDo(2000, "update", actorSystem, tt.payload, tt.code)
			if tt.code != "" {
				if after := h.catalogJSON(); after != before {
					t.Errorf("rejected command changed the catalog:\nbefore %s\nafter  %s", before, after)
				}
			}
			if tt.check != nil {
				tt.check(h)
			}
		})
	}
}

// TestSetTenantSuspended covers ADR 0029: suspending revokes the refresh
// sessions of that tenant only, and the command is idempotent.
func TestSetTenantSuspended(t *testing.T) {
	t.Parallel()
	for _, id := range []string{tenant.DefaultTenantID, store.SystemTenantID} {
		t.Run("protected "+id+" cannot be suspended", func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			h.adminDo(2000, "suspend", actorSystem, &pb.SetTenantSuspended{TenantId: id, Suspended: true}, CodeInvalid)
		})
	}
	t.Run("unknown tenant", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "suspend-ghost", actorSystem, &pb.SetTenantSuspended{TenantId: "ghost", Suspended: true}, CodeNotFound)
	})

	t.Run("revokes the sessions of the tenant only", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(1100, "create-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
		alice := h.userWithSession(1200, "t1", "alice")
		bob := h.userWithSession(1201, "t2", "bob")

		h.adminDo(2000, "suspend-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
		if tn := h.tenantOrg("t1"); !tn.Suspended || tn.UpdatedMs != 2000 {
			t.Errorf("t1 = %+v; want suspended at 2000", tn)
		}
		if got := h.session(alice).RevokedMs; got != 2000 {
			t.Errorf("alice's session revoked at %d, want 2000", got)
		}
		if got := h.session(bob).RevokedMs; got != 0 {
			t.Errorf("bob's session of t2 revoked at %d, want not revoked", got)
		}

		// Suspending again changes nothing, so the update time stays.
		h.adminDo(3000, "suspend-t1-again", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
		if got := h.tenantOrg("t1").UpdatedMs; got != 2000 {
			t.Errorf("updated = %d after a repeated suspension, want 2000", got)
		}

		// Resuming does not restore the revoked sessions.
		h.adminDo(4000, "resume-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: false}, "")
		if tn := h.tenantOrg("t1"); tn.Suspended || tn.UpdatedMs != 4000 {
			t.Errorf("t1 = %+v; want resumed at 4000", tn)
		}
		if got := h.session(alice).RevokedMs; got != 2000 {
			t.Errorf("alice's session revoked at %d after resume, want still 2000", got)
		}
	})
}

// TestDeleteTenant covers ADR 0029 (5A): a suspended tenant is deleted with all
// its replicated data, a tombstone remains and the organization is freed.
func TestDeleteTenant(t *testing.T) {
	t.Parallel()
	t.Run("protected tenants cannot be deleted", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "delete-default", actorSystem, &pb.DeleteTenant{TenantId: tenant.DefaultTenantID}, CodeInvalid)
		h.adminDo(2000, "delete-system", actorSystem, &pb.DeleteTenant{TenantId: store.SystemTenantID}, CodeInvalid)
	})
	t.Run("a tenant must be suspended first", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		before := h.catalogJSON()
		h.adminDo(2000, "delete-active", actorSystem, &pb.DeleteTenant{TenantId: "t1"}, CodeConflict)
		if after := h.catalogJSON(); after != before {
			t.Errorf("rejected command changed the catalog")
		}
	})
	t.Run("unknown tenant", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "delete-ghost", actorSystem, &pb.DeleteTenant{TenantId: "ghost"}, CodeNotFound)
	})
	t.Run("deletes the data of the tenant and frees its organization", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(1100, "create-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
		h.putMonitorAt(1200, "m-t1-1", "t1", "m1", 60, 0, "")
		h.putMonitorAt(1201, "m-t1-2", "t1", "m2", 60, 0, "")
		h.putChannelAt(1202, "c-t1-1", "t1", "c1", []string{store.EventDown}, 0, "")
		h.putMonitorAt(1203, "m-t2-1", "t2", "m3", 60, 0, "")                        // monitor IDs are global
		h.putChannelAt(1204, "c-t2-1", "t2", "c2", []string{store.EventDown}, 0, "") // channel IDs are global
		alice := h.userWithSession(1300, "t1", "alice")

		h.adminDo(2000, "suspend-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
		h.adminDo(3000, "delete-t1", actorSystem, &pb.DeleteTenant{TenantId: "t1"}, "")

		h.wantAbsentTenant("t1")
		deleted, err := h.st.ListDeletedTenants(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) != 1 || deleted[0].ID != "t1" || deleted[0].DeletedMs != 3000 {
			t.Errorf("tombstones = %+v, want t1 at 3000", deleted)
		}
		h.wantAbsentMonitor("t1", "m1")
		h.wantAbsentMonitor("t1", "m2")
		if _, err := h.st.GetChannel(h.ctx, "t1", "c1"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetChannel(t1, c1) err = %v, want ErrNotFound", err)
		}
		if _, err := h.st.GetUser(h.ctx, "t1", "alice"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetUser(t1, alice) err = %v, want ErrNotFound", err)
		}
		if _, err := h.st.GetSession(h.ctx, alice); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("session of alice err = %v, want ErrNotFound", err)
		}

		// The other tenant is untouched.
		if _, err := h.st.GetMonitor(h.ctx, "t2", "m3"); err != nil {
			t.Errorf("GetMonitor(t2, m3): %v", err)
		}
		if _, err := h.st.GetChannel(h.ctx, "t2", "c2"); err != nil {
			t.Errorf("GetChannel(t2, c2): %v", err)
		}
		if got := h.tenantOrg("t2").ZitadelOrgID; got != "org-2" {
			t.Errorf("t2 org = %q, want org-2", got)
		}

		// The organization is free again; the tenant ID is not.
		h.adminDo(4000, "create-t3", actorSystem, &pb.CreateTenant{TenantId: "t3", Name: "Three", PlanId: "pro", OrgId: "org-1", Source: tenant.SourceAdmin}, "")
		if got, err := h.st.GetTenantByOrg(h.ctx, "org-1"); err != nil || got.ID != "t3" {
			t.Errorf("GetTenantByOrg(org-1) = %+v, %v; want t3", got, err)
		}
		h.adminDo(4001, "create-t1-again", actorSystem, &pb.CreateTenant{TenantId: "t1", Name: "Again", PlanId: "pro", Source: tenant.SourceAdmin}, CodeConflict)
		h.adminDo(4002, "delete-t1-again", actorSystem, &pb.DeleteTenant{TenantId: "t1"}, CodeNotFound)
	})
}

// TestPlans covers PutPlan and DeletePlan (ADR 0029).
func TestPutPlanRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload *pb.PutPlan
		code    string
	}{
		{"system plan is immutable", &pb.PutPlan{PlanId: tenant.SystemPlanID, Name: "System", LimitsJson: "{}"}, CodeInvalid},
		{"uppercase plan ID", &pb.PutPlan{PlanId: "Team", Name: "Team"}, CodeInvalid},
		{"empty plan ID", &pb.PutPlan{PlanId: "", Name: "Team"}, CodeInvalid},
		{"plan ID with underscore", &pb.PutPlan{PlanId: "_team", Name: "Team"}, CodeInvalid},
		{"empty name", &pb.PutPlan{PlanId: "team", Name: ""}, CodeInvalid},
		{"name of 101 characters", &pb.PutPlan{PlanId: "team", Name: strings.Repeat("n", 101)}, CodeInvalid},
		{"unknown limit", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxUsers":1}`}, CodeInvalid},
		{"minimum interval of zero", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"minIntervalSeconds":0}`}, CodeInvalid},
		{"limit above maximum", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxChannels":1000001}`}, CodeInvalid},
		{"fractional limit", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxMonitors":1.5}`}, CodeInvalid},
		{"string limit", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxMonitors":"2"}`}, CodeInvalid},
		{"limits as array", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `[1]`}, CodeInvalid},
		{"trailing data", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{} {}`}, CodeInvalid},
		{"zero monitors are a limit", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxMonitors":0}`}, ""},
		{"null is unlimited", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxMonitors":null}`}, ""},
		{"empty limits are unlimited", &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: ""}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := adminFSM(t)
			before := h.catalogJSON()
			h.adminDo(2000, "put-plan", actorSystem, tt.payload, tt.code)
			if tt.code != "" {
				if after := h.catalogJSON(); after != before {
					t.Errorf("rejected command changed the catalog")
				}
			}
		})
	}

	t.Run("stores limits canonically", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "plan-a", actorSystem, &pb.PutPlan{PlanId: "team", Name: "Team", LimitsJson: `{"maxChannels":3,"maxMonitors":2}`}, "")
		if got := string(h.plan("team").Limits); got != `{"maxMonitors":2,"maxChannels":3}` {
			t.Errorf("limits = %s", got)
		}
		h.adminDo(2001, "plan-b", actorSystem, &pb.PutPlan{PlanId: "nulls", Name: "Nulls", LimitsJson: `{"maxMonitors":null,"minIntervalSeconds":30}`}, "")
		if got := string(h.plan("nulls").Limits); got != `{"minIntervalSeconds":30}` {
			t.Errorf("limits with null = %s", got)
		}
		h.adminDo(2002, "plan-c", actorSystem, &pb.PutPlan{PlanId: "empty", Name: "Empty", LimitsJson: ""}, "")
		if got := string(h.plan("empty").Limits); got != "{}" {
			t.Errorf("empty limits = %s, want {}", got)
		}
	})

	t.Run("replacing a plan keeps its creation time", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(5000, "pro-again", actorSystem, &pb.PutPlan{PlanId: "pro", Name: "Pro 2", LimitsJson: "{}"}, "")
		p := h.plan("pro")
		if p.Name != "Pro 2" || p.CreatedMs != 1000 || p.UpdatedMs != 5000 {
			t.Errorf("plan = %+v; want name Pro 2, created 1000, updated 5000", p)
		}
	})
}

// TestDeletePlanRules covers DeletePlan: the reserved plans are never deleted,
// a plan in use is a conflict.
func TestDeletePlanRules(t *testing.T) {
	t.Parallel()
	t.Run("default and system plans stay", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "del-default", actorSystem, &pb.DeletePlan{PlanId: tenant.DefaultPlanID}, CodeInvalid)
		h.adminDo(2000, "del-system", actorSystem, &pb.DeletePlan{PlanId: tenant.SystemPlanID}, CodeInvalid)
	})
	t.Run("a plan in use conflicts until no tenant uses it", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "del-starter-in-use", actorSystem, &pb.DeletePlan{PlanId: "starter"}, CodeConflict)
		if _, err := h.st.GetPlan(h.ctx, "starter"); err != nil {
			t.Fatalf("starter removed although in use: %v", err)
		}
		h.adminDo(2001, "move-t1", actorSystem, &pb.UpdateTenant{TenantId: "t1", PlanId: proto.String("pro")}, "")
		h.adminDo(2002, "del-starter", actorSystem, &pb.DeletePlan{PlanId: "starter"}, "")
		if _, err := h.st.GetPlan(h.ctx, "starter"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetPlan(starter) err = %v, want ErrNotFound", err)
		}
	})
	t.Run("unknown plan", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(2000, "del-ghost", actorSystem, &pb.DeletePlan{PlanId: "ghost"}, CodeNotFound)
	})
}

// TestTenantLimitsOnWrites covers the limits enforced in PutEntity for monitors
// and channels (ADR 0029, 2B and 3A).
func TestTenantLimitsOnWrites(t *testing.T) {
	t.Parallel()
	t.Run("monitor limit rejects new monitors", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.putMonitorAt(1100, "m1", "t1", "m1", 60, 0, "")
		h.putMonitorAt(1101, "m2", "t1", "m2", 60, 0, "")
		h.putMonitorAt(1102, "m3", "t1", "m3", 60, 0, CodeLimitExceeded)
		h.wantAbsentMonitor("t1", "m3")
	})

	t.Run("updates of existing monitors are allowed at the limit", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.putMonitorAt(1100, "m1", "t1", "m1", 60, 0, "")
		h.putMonitorAt(1101, "m2", "t1", "m2", 60, 0, "")
		h.putMonitorAt(1200, "upd-m1", "t1", "m1", 60, 1, "")
		h.putMonitorAt(1201, "upd-m2", "t1", "m2", 90, 1, "")
	})

	t.Run("a shorter plan keeps the stock and rejects new or shorter intervals", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(1100, "create-t2", actorSystem, createTenantPayload(tenant.SourceAdmin), "")
		h.putMonitorAt(1200, "m-fast", "t2", "fast", 30, 0, "") // pro has no limits
		h.adminDo(1300, "t2-to-starter", actorSystem, &pb.UpdateTenant{TenantId: "t2", PlanId: proto.String("starter")}, "")

		h.putMonitorAt(1400, "fast-same", "t2", "fast", 30, 1, "") // interval unchanged: allowed
		h.putMonitorAt(1500, "fast-45", "t2", "fast", 45, 2, CodeLimitExceeded)
		if got := h.monitorInterval("t2", "fast"); got != 30 {
			t.Errorf("interval after rejected change = %d, want 30", got)
		}
		h.putMonitorAt(1600, "fast-60", "t2", "fast", 60, 2, "")

		h.putMonitorAt(1700, "slow-30", "t2", "slow", 30, 0, CodeLimitExceeded)
		h.wantAbsentMonitor("t2", "slow")
		h.putMonitorAt(1800, "slow-60", "t2", "slow", 60, 0, "")
	})

	t.Run("channel limit rejects new channels", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.putChannelAt(1100, "ch-1", "t1", "c1", []string{store.EventDown}, 0, "")
		h.putChannelAt(1101, "ch-2", "t1", "c2", []string{store.EventDown}, 0, CodeLimitExceeded)
		h.putChannelAt(1102, "ch-1-upd", "t1", "c1", []string{store.EventDown}, 1, "")
	})

	t.Run("explicit null lifts plan limits", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(1100, "create-t3", actorSystem, &pb.CreateTenant{TenantId: "t3", Name: "Three", PlanId: "starter", OrgId: "org-3",
			LimitsJson: `{"maxMonitors":null,"minIntervalSeconds":null,"maxChannels":null}`, Source: tenant.SourceAdmin}, "")
		h.putMonitorAt(1200, "m1", "t3", "m1", 30, 0, "")
		h.putMonitorAt(1201, "m2", "t3", "m2", 60, 0, "")
		h.putMonitorAt(1202, "m3", "t3", "m3", 60, 0, "")
		h.putChannelAt(1203, "c1", "t3", "c1", []string{store.EventDown}, 0, "")
		h.putChannelAt(1204, "c2", "t3", "c2", []string{store.EventDown}, 0, "")
	})

	t.Run("override raises a plan limit", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.adminDo(1100, "create-t3", actorSystem, &pb.CreateTenant{TenantId: "t3", Name: "Three", PlanId: "starter", OrgId: "org-3",
			LimitsJson: `{"maxMonitors":3}`, Source: tenant.SourceAdmin}, "")
		h.putMonitorAt(1200, "m1", "t3", "m1", 60, 0, "")
		h.putMonitorAt(1201, "m2", "t3", "m2", 60, 0, "")
		h.putMonitorAt(1202, "m3", "t3", "m3", 60, 0, "")
		h.putMonitorAt(1203, "m4", "t3", "m4", 60, 0, CodeLimitExceeded)
	})

	t.Run("suspended tenant rejects writes", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		h.putMonitorAt(1100, "m1", "t1", "m1", 60, 0, "")
		h.adminDo(1200, "suspend-t1", actorSystem, &pb.SetTenantSuspended{TenantId: "t1", Suspended: true}, "")
		h.putMonitorAt(1300, "m2", "t1", "m2", 60, 0, CodeConflict)
		h.putMonitorAt(1301, "m1-upd", "t1", "m1", 60, 1, CodeConflict)
		h.putChannelAt(1302, "c1", "t1", "c1", []string{store.EventDown}, 0, CodeConflict)
	})

	t.Run("system channels are not limited", func(t *testing.T) {
		t.Parallel()
		h := adminFSM(t)
		// The system tenant carries a limit of one channel; system channels ignore limits.
		h.putTenantDoc(1100, "tenant-system-limits", store.SystemTenantID,
			TenantDocument{Name: "System", Plan: tenant.SystemPlanID, Limits: json.RawMessage(`{"maxChannels":1}`)}, actorSystem, "")
		events := []string{store.EventSystem}
		h.putChannelAt(1200, "sys-1", store.SystemTenantID, "sys1", events, 0, "")
		h.putChannelAt(1201, "sys-2", store.SystemTenantID, "sys2", events, 0, "")
		h.putChannelAt(1202, "sys-3", store.SystemTenantID, "sys3", events, 0, "")
	})
}

// TestLimitsIgnoredBelowLevel8 covers ADR 0018 for ADR 0029: below command level
// 8 no limit and no suspension is checked on writes.
func TestLimitsIgnoredBelowLevel8(t *testing.T) {
	t.Parallel()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(TenantAdminCommandLevel - 1)
	h.putSystemTenant(1000)
	err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		return tx.PutPlan(h.ctx, store.Plan{ID: "starter", Name: "Starter",
			Limits: json.RawMessage(`{"maxMonitors":1,"minIntervalSeconds":60,"maxChannels":1}`), CreatedMs: 1, UpdatedMs: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
	h.putTenantDoc(1000, "put-t1", "t1", TenantDocument{Name: "t1", Plan: "starter", Limits: json.RawMessage(`{"maxMonitors":1}`)}, actorNone, "")
	h.putMonitorAt(1100, "m1", "t1", "m1", 30, 0, "")
	h.putMonitorAt(1101, "m2", "t1", "m2", 30, 0, "")
	h.putMonitorAt(1102, "m3", "t1", "m3", 30, 0, "")
	h.putChannelAt(1103, "c1", "t1", "c1", []string{store.EventDown}, 0, "")
	h.putChannelAt(1104, "c2", "t1", "c2", []string{store.EventDown}, 0, "")

	h.putTenantDoc(1200, "put-t2", "t2", TenantDocument{Name: "t2", Plan: "default", Suspended: true}, actorNone, "")
	h.putMonitorAt(1201, "m-t2", "t2", "m-t2", 60, 0, "")
}

// TestLimitViolation covers the rules of ADR 0029 for a single write.
func TestLimitViolation(t *testing.T) {
	t.Parallel()
	ptr := func(v int) *int { return &v }
	starter := tenant.Limits{MaxMonitors: ptr(2), MinIntervalSeconds: ptr(60), MaxChannels: ptr(1)}
	monitor := pb.EntityKind_ENTITY_KIND_MONITOR
	channel := pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL
	tests := []struct {
		name        string
		lim         tenant.Limits
		usage       store.TenantUsage
		kind        pb.EntityKind
		create      bool
		interval    int
		oldInterval int
		violation   bool
	}{
		{"monitor create below the maximum", starter, store.TenantUsage{Monitors: 1}, monitor, true, 60, 0, false},
		{"monitor create at the maximum", starter, store.TenantUsage{Monitors: 2}, monitor, true, 60, 0, true},
		{"monitor create without a maximum", tenant.Limits{}, store.TenantUsage{Monitors: 1000}, monitor, true, 60, 0, false},
		{"monitor create at the minimum interval", starter, store.TenantUsage{}, monitor, true, 60, 0, false},
		{"monitor create below the minimum interval", starter, store.TenantUsage{}, monitor, true, 59, 0, true},
		{"monitor update keeping a short interval", starter, store.TenantUsage{Monitors: 2}, monitor, false, 30, 30, false},
		{"monitor update to a still short interval", starter, store.TenantUsage{Monitors: 2}, monitor, false, 45, 30, true},
		{"monitor update to the minimum interval", starter, store.TenantUsage{Monitors: 2}, monitor, false, 60, 30, false},
		{"monitor update to a short interval from a long one", starter, store.TenantUsage{Monitors: 2}, monitor, false, 30, 120, true},
		{"monitor update without an interval minimum", tenant.Limits{}, store.TenantUsage{Monitors: 2}, monitor, false, 1, 30, false},
		{"monitor update at the maximum", starter, store.TenantUsage{Monitors: 2}, monitor, false, 60, 60, false},
		{"channel create below the maximum", starter, store.TenantUsage{Channels: 0}, channel, true, 0, 0, false},
		{"channel create at the maximum", starter, store.TenantUsage{Channels: 1}, channel, true, 0, 0, true},
		{"channel update at the maximum", starter, store.TenantUsage{Channels: 1}, channel, false, 0, 0, false},
		{"channel create without a maximum", tenant.Limits{}, store.TenantUsage{Channels: 9}, channel, true, 0, 0, false},
		{"maintenance window is not limited", starter, store.TenantUsage{Monitors: 9, Channels: 9}, pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW, true, 1, 0, false},
		{"user is not limited", starter, store.TenantUsage{Monitors: 9}, pb.EntityKind_ENTITY_KIND_USER, true, 1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := LimitViolation(tt.lim, "starter", tt.usage, tt.kind, tt.create, tt.interval, tt.oldInterval)
			if !tt.violation {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			var ce *CommandError
			if !errors.As(err, &ce) || ce.Code != CodeLimitExceeded {
				t.Fatalf("err = %v, want code %q", err, CodeLimitExceeded)
			}
			if !strings.Contains(ce.Message, `"starter"`) {
				t.Errorf("message %q does not name the plan", ce.Message)
			}
		})
	}
}

// adminScript applies a fixed sequence of tenant, plan and limit commands to a
// fresh FSM and returns the outcome of every step and the final fingerprint.
func adminScript(t *testing.T) ([]string, string) {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	var steps []string
	run := func(name string, cmd *pb.Command, issuedAt int64) {
		res := h.apply(cmd, issuedAt)
		steps = append(steps, fmt.Sprintf("%s: err=%v result=%s", name, res.Err, res.Result))
	}
	put := func(requestID, id string, doc TenantDocument, actor pb.Actor_Kind) *pb.Command {
		cmd := must(NewPutTenant(requestID, id, doc))
		cmd.Actor = &pb.Actor{Kind: actor}
		return cmd
	}
	admin := func(requestID string, actor pb.Actor_Kind, payload any) *pb.Command {
		return adminCommand(requestID, actor, payload)
	}
	monitor := func(requestID, tenantID, id string, interval int, rev uint64) *pb.Command {
		doc := monitorDoc("Web " + id)
		doc.IntervalSeconds = interval
		return must(NewPutMonitor(requestID, tenantID, id, doc, rev))
	}

	run("level", NewSetFeatureLevel("level", TenantAdminCommandLevel), 1000)
	run("system", put("sys", store.SystemTenantID, TenantDocument{Name: "System", Plan: tenant.SystemPlanID}, actorSystem), 1000)
	run("default", put("default", tenant.DefaultTenantID, TenantDocument{Name: "Default", Plan: tenant.DefaultPlanID, ZitadelOrgID: "org-platform"}, actorSystem), 1000)
	run("plan starter", admin("plan-starter", actorSystem, &pb.PutPlan{PlanId: "starter", Name: "Starter", LimitsJson: `{"maxMonitors":2,"minIntervalSeconds":60,"maxChannels":1}`}), 1000)
	run("plan pro", admin("plan-pro", actorSystem, &pb.PutPlan{PlanId: "pro", Name: "Pro", LimitsJson: "{}"}), 1000)
	run("create t1", admin("create-t1", actorSystem, &pb.CreateTenant{TenantId: "t1", Name: "One", PlanId: "starter", OrgId: "org-1", Source: tenant.SourceAdmin}), 1000)
	run("create t1 again", admin("create-t1-again", actorSystem, &pb.CreateTenant{TenantId: "t1", Name: "One", PlanId: "starter", OrgId: "org-9", Source: tenant.SourceAdmin}), 1001)
	run("auto existing", admin("auto-existing", actorSystem, &pb.CreateTenant{TenantId: "p0", Name: "Org 1", PlanId: "pro", OrgId: "org-1", Source: tenant.SourceAuto, MaxAutoTenants: 1}), 1002)
	run("auto a1", admin("auto-a1", actorSystem, &pb.CreateTenant{TenantId: "a1", Name: "Org 2", PlanId: "pro", OrgId: "org-2", Source: tenant.SourceAuto, MaxAutoTenants: 1}), 1003)
	run("auto a2", admin("auto-a2", actorSystem, &pb.CreateTenant{TenantId: "a2", Name: "Org 3", PlanId: "pro", OrgId: "org-3", Source: tenant.SourceAuto, MaxAutoTenants: 1}), 1004)
	run("update t1 plan", admin("update-t1", actorSystem, &pb.UpdateTenant{TenantId: "t1", PlanId: proto.String("pro")}), 1005)
	run("monitor m1", monitor("mon-m1", "t1", "m1", 60, 0), 1006)
	run("monitor m1 again", monitor("mon-m1-upd", "t1", "m1", 90, 1), 1007)
	run("suspend a1", admin("suspend-a1", actorSystem, &pb.SetTenantSuspended{TenantId: "a1", Suspended: true}), 1008)
	run("delete a1", admin("delete-a1", actorSystem, &pb.DeleteTenant{TenantId: "a1"}), 1009)
	run("delete starter", admin("delete-starter", actorSystem, &pb.DeletePlan{PlanId: "starter"}), 1010)
	run("delete starter again", admin("delete-starter-again", actorSystem, &pb.DeletePlan{PlanId: "starter"}), 1011)

	fp, err := h.st.Fingerprint(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return steps, fp
}

// TestAdminCommandsAreDeterministic checks that the same log gives the same
// results and the same state on every replica (FA-CLU-07, ADR 0029).
func TestAdminCommandsAreDeterministic(t *testing.T) {
	t.Parallel()
	stepsA, fpA := adminScript(t)
	stepsB, fpB := adminScript(t)
	if len(stepsA) != len(stepsB) {
		t.Fatalf("step counts differ: %d vs %d", len(stepsA), len(stepsB))
	}
	for i := range stepsA {
		if stepsA[i] != stepsB[i] {
			t.Errorf("step %d differs:\n%s\n%s", i, stepsA[i], stepsB[i])
		}
	}
	if fpA != fpB {
		t.Errorf("fingerprints differ: %s vs %s", fpA, fpB)
	}
	// The script must exercise both outcomes, or the comparison proves little.
	joined := strings.Join(stepsA, "\n")
	for _, want := range []string{"conflict", "limit_exceeded", `"existing":true`} {
		if !strings.Contains(joined, want) {
			t.Errorf("script never produced %q:\n%s", want, joined)
		}
	}
}

// TestBootstrapTenantAtLevel8 is a regression test: the bootstrap command of
// the default tenant (EnsureDefaultTenant) carries the system actor, so a
// cluster already at command level 8 still creates it (ADR 0029).
func TestBootstrapTenantAtLevel8(t *testing.T) {
	t.Parallel()
	h := adminFSM(t)
	cmd, err := NewPutTenant("bootstrap-t7", "t7", TenantDocument{Name: "Seven", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.GetActor().GetKind() != pb.Actor_KIND_SYSTEM {
		t.Fatalf("NewPutTenant actor = %v, want the system actor", cmd.GetActor().GetKind())
	}
	checkCode(t, h.apply(cmd, 3000).Err, "")
	if _, err := h.st.GetTenant(h.ctx, "t7"); err != nil {
		t.Errorf("GetTenant(t7) = %v, want the bootstrapped tenant", err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// withRequestID returns cmd with another request ID, so that the FSM applies it
// instead of deduplicating it against an earlier command with the same binding.
func withRequestID(cmd *pb.Command, id string) *pb.Command {
	cmd.RequestId = id
	return cmd
}

// bindFSM returns an FSM at command level 7 with the tenants t1, t2 and the
// system tenant, none of them bound to an organization.
func bindFSM(t *testing.T) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(OIDCCommandLevel)
	h.putTenant(1000, "t1")
	h.putTenant(1000, "t2")
	h.putSystemTenant(1000)
	return h
}

// tenantOrg reads the organization of a tenant, or fails the test.
func (h *alertFSM) tenantOrg(id string) store.Tenant {
	h.t.Helper()
	tn, err := h.st.GetTenant(h.ctx, id)
	if err != nil {
		h.t.Fatalf("GetTenant(%s): %v", id, err)
	}
	return tn
}

// TestBindTenantOrg covers ADR 0027: the rules of BindTenantOrg, applied in the
// order of the FSM, with the expected outcome and organization afterwards.
func TestBindTenantOrg(t *testing.T) {
	long := strings.Repeat("a", maxOrgIDLength)
	tests := []struct {
		name string
		// setup commands are applied first and must succeed.
		setup []*pb.Command
		cmd   *pb.Command
		want  error
		// wantOrg is the expected organization of t1 and t2 afterwards.
		wantT1, wantT2 string
	}{
		{name: "binds an unbound tenant", cmd: NewBindTenantOrg("t1", "org-1"), wantT1: "org-1"},
		{name: "accepts the longest ID", cmd: NewBindTenantOrg("t1", long), wantT1: long},
		{name: "accepts all allowed characters", cmd: NewBindTenantOrg("t1", "Org.1_a@b-C9"), wantT1: "Org.1_a@b-C9"},
		{
			name:   "same organization again is a no-op success",
			setup:  []*pb.Command{NewBindTenantOrg("t1", "org-1")},
			cmd:    withRequestID(NewBindTenantOrg("t1", "org-1"), "again"),
			wantT1: "org-1",
		},
		{
			name:  "rebinding to another organization conflicts",
			setup: []*pb.Command{NewBindTenantOrg("t1", "org-1")},
			cmd:   withRequestID(NewBindTenantOrg("t1", "org-2"), "rebind"),
			want:  store.ErrConflict, wantT1: "org-1",
		},
		{
			name:  "organization of another tenant conflicts",
			setup: []*pb.Command{NewBindTenantOrg("t1", "org-1")},
			cmd:   NewBindTenantOrg("t2", "org-1"),
			want:  store.ErrConflict, wantT1: "org-1",
		},
		{
			name:  "organization of another tenant conflicts even if the tenant is unbound",
			setup: []*pb.Command{NewBindTenantOrg("t1", "org-1")},
			cmd:   NewBindTenantOrg("t2", "org-1"),
			want:  store.ErrConflict, wantT1: "org-1", wantT2: "",
		},
		{name: "unknown tenant is not found", cmd: NewBindTenantOrg("missing", "org-1"), want: store.ErrNotFound},
		{
			name:  "unknown tenant with a taken organization is not found",
			setup: []*pb.Command{NewBindTenantOrg("t1", "org-1")},
			cmd:   NewBindTenantOrg("missing", "org-1"),
			want:  store.ErrNotFound, wantT1: "org-1",
		},
		{name: "empty tenant ID is invalid", cmd: NewBindTenantOrg("", "org-1"), want: ErrInvalidCommand},
		{name: "system tenant is invalid", cmd: NewBindTenantOrg(store.SystemTenantID, "org-1"), want: ErrInvalidCommand},
		{name: "empty organization is invalid", cmd: NewBindTenantOrg("t1", ""), want: ErrInvalidCommand},
		{name: "organization of 129 bytes is invalid", cmd: NewBindTenantOrg("t1", long+"a"), want: ErrInvalidCommand},
		{name: "space is invalid", cmd: NewBindTenantOrg("t1", "org 1"), want: ErrInvalidCommand},
		{name: "slash is invalid", cmd: NewBindTenantOrg("t1", "org/1"), want: ErrInvalidCommand},
		{name: "non-ASCII letter is invalid", cmd: NewBindTenantOrg("t1", "orgä"), want: ErrInvalidCommand},
		{name: "newline is invalid", cmd: NewBindTenantOrg("t1", "org\n1"), want: ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := bindFSM(t)
			for _, s := range tt.setup {
				h.do(s, 2000, nil)
			}
			h.do(tt.cmd, 3000, tt.want)
			// Rejected commands write nothing, so the organizations stay as expected.
			if got := h.tenantOrg("t1").ZitadelOrgID; got != tt.wantT1 {
				t.Errorf("t1 org = %q, want %q", got, tt.wantT1)
			}
			if got := h.tenantOrg("t2").ZitadelOrgID; got != tt.wantT2 {
				t.Errorf("t2 org = %q, want %q", got, tt.wantT2)
			}
		})
	}
}

// TestBindTenantOrgResult checks the result JSON of a bind, including the
// idempotent case.
func TestBindTenantOrgResult(t *testing.T) {
	h := bindFSM(t)
	want := `{"tenant_id":"t1","org_id":"org-1"}`
	for _, id := range []string{"first", "again"} {
		res := h.apply(withRequestID(NewBindTenantOrg("t1", "org-1"), id), 2000)
		if res.Err != nil {
			t.Fatalf("bind %s: %v", id, res.Err)
		}
		if got := string(res.Result); got != want {
			t.Errorf("result %s = %s, want %s", id, got, want)
		}
	}
}

// TestBindTenantOrgNoWriteWhenBound checks that a repeated bind to the same
// organization does not touch the tenant row (the update time stays).
func TestBindTenantOrgNoWriteWhenBound(t *testing.T) {
	h := bindFSM(t)
	h.do(NewBindTenantOrg("t1", "org-1"), 2000, nil)
	before := h.tenantOrg("t1")

	h.do(withRequestID(NewBindTenantOrg("t1", "org-1"), "again"), 9000, nil)
	after := h.tenantOrg("t1")
	if after.UpdatedMs != before.UpdatedMs || after.UpdatedMs != 2000 {
		t.Errorf("UpdatedMs = %d after no-op bind, want %d", after.UpdatedMs, before.UpdatedMs)
	}
}

// TestBindTenantOrgKeepsOtherFields checks that only the organization and the
// update time change, and that the tenant is found by its organization.
func TestBindTenantOrgKeepsOtherFields(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(OIDCCommandLevel)
	cmd, err := NewPutTenant("put-acme", "acme", TenantDocument{Name: "Acme", Plan: "pro", Limits: json.RawMessage(`{"monitors":5}`)})
	if err != nil {
		t.Fatal(err)
	}
	h.do(cmd, 1000, nil)
	h.do(NewBindTenantOrg("acme", "org-acme"), 2000, nil)

	got := h.tenantOrg("acme")
	if got.Name != "Acme" || got.Plan != "pro" || got.CreatedMs != 1000 || got.UpdatedMs != 2000 {
		t.Errorf("tenant = %+v; name, plan, created and updated time must be kept or set as expected", got)
	}
	if string(got.Limits) != `{"monitors":5}` {
		t.Errorf("limits = %s, want kept", got.Limits)
	}
	if got.ZitadelOrgID != "org-acme" {
		t.Errorf("org = %q, want org-acme", got.ZitadelOrgID)
	}
	byOrg, err := h.st.GetTenantByOrg(h.ctx, "org-acme")
	if err != nil || byOrg.ID != "acme" {
		t.Errorf("GetTenantByOrg = %+v, %v; want acme", byOrg, err)
	}
}

// TestBindTenantOrgNeedsLevel7 covers ADR 0018 at the FSM: below level 7 the
// command is rejected and writes nothing; at level 7 it is applied.
func TestBindTenantOrgNeedsLevel7(t *testing.T) {
	t.Run("rejected at level 6", func(t *testing.T) {
		h := newAlertFSM(t, t.TempDir())
		h.setLevel(OIDCCommandLevel - 1)
		h.putTenant(1000, "t1")
		h.do(NewBindTenantOrg("t1", "org-1"), 2000, ErrInvalidCommand)
		if got := h.tenantOrg("t1").ZitadelOrgID; got != "" {
			t.Errorf("org stored below level 7: %q", got)
		}
	})
	t.Run("accepted at level 7", func(t *testing.T) {
		h := newAlertFSM(t, t.TempDir())
		h.setLevel(OIDCCommandLevel)
		h.putTenant(1000, "t1")
		h.do(NewBindTenantOrg("t1", "org-1"), 2000, nil)
		if got := h.tenantOrg("t1").ZitadelOrgID; got != "org-1" {
			t.Errorf("org = %q, want org-1", got)
		}
	})
}

// TestBindTenantOrgCommandLevelGate covers ADR 0018 at the node: a command that
// needs a version the cluster does not have yet is rejected before it is proposed,
// and a node that cannot apply version 7 rejects it locally.
func TestBindTenantOrgCommandLevelGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("rejected until the level is raised", func(t *testing.T) {
		dir := t.TempDir()
		n := startGateNode(ctx, t, dir, openStore(t, dir), CommandVersion)
		if _, err := n.Apply(ctx, NewBindTenantOrg("t1", "org-1")); !notEnabled(err) {
			t.Fatalf("err = %v, want CodeNotEnabled while level is 2", err)
		}
		if _, err := n.Apply(ctx, NewSetFeatureLevel("b1", OIDCCommandLevel)); err != nil {
			t.Fatal(err)
		}
		put, err := NewPutTenant("put-t1", "t1", TenantDocument{Name: "t1", Plan: "default"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.Apply(ctx, put); err != nil {
			t.Fatal(err)
		}
		if _, err := n.Apply(ctx, NewBindTenantOrg("t1", "org-1")); err != nil {
			t.Errorf("bind after raising the level: %v", err)
		}
	})

	t.Run("node that cannot apply version 7 rejects it locally", func(t *testing.T) {
		dir := t.TempDir()
		n := startGateNode(ctx, t, dir, openStore(t, dir), OIDCCommandLevel-1)
		if _, err := n.Apply(ctx, NewSetFeatureLevel("b2", OIDCCommandLevel)); err != nil {
			t.Fatal(err)
		}
		if _, err := n.ApplyLocal(ctx, NewBindTenantOrg("t1", "org-1")); !notEnabled(err) {
			t.Errorf("ApplyLocal err = %v, want CodeNotEnabled from the node's own maximum", err)
		}
	})
}

// TestSystemTenantPutRejectsOrganization checks the hardening of PutEntity for
// tenants (ADR 0027): the system tenant never holds an organization, but other
// tenants are unaffected.
func TestSystemTenantPutRejectsOrganization(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		orgID    string
		actor    pb.Actor_Kind
		want     error
	}{
		{"system tenant with organization is invalid", store.SystemTenantID, "org-1", pb.Actor_KIND_SYSTEM, ErrInvalidCommand},
		{"system tenant without organization is accepted", store.SystemTenantID, "", pb.Actor_KIND_SYSTEM, nil},
		{"other tenant with organization is accepted", "t1", "org-1", pb.Actor_KIND_USER, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAlertFSM(t, t.TempDir())
			h.setLevel(OIDCCommandLevel)
			cmd, err := NewPutTenant("put-"+tt.tenantID+"-"+tt.orgID, tt.tenantID,
				TenantDocument{Name: "x", Plan: "default", ZitadelOrgID: tt.orgID})
			if err != nil {
				t.Fatal(err)
			}
			cmd.Actor = &pb.Actor{Kind: tt.actor}
			h.do(cmd, 1000, tt.want)
			if tt.want != nil {
				if _, err := h.st.GetTenant(h.ctx, tt.tenantID); err == nil {
					t.Errorf("tenant %s stored after a rejected put", tt.tenantID)
				}
			}
		})
	}
}

// TestValidOrgID covers the character set and length rules of organization IDs (ADR 0027).
func TestValidOrgID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", false},
		{"1", true},
		{"123456789012345678", true},
		{"a.b_c@d-e", true},
		{strings.Repeat("x", maxOrgIDLength), true},
		{strings.Repeat("x", maxOrgIDLength+1), false},
		{"a b", false},
		{"a/b", false},
		{"a:b", false},
		{"a+b", false},
		{"äb", false},
		{"a\x00b", false},
	}
	for _, tt := range tests {
		if got := validOrgID(tt.id); got != tt.want {
			t.Errorf("validOrgID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

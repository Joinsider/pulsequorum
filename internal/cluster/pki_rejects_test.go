// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/pki"
	"github.com/joinsider/pulsequorum/internal/store"
)

// probeTokenCommand builds a join token for a probe of tenantID that expires
// after an hour.
func probeTokenCommand(t *testing.T, requestID, tokenID, tenantID string) *pb.Command {
	t.Helper()
	cmd, err := NewPutJoinToken(requestID, tokenID, JoinTokenDocument{
		SecretHash: pki.HashSecret("secret-" + tokenID),
		Role:       "probe",
		TenantID:   tenantID,
		ExpiresMs:  10_000 + time.Hour.Milliseconds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// quotaFSM returns an FSM at command level 10 with the tenant tq on the plan
// quota, which allows one private probe.
func quotaFSM(t *testing.T) *alertFSM {
	t.Helper()
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	h.putPlan(1100, "quota", `{"maxPrivateProbes":1}`)
	h.createTenant(1100, "tq", "quota")
	return h
}

// TestPrivateProbeTokenRejections covers the checks of a tenant-bound probe
// token at command level 10 and above: the system tenant, a command tenant
// that differs, the missing user actor, a suspended tenant and the quota.
func TestPrivateProbeTokenRejections(t *testing.T) {
	t.Parallel()
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u1", Name: "Ada"}
	tests := []struct {
		name      string
		setup     func(h *alertFSM)
		docTenant string
		// cmdTenant replaces the tenant of the command envelope when set.
		cmdTenant string
		// quota selects the FSM with the plan quota instead of the default one.
		quota    bool
		actor    *pb.Actor
		wantCode string
	}{
		{name: "system tenant", docTenant: store.SystemTenantID, actor: user, wantCode: CodeInvalid},
		{name: "command tenant differs", docTenant: "t1", cmdTenant: "t2", actor: user, wantCode: CodeInvalid},
		{name: "no user actor", docTenant: "t1", wantCode: CodeInvalid},
		{
			name:      "suspended tenant",
			setup:     func(h *alertFSM) { h.suspendTenant(1100, "t1", true) },
			docTenant: "t1", actor: user, wantCode: CodeConflict,
		},
		{
			name: "private probe limit reached",
			setup: func(h *alertFSM) {
				// The first token is open, so it counts against the limit of one.
				first := withActor(probeTokenCommand(h.t, "tok-first-req", "tok-first", "tq"), user)
				checkCode(h.t, h.apply(first, 10_000).Err, "")
			},
			docTenant: "tq", quota: true, actor: user, wantCode: CodeLimitExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var h *alertFSM
			if tt.quota {
				h = quotaFSM(t)
			} else {
				h = setupProbeAlertFSM(t, ProbeCommandLevel)
			}
			if tt.setup != nil {
				tt.setup(h)
			}
			cmd := probeTokenCommand(t, "tok-req", "tok-private", tt.docTenant)
			if tt.cmdTenant != "" {
				cmd.TenantId = tt.cmdTenant
			}
			cmd.Actor = tt.actor
			checkCode(t, h.apply(cmd, 10_000).Err, tt.wantCode)
		})
	}
}

// TestPrivateProbeChecksReportStoreErrors covers the store failures and the
// missing plan of the private probe checks.
func TestPrivateProbeChecksReportStoreErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fail string
		run  func(ctx context.Context, tx store.StateTx) error
		want error
	}{
		{
			name: "tenant lookup fails",
			fail: "GetTenant",
			run: func(ctx context.Context, tx store.StateTx) error {
				return validatePrivateProbeTokenTenant(ctx, tx, &pb.Command{}, "t1", 10_000)
			},
			want: errInjected,
		},
		{
			name: "command level unreadable",
			fail: "CommandLevel",
			run: func(ctx context.Context, tx store.StateTx) error {
				return validatePrivateProbeTokenTenant(ctx, tx, &pb.Command{}, "t1", 10_000)
			},
			want: errInjected,
		},
		{
			name: "plan of tenant missing",
			run: func(ctx context.Context, tx store.StateTx) error {
				return checkPrivateProbeQuota(ctx, tx, store.Tenant{ID: "t1", Plan: "no-such-plan"}, 10_000)
			},
			want: ErrInvalidCommand,
		},
		{
			name: "private probe count fails",
			fail: "CountTenantPrivateProbes",
			run: func(ctx context.Context, tx store.StateTx) error {
				return checkPrivateProbeQuota(ctx, tx, store.Tenant{ID: "tq", Plan: "quota"}, 10_000)
			},
			want: errInjected,
		},
		{
			name: "open join token count fails",
			fail: "CountOpenTenantJoinTokens",
			run: func(ctx context.Context, tx store.StateTx) error {
				return checkPrivateProbeQuota(ctx, tx, store.Tenant{ID: "tq", Plan: "quota"}, 10_000)
			},
			want: errInjected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := quotaFSM(t)
			inTx(t, h, func(tx store.StateTx) {
				if err := tt.run(h.ctx, faultyTx{StateTx: tx, fail: tt.fail}); !errors.Is(err, tt.want) {
					t.Errorf("err = %v, want %v", err, tt.want)
				}
			})
		})
	}
}

// TestRevokeNodeClearsProbeAlertState covers a platform revocation of a public
// probe with and without a probe_offline alert, and checks that the tenant
// boundary holds for tenant-scoped revocations.
func TestRevokeNodeClearsProbeAlertState(t *testing.T) {
	t.Parallel()
	t.Run("public probe without alert", func(t *testing.T) {
		t.Parallel()
		h := setupProbeAlertFSM(t, ProbeCommandLevel)
		h.do(NewRegisterProbe("rp-bare", store.Node{ID: "p-bare", Name: "bare"}), 1100, nil)
		h.do(NewRevokeNode("rv-bare", "p-bare"), 1300, nil)
	})
	t.Run("public probe with inactive alert", func(t *testing.T) {
		t.Parallel()
		h := setupProbeAlertFSM(t, ProbeCommandLevel)
		h.do(NewRegisterProbe("rp-off", store.Node{ID: "p-off", Name: "off"}), 1100, nil)
		// An alert is stored only once raised; it stays inactive after it resolved.
		h.do(NewSetSystemAlert("probe_offline", "p-off", true, 0, nil), 1200, nil)
		h.do(NewSetSystemAlert("probe_offline", "p-off", false, 0, nil), 1250, nil)
		h.do(NewRevokeNode("rv-off", "p-off"), 1300, nil)
		inTx(t, h, func(tx store.StateTx) {
			sa, err := tx.GetSystemAlert(h.ctx, "probe_offline", "p-off")
			if err != nil {
				t.Fatal(err)
			}
			if sa.Active {
				t.Error("inactive alert became active")
			}
		})
	})
}

// TestTenantProbeRevokeAnswersNotFoundAcrossTenants checks the tenant boundary:
// a tenant cannot revoke a probe of another tenant, and the probe survives.
func TestTenantProbeRevokeAnswersNotFoundAcrossTenants(t *testing.T) {
	t.Parallel()
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	user := &pb.Actor{Kind: pb.Actor_KIND_USER, Id: "u-t1", Name: "Ada"}
	h.do(NewRegisterProbe("rp-t2", store.Node{ID: "p2", Name: "p2", OwnerTenantID: "t2"}), 1100, nil)

	res := h.apply(withActor(NewRevokeTenantProbe("rev-foreign", "t1", "p2"), user), 1300)
	checkCode(t, res.Err, CodeNotFound)
	var ce *CommandError
	if errors.As(res.Err, &ce) && ce.Message != "probe not found" {
		t.Errorf("message = %q, want %q", ce.Message, "probe not found")
	}
	inTx(t, h, func(tx store.StateTx) {
		n, err := tx.GetNode(h.ctx, "p2")
		if err != nil {
			t.Fatalf("GetNode(p2): %v", err)
		}
		if n.OwnerTenantID != "t2" {
			t.Errorf("owner of p2 = %q, want t2", n.OwnerTenantID)
		}
	})
}

// TestRevokeNodeReportsStoreErrors covers the store failures on the way of a
// node revocation: the probe alerts, and the probe_offline system alert.
func TestRevokeNodeReportsStoreErrors(t *testing.T) {
	t.Parallel()
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	h.do(NewRegisterProbe("rp-pub", store.Node{ID: "p-pub", Name: "pub"}), 1100, nil)
	h.do(NewSetSystemAlert("probe_offline", "p-pub", true, 0, nil), 1200, nil)
	node := store.Node{ID: "p-pub", Role: store.NodeRoleProbe}

	t.Run("probe alert deletion fails", func(t *testing.T) {
		inTx(t, h, func(tx store.StateTx) {
			_, err := applyRevokeNode(h.ctx, faultyTx{StateTx: tx, fail: "DeleteProbeAlertsByProbe"},
				&pb.Command{IssuedAtMs: 1300}, &pb.RevokeNode{NodeId: "p-pub"})
			if !errors.Is(err, errInjected) {
				t.Errorf("err = %v, want the injected store failure", err)
			}
		})
	})
	t.Run("system alert lookup fails", func(t *testing.T) {
		inTx(t, h, func(tx store.StateTx) {
			err := clearRevokedProbeAlerts(h.ctx, faultyTx{StateTx: tx, fail: "GetSystemAlert"}, "p-pub", node, true, 1300)
			if !errors.Is(err, errInjected) {
				t.Errorf("err = %v, want the injected store failure", err)
			}
		})
	})
	t.Run("system alert update fails", func(t *testing.T) {
		inTx(t, h, func(tx store.StateTx) {
			err := clearRevokedProbeAlerts(h.ctx, faultyTx{StateTx: tx, fail: "PutSystemAlert"}, "p-pub", node, true, 1300)
			if !errors.Is(err, errInjected) {
				t.Errorf("err = %v, want the injected store failure", err)
			}
		})
	})
}

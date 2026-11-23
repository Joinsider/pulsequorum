// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// errInjected is the failure that faultyTx returns for the call it is told to fail.
var errInjected = errors.New("injected store failure")

// faultyTx is a store.StateTx whose method named by fail returns errInjected.
// All other calls go to the real transaction. The store-error branches of the
// FSM helpers cannot be reached with SQLite alone, so the tests use it.
type faultyTx struct {
	store.StateTx
	fail string
}

func (f faultyTx) CommandLevel(ctx context.Context) (uint32, error) {
	if f.fail == "CommandLevel" {
		return 0, errInjected
	}
	return f.StateTx.CommandLevel(ctx)
}

func (f faultyTx) GetTenant(ctx context.Context, id string) (store.Tenant, error) {
	if f.fail == "GetTenant" {
		return store.Tenant{}, errInjected
	}
	return f.StateTx.GetTenant(ctx, id)
}

func (f faultyTx) PutTenant(ctx context.Context, t store.Tenant) error {
	if f.fail == "PutTenant" {
		return errInjected
	}
	return f.StateTx.PutTenant(ctx, t)
}

func (f faultyTx) CountTenantPrivateProbes(ctx context.Context, tenantID string) (int, error) {
	if f.fail == "CountTenantPrivateProbes" {
		return 0, errInjected
	}
	return f.StateTx.CountTenantPrivateProbes(ctx, tenantID)
}

func (f faultyTx) CountOpenTenantJoinTokens(ctx context.Context, tenantID string, nowMs int64) (int, error) {
	if f.fail == "CountOpenTenantJoinTokens" {
		return 0, errInjected
	}
	return f.StateTx.CountOpenTenantJoinTokens(ctx, tenantID, nowMs)
}

func (f faultyTx) DeleteProbeAlertsByProbe(ctx context.Context, probeID string) error {
	if f.fail == "DeleteProbeAlertsByProbe" {
		return errInjected
	}
	return f.StateTx.DeleteProbeAlertsByProbe(ctx, probeID)
}

func (f faultyTx) GetSystemAlert(ctx context.Context, kind, subject string) (store.SystemAlert, error) {
	if f.fail == "GetSystemAlert" {
		return store.SystemAlert{}, errInjected
	}
	return f.StateTx.GetSystemAlert(ctx, kind, subject)
}

func (f faultyTx) PutSystemAlert(ctx context.Context, a store.SystemAlert) error {
	if f.fail == "PutSystemAlert" {
		return errInjected
	}
	return f.StateTx.PutSystemAlert(ctx, a)
}

func (f faultyTx) PutMonitorState(ctx context.Context, tenantID, monitorID string, st store.MonitorState) error {
	if f.fail == "PutMonitorState" {
		return errInjected
	}
	return f.StateTx.PutMonitorState(ctx, tenantID, monitorID, st)
}

func (f faultyTx) PutSecretKey(ctx context.Context, k store.SecretKey) error {
	if f.fail == "PutSecretKey" {
		return errInjected
	}
	return f.StateTx.PutSecretKey(ctx, k)
}

// inTx runs fn in a state transaction that is rolled back afterwards, so that
// helpers can be called with a faultyTx without leaving writes behind.
func inTx(t *testing.T, h *alertFSM, fn func(tx store.StateTx)) {
	t.Helper()
	errRollback := errors.New("rollback")
	err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		fn(tx)
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("state transaction: %v", err)
	}
}

// TestPutEntityRejectsMalformedCommands covers the rejections of PutEntity
// commands whose document or kind is invalid, and of commands without payload.
func TestPutEntityRejectsMalformedCommands(t *testing.T) {
	t.Parallel()
	raw := func(kind pb.EntityKind, tenantID, id, doc string) *pb.Command {
		return stamp(&pb.Command{
			RequestId: fmt.Sprint("raw-", kind, "-", id),
			TenantId:  tenantID,
			Payload: &pb.Command_PutEntity{PutEntity: &pb.PutEntity{
				Kind: kind, Id: id, DocumentJson: []byte(doc),
			}},
		})
	}
	tests := []struct {
		name     string
		cmd      *pb.Command
		wantCode string
	}{
		{"command without payload", stamp(&pb.Command{RequestId: "no-payload"}), CodeInvalid},
		{"entity kind not supported", raw(pb.EntityKind_ENTITY_KIND_UNSPECIFIED, "t1", "x1", "{}"), CodeUnsupported},
		{"tenant document not JSON", raw(pb.EntityKind_ENTITY_KIND_TENANT, "", "t9", "{"), CodeInvalid},
		{"monitor without tenant", must(NewPutMonitor("mon-no-tenant", "", "m1", monitorDoc("Web"), 0)), CodeInvalid},
		{"monitor document not JSON", raw(pb.EntityKind_ENTITY_KIND_MONITOR, "t1", "m1", "{"), CodeInvalid},
		{"channel document not JSON", raw(pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL, "t1", "c1", "{"), CodeInvalid},
		{"maintenance document not JSON", raw(pb.EntityKind_ENTITY_KIND_MAINTENANCE_WINDOW, "t1", "w1", "{"), CodeInvalid},
		{"secret key document not JSON", raw(pb.EntityKind_ENTITY_KIND_SECRET_KEY, "", "k1", "{"), CodeInvalid},
		{"secret key without wrapped key", must(NewPutSecretKey("sk-empty", "k1", SecretKeyDocument{KEKFingerprint: "fp"})), CodeInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := setupProbeAlertFSM(t, ProbeCommandLevel)
			checkCode(t, h.apply(tt.cmd, 10_000).Err, tt.wantCode)
		})
	}
}

// TestPutChannelRejectsStaleRevision covers a replacement that names an older
// revision than the stored one: the store conflicts and the FSM reports it.
func TestPutChannelRejectsStaleRevision(t *testing.T) {
	t.Parallel()
	h := setupProbeAlertFSM(t, ProbeCommandLevel)
	h.putChannelAt(1200, "ch-create", "t1", "c1", []string{store.EventProbeOffline}, 0, "")
	h.putChannelAt(1300, "ch-again", "t1", "c1", []string{store.EventProbeOffline}, 0, CodeConflict)
}

// TestPutHelpersReportStoreErrors covers the store failures of the put helpers,
// which the FSM cannot produce from valid documents on a healthy store.
func TestPutHelpersReportStoreErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  *pb.Command
		fail string
		run  func(ctx context.Context, tx store.StateTx, cmd *pb.Command) error
	}{
		{
			name: "tenant write fails",
			cmd:  must(NewPutTenant("ten-fail", "t9", TenantDocument{Name: "Nine", Plan: "default"})),
			fail: "PutTenant",
			run: func(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
				_, err := applyPutTenantEntity(ctx, tx, cmd, cmd.GetPutEntity(), 10_000)
				return err
			},
		},
		{
			name: "monitor state sync fails",
			cmd:  must(NewPutMonitor("mon-sync", "t1", "m1", monitorDoc("Web"), 0)),
			fail: "PutMonitorState",
			run: func(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
				_, err := applyPutMonitorEntity(ctx, tx, cmd, cmd.GetPutEntity(), 10_000)
				return err
			},
		},
		{
			name: "channel level unreadable",
			cmd:  must(NewPutChannel("ch-level", "t1", "c9", channel(store.EventProbeOffline), 0)),
			fail: "CommandLevel",
			run: func(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
				_, err := applyPutChannelEntity(ctx, tx, cmd, cmd.GetPutEntity(), 10_000)
				return err
			},
		},
		{
			name: "secret key write fails",
			cmd:  must(NewPutSecretKey("sk-store", "k9", SecretKeyDocument{WrappedDEK: []byte("wrapped"), KEKFingerprint: "fp"})),
			fail: "PutSecretKey",
			run: func(ctx context.Context, tx store.StateTx, cmd *pb.Command) error {
				_, err := applyPutSecretKeyEntity(ctx, tx, cmd.GetPutEntity(), 10_000)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := setupProbeAlertFSM(t, ProbeCommandLevel)
			inTx(t, h, func(tx store.StateTx) {
				err := tt.run(h.ctx, faultyTx{StateTx: tx, fail: tt.fail}, tt.cmd)
				if !errors.Is(err, errInjected) {
					t.Errorf("err = %v, want the injected store failure", err)
				}
			})
		})
	}
}

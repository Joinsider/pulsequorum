// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"encoding/json"
	"errors"
	"testing"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// putSystemTenant creates the reserved system tenant; only the system actor may (ADR 0025).
func (h *alertFSM) putSystemTenant(issuedAt int64) {
	h.t.Helper()
	cmd, err := NewPutTenant("tenant-"+store.SystemTenantID, store.SystemTenantID, TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		h.t.Fatal(err)
	}
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	h.do(cmd, issuedAt, nil)
}

// sysSetup returns an FSM at command level 6 with the system tenant and the
// channel "sys" of it, subscribed to the system event.
func sysSetup(t *testing.T) *alertFSM {
	t.Helper()
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(SystemAlertCommandLevel)
	h.putSystemTenant(1000)
	h.putChannel(1000, store.SystemTenantID, "sys", channel(store.EventSystem), nil)
	return h
}

// systemAlert reads a system alert in a read-only transaction.
func (h *alertFSM) systemAlert(kind, subject string) (store.SystemAlert, error) {
	h.t.Helper()
	var a store.SystemAlert
	err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		var err error
		a, err = tx.GetSystemAlert(h.ctx, kind, subject)
		return err
	})
	return a, err
}

// setAlert applies a SetSystemAlert and returns whether it changed the alert.
func (h *alertFSM) setAlert(issuedAt int64, kind, subject string, active bool, level int, details string) bool {
	h.t.Helper()
	var raw json.RawMessage
	if details != "" {
		raw = json.RawMessage(details)
	}
	res := h.apply(NewSetSystemAlert(kind, subject, active, level, raw), issuedAt)
	if res.Err != nil {
		h.t.Fatalf("SetSystemAlert(%s, %s): %v", kind, subject, res.Err)
	}
	var r SystemAlertResult
	if err := json.Unmarshal(res.Result, &r); err != nil {
		h.t.Fatal(err)
	}
	return r.Changed
}

// TestSystemCommandsNeedLevel6 covers ADR 0025 and ADR 0018: the three commands
// are rejected below command level 6 and write nothing.
func TestSystemCommandsNeedLevel6(t *testing.T) {
	tests := []struct {
		name string
		cmd  func() *pb.Command
	}{
		{"set system alert", func() *pb.Command {
			return NewSetSystemAlert("replication_lag", "n2", true, 0, nil)
		}},
		{"record incarnation", func() *pb.Command { return NewRecordIncarnation("n2", "inc-1") }},
		{"checkpoint", NewCheckpoint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newAlertFSM(t, t.TempDir())
			h.setLevel(SystemAlertCommandLevel - 1)
			h.putSystemTenant(1000)
			h.putChannel(1000, store.SystemTenantID, "sys", channel(store.EventSystem), ErrInvalidCommand)
			h.do(tt.cmd(), 2000, ErrInvalidCommand)
			if alerts, _ := h.st.ListSystemAlerts(h.ctx, false); len(alerts) != 0 {
				t.Errorf("system alerts stored below level 6: %+v", alerts)
			}
			if incs, _ := h.st.ListNodeIncarnations(h.ctx); len(incs) != 0 {
				t.Errorf("incarnations stored below level 6: %+v", incs)
			}
			if idx, fp := h.fsm.LastCheckpoint(); idx != 0 || fp != "" {
				t.Errorf("checkpoint below level 6: %d %q", idx, fp)
			}
		})
	}

	t.Run("accepted at level 6", func(t *testing.T) {
		h := sysSetup(t)
		h.do(NewRecordIncarnation("n2", "inc-1"), 2000, nil)
		h.do(NewCheckpoint(), 3000, nil)
		h.do(NewSetSystemAlert("replication_lag", "n2", true, 0, nil), 4000, nil)
	})
}

// TestSetSystemAlertValidation covers the checks of a SetSystemAlert command.
func TestSetSystemAlertValidation(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		subject string
		level   int
		details string
		want    error
	}{
		{"no details", "disk_low", "n1", 0, "", nil},
		{"object details", "disk_low", "n2", 0, `{"freeRatio":0.1}`, nil},
		{"certificate stage", "cert_expiry", "node-cert", 2, `{"notAfterMs":1}`, nil},
		{"no kind", "", "n1", 0, "", ErrInvalidCommand},
		{"no subject", "disk_low", "", 0, "", ErrInvalidCommand},
		{"negative level", "disk_low", "n3", -1, "", ErrInvalidCommand},
		{"array details", "disk_low", "n4", 0, `[]`, ErrInvalidCommand},
		{"null details", "disk_low", "n5", 0, `null`, ErrInvalidCommand},
		{"scalar details", "disk_low", "n6", 0, `"x"`, ErrInvalidCommand},
		{"broken details", "disk_low", "n7", 0, `{"a":`, ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := sysSetup(t)
			var raw json.RawMessage
			if tt.details != "" {
				raw = json.RawMessage(tt.details)
			}
			h.do(NewSetSystemAlert(tt.kind, tt.subject, true, tt.level, raw), 2000, tt.want)
		})
	}

	t.Run("details are compacted", func(t *testing.T) {
		h := sysSetup(t)
		h.setAlert(2000, "disk_low", "n1", true, 0, "{ \"freeRatio\" :\n 0.1 }")
		a, err := h.systemAlert("disk_low", "n1")
		if err != nil {
			t.Fatal(err)
		}
		if string(a.DetailsJSON) != `{"freeRatio":0.1}` {
			t.Errorf("details = %s, want compacted", a.DetailsJSON)
		}
	})
}

// TestSystemAlertTransitions covers the rules of SetSystemAlert: no-op on a
// repeat, since time and key on activation, level change and clear.
func TestSystemAlertTransitions(t *testing.T) {
	h := sysSetup(t)
	const kind, subject = "replication_lag", "n2"
	since := func() int64 {
		t.Helper()
		a, err := h.systemAlert(kind, subject)
		if err != nil {
			t.Fatal(err)
		}
		return a.SinceMs
	}

	// Activation starts a period and enqueues one notification.
	if !h.setAlert(10_000, kind, subject, true, 0, `{"lagMs":300000}`) {
		t.Fatal("activation not changed")
	}
	a := h.alert("sys:replication_lag:n2:10000:active", "sys")
	h.checkPayload(a, map[string]any{"event": "system", "tenantId": store.SystemTenantID, "confirmed": true})
	if a.MonitorID != "" || a.TenantID != store.SystemTenantID {
		t.Errorf("outbox entry = tenant %q monitor %q, want the system tenant without monitor", a.TenantID, a.MonitorID)
	}
	var p AlertPayload
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.System == nil || p.System.Kind != kind || p.System.Subject != subject || !p.System.Active || p.System.Level != 0 ||
		string(p.System.Details) != `{"lagMs":300000}` || p.SinceMs != 10_000 {
		t.Errorf("payload = %+v, want the system info of the activation", p)
	}

	// The same flag and level change nothing, not even the details.
	if h.setAlert(20_000, kind, subject, true, 0, `{"lagMs":400000}`) {
		t.Error("repeated activation changed the alert")
	}
	h.noAlert("sys:replication_lag:n2:20000:active", "sys")
	if since() != 10_000 {
		t.Errorf("since after repeat = %d, want 10000", since())
	}

	// A new level restarts the period and carries the level in the key.
	if !h.setAlert(30_000, kind, subject, true, 2, "") {
		t.Fatal("level change not changed")
	}
	h.alert("sys:replication_lag:n2:30000:L2", "sys")
	if since() != 30_000 {
		t.Errorf("since after level change = %d, want 30000", since())
	}

	// A clear keeps the start of the period and ends it with "clear".
	if !h.setAlert(40_000, kind, subject, false, 0, "") {
		t.Fatal("clear not changed")
	}
	h.alert("sys:replication_lag:n2:30000:clear", "sys")
	if since() != 30_000 {
		t.Errorf("since after clear = %d, want 30000", since())
	}
	if h.setAlert(50_000, kind, subject, false, 0, "") {
		t.Error("second clear changed the alert")
	}
	h.noAlert("sys:replication_lag:n2:50000:clear", "sys")

	// A new activation after a clear starts a new period.
	if !h.setAlert(60_000, kind, subject, true, 0, "") {
		t.Fatal("reactivation not changed")
	}
	h.alert("sys:replication_lag:n2:60000:active", "sys")

	// A clear of an alert that was never active is a no-op and stores nothing.
	if h.setAlert(70_000, "disk_low", "n9", false, 0, "") {
		t.Error("clear of an unknown alert changed the state")
	}
	if _, err := h.systemAlert("disk_low", "n9"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown alert stored: %v", err)
	}
}

// TestSystemAlertOnlyToSystemChannels covers FA-OBS-01 and the tenant boundary:
// a system alert reaches only enabled channels of the system tenant that
// subscribe to "system", never a tenant channel.
func TestSystemAlertOnlyToSystemChannels(t *testing.T) {
	h := sysSetup(t)
	h.putTenant(1000, "t1")
	h.putChannel(1000, "t1", "a", channel(store.EventDown, store.EventUp), nil)
	h.putChannel(1000, store.SystemTenantID, "quiet", channel(store.EventDown), nil)
	h.putChannel(1000, store.SystemTenantID, "off", channel(store.EventSystem), nil)
	if err := h.st.Update(h.ctx, func(tx store.StateTx) error {
		return tx.SetChannelState(h.ctx, store.SystemTenantID, "off", store.ChannelDisabled, "gone")
	}); err != nil {
		t.Fatal(err)
	}

	h.setAlert(10_000, "clock_skew", "cluster", true, 0, "")
	key := "sys:clock_skew:cluster:10000:active"
	h.alert(key, "sys")
	h.noAlert(key, "a")
	h.noAlert(key, "quiet")
	h.noAlert(key, "off")

	// Tenant channels may not subscribe to system alerts, and below level 6 no
	// channel may.
	h.putChannel(11_000, "t1", "bad", channel(store.EventSystem), ErrInvalidCommand)
}

// TestSystemChannelEventNeedsLevel6 covers the level gate of the system event.
func TestSystemChannelEventNeedsLevel6(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())
	h.setLevel(SystemAlertCommandLevel - 1)
	h.putSystemTenant(1000)
	h.putChannel(1000, store.SystemTenantID, "sys", channel(store.EventSystem), ErrInvalidCommand)
}

// TestRecordIncarnation covers FA-REP-06: the last reported incarnation of a
// node is stored, and a return to an older one is a change too.
func TestRecordIncarnation(t *testing.T) {
	h := sysSetup(t)
	h.do(NewRecordIncarnation("n2", "inc-1"), 2000, nil)
	h.do(NewRecordIncarnation("n2", "inc-2"), 3000, nil)
	h.do(NewRecordIncarnation("n2", "inc-1"), 4000, nil)

	incs, err := h.st.ListNodeIncarnations(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(incs) != 1 || incs[0] != (store.NodeIncarnation{NodeID: "n2", Incarnation: "inc-1", SeenMs: 4000}) {
		t.Errorf("incarnations = %+v, want n2 inc-1 seen at 4000", incs)
	}

	h.do(NewRecordIncarnation("", "inc-1"), 5000, ErrInvalidCommand)
	h.do(NewRecordIncarnation("n3", ""), 5000, ErrInvalidCommand)
}

// replayEntry is one command of a log with its command time.
type replayEntry struct {
	cmd *pb.Command
	at  int64
}

// replay applies the entries to h, in order, at the next indexes.
func replay(h *alertFSM, entries []replayEntry) {
	h.t.Helper()
	for _, e := range entries {
		h.do(e.cmd, e.at, nil)
	}
}

// TestCheckpointAgreesAcrossReplicas covers FA-CLU-07 and ADR 0025: replicas
// that applied the same log compute the same fingerprint at a checkpoint, a
// replica that diverged does not, and the value equals the committed state.
func TestCheckpointAgreesAcrossReplicas(t *testing.T) {
	// The commands are built once, so that every replica gets the same bytes.
	log := []replayEntry{
		{NewSetFeatureLevel("level-6", SystemAlertCommandLevel), 1000},
		{mustSystemTenant(t), 1000},
		{mustSysChannel(t), 1000},
		{mustPutTenant(t, "t1"), 1000},
		{NewSetSystemAlert("replication_lag", "n2", true, 0, json.RawMessage(`{"lagMs":300000}`)), 10_000},
		{NewRecordIncarnation("n2", "inc-1"), 11_000},
	}
	cp := NewCheckpoint()
	build := func(t *testing.T, entries []replayEntry) *alertFSM {
		t.Helper()
		h := newAlertFSM(t, t.TempDir())
		replay(h, entries)
		h.do(cp, 12_000, nil)
		return h
	}
	a := build(t, log)
	b := build(t, log)

	idxA, fpA := a.fsm.LastCheckpoint()
	idxB, fpB := b.fsm.LastCheckpoint()
	if idxA == 0 || fpA == "" {
		t.Fatalf("checkpoint not recorded: %d %q", idxA, fpA)
	}
	if idxA != idxB || fpA != fpB {
		t.Errorf("replicas disagree: %d %q vs %d %q", idxA, fpA, idxB, fpB)
	}
	if idxA != a.idx {
		t.Errorf("checkpoint index = %d, want the applied index %d", idxA, a.idx)
	}
	committed, err := a.st.Fingerprint(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if committed != fpA {
		t.Errorf("checkpoint fingerprint %q differs from the committed state %q", fpA, committed)
	}

	// A replica that applied a different incarnation diverges.
	diverged := append(append([]replayEntry{}, log[:5]...), replayEntry{NewRecordIncarnation("n2", "inc-X"), 11_000})
	c := build(t, diverged)
	if _, fpC := c.fsm.LastCheckpoint(); fpC == fpA {
		t.Errorf("diverged replica has the same fingerprint %q", fpC)
	}
}

func mustSystemTenant(t *testing.T) *pb.Command {
	t.Helper()
	cmd, err := NewPutTenant("tenant-"+store.SystemTenantID, store.SystemTenantID, TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	return cmd
}

func mustSysChannel(t *testing.T) *pb.Command {
	t.Helper()
	cmd, err := NewPutChannel("channel-sys", store.SystemTenantID, "sys", channel(store.EventSystem), 0)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func mustPutTenant(t *testing.T, id string) *pb.Command {
	t.Helper()
	cmd, err := NewPutTenant("tenant-"+id, id, TenantDocument{Name: id, Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// TestCheckpointWritesNoAlert covers the promise of a checkpoint: it changes no
// alert and enqueues nothing, and it records the index of its own command.
func TestCheckpointWritesNoAlert(t *testing.T) {
	h := sysSetup(t)
	h.setAlert(2000, "disk_low", "n1", true, 0, "")
	outboxBefore := len(h.outbox(t))
	h.do(NewCheckpoint(), 3000, nil)
	h.do(NewCheckpoint(), 4000, nil)
	idx, fp := h.fsm.LastCheckpoint()
	if idx != h.idx || fp == "" {
		t.Fatalf("checkpoint = %d %q, want index %d", idx, fp, h.idx)
	}
	if alerts, _ := h.st.ListSystemAlerts(h.ctx, false); len(alerts) != 1 {
		t.Errorf("alerts after checkpoints = %+v, want the one set before", alerts)
	}
	if n := len(h.outbox(t)); n != outboxBefore {
		t.Errorf("outbox grew from %d to %d by checkpoints", outboxBefore, n)
	}
}

// outbox returns every outbox entry of the system tenant.
func (h *alertFSM) outbox(t *testing.T) []store.AlertDelivery {
	t.Helper()
	due, err := h.st.DueAlerts(h.ctx, 1<<62, 500)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

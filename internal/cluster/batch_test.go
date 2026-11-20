// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// putOp builds a batch operation that puts a document of kind with id at the
// expected revision.
func putOp(kind pb.EntityKind, id string, doc any, rev uint64) *pb.BatchOp {
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return &pb.BatchOp{Op: &pb.BatchOp_PutEntity{PutEntity: &pb.PutEntity{
		Kind: kind, Id: id, DocumentJson: raw, ExpectedRevision: rev,
	}}}
}

// deleteOp builds a batch operation that deletes the entity of kind with id.
func deleteOp(kind pb.EntityKind, id string) *pb.BatchOp {
	return &pb.BatchOp{Op: &pb.BatchOp_DeleteEntity{DeleteEntity: &pb.DeleteEntity{Kind: kind, Id: id}}}
}

// monitorOp builds a batch operation that sets the interval of monitor id.
func monitorOp(id string, interval int, rev uint64) *pb.BatchOp {
	doc := monitorDoc("Web " + id)
	doc.IntervalSeconds = interval
	return putOp(pb.EntityKind_ENTITY_KIND_MONITOR, id, doc, rev)
}

// monitorDeleteOp builds a batch operation that deletes monitor id.
func monitorDeleteOp(id string) *pb.BatchOp {
	return deleteOp(pb.EntityKind_ENTITY_KIND_MONITOR, id)
}

// withHint sets the audit_action of a batch operation.
func withHint(op *pb.BatchOp, hint string) *pb.BatchOp {
	op.AuditAction = hint
	return op
}

// batchFSM returns a token FSM with the monitors m1 (interval 60, revision 1),
// m2 and m3 of tenant t1, created by a signed-in user.
func batchFSM(t *testing.T) *alertFSM {
	t.Helper()
	h := tokenFSM(t)
	for _, id := range []string{"m1", "m2", "m3"} {
		h.putMonitorAt(1100, "create-"+id, "t1", id, 60, 0, "")
	}
	return h
}

// intervalOf returns the interval of monitor id of t1 or fails the test.
func (h *alertFSM) intervalOf(id string) int {
	h.t.Helper()
	m, err := h.st.GetMonitor(h.ctx, "t1", id)
	if err != nil {
		h.t.Fatal(err)
	}
	return m.IntervalSeconds
}

// TestBatchAppliesEveryOperation covers the success case of ADR 0031, section 3:
// a pause with a hint, an update and a delete apply together, and the result
// lists one PutResult per operation in order.
func TestBatchAppliesEveryOperation(t *testing.T) {
	h := batchFSM(t)
	paused := monitorDoc("Web m1")
	paused.Paused = true
	ops := []*pb.BatchOp{
		withHint(putOp(pb.EntityKind_ENTITY_KIND_MONITOR, "m1", paused, 1), "pause"),
		monitorOp("m2", 120, 1),
		monitorDeleteOp("m3"),
	}
	res := h.apply(NewBatch("batch-1", "t1", userActor(), ops), 2000)
	checkCode(t, res.Err, "")

	var got BatchResult
	if err := json.Unmarshal(res.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	want := []PutResult{{ID: "m1", Revision: 2}, {ID: "m2", Revision: 2}, {ID: "m3"}}
	if len(got.Results) != len(want) {
		t.Fatalf("results = %+v, want %+v", got.Results, want)
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got.Results[i], want[i])
		}
	}
	if st := h.monitorState("m1").Status; st != store.StatusPaused {
		t.Errorf("m1 status = %s, want PAUSED", st)
	}
	if iv := h.intervalOf("m2"); iv != 120 {
		t.Errorf("m2 interval = %d, want 120", iv)
	}
	if _, err := h.st.GetMonitor(h.ctx, "t1", "m3"); err == nil {
		t.Error("m3 still exists after the batch deleted it")
	}
}

// TestBatchIsAtomic covers the all-or-nothing rule: a stale revision or a
// missing monitor in any operation applies none of them, and the error names
// the operation and the monitor.
func TestBatchIsAtomic(t *testing.T) {
	tests := []struct {
		name    string
		ops     []*pb.BatchOp
		code    string
		message string
	}{
		{"stale revision in the second operation", []*pb.BatchOp{
			monitorOp("m1", 300, 1), monitorOp("m2", 300, 5), monitorDeleteOp("m3"),
		}, CodeConflict, "operation 1 (monitor m2)"},
		{"missing monitor in the last operation", []*pb.BatchOp{
			monitorOp("m1", 300, 1), monitorDeleteOp("m9"),
		}, CodeNotFound, "operation 1 (monitor m9)"},
		{"create of an existing monitor", []*pb.BatchOp{
			monitorOp("m1", 300, 1), monitorOp("m2", 300, 0),
		}, CodeConflict, "operation 1 (monitor m2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := batchFSM(t)
			res := h.apply(NewBatch("batch-atomic", "t1", userActor(), tt.ops), 2000)
			checkCode(t, res.Err, tt.code)
			if !strings.Contains(res.Err.Error(), tt.message) {
				t.Errorf("error = %q, want it to name %q", res.Err, tt.message)
			}
			if iv := h.intervalOf("m1"); iv != 60 {
				t.Errorf("m1 interval = %d after a rejected batch, want 60", iv)
			}
			if _, err := h.st.GetMonitor(h.ctx, "t1", "m3"); err != nil {
				t.Errorf("m3 changed by a rejected batch: %v", err)
			}
		})
	}
}

// TestBatchValidation covers the rules checked before any operation runs.
// Nothing changes in any case.
func TestBatchValidation(t *testing.T) {
	tooMany := make([]*pb.BatchOp, MaxBatchOps+1)
	for i := range tooMany {
		tooMany[i] = monitorDeleteOp("m1")
	}
	channelPut := putOp(pb.EntityKind_ENTITY_KIND_NOTIFICATION_CHANNEL, "ch1", channel("down"), 0)
	userDelete := deleteOp(pb.EntityKind_ENTITY_KIND_USER, "u1")
	tests := []struct {
		name      string
		actor     *pb.Actor
		tn        string
		ops       []*pb.BatchOp
		batchHint string
	}{
		{"no operations", userActor(), "t1", nil, ""},
		{"more than 500 operations", userActor(), "t1", tooMany, ""},
		{"channel put", userActor(), "t1", []*pb.BatchOp{monitorOp("m1", 300, 1), channelPut}, ""},
		{"user delete", userActor(), "t1", []*pb.BatchOp{userDelete}, ""},
		{"operation without put or delete", userActor(), "t1", []*pb.BatchOp{{}}, ""},
		{"hint on a delete", userActor(), "t1", []*pb.BatchOp{withHint(monitorDeleteOp("m1"), "pause")}, ""},
		{"unknown hint", userActor(), "t1", []*pb.BatchOp{withHint(monitorOp("m1", 300, 1), "archive")}, ""},
		{"hint on a channel", userActor(), "t1", []*pb.BatchOp{withHint(channelPut, "pause")}, ""},
		{"hint on the batch itself", userActor(), "t1", []*pb.BatchOp{monitorOp("m1", 300, 1)}, "pause"},
		{"system actor", &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}, "t1", []*pb.BatchOp{monitorOp("m1", 300, 1)}, ""},
		{"actor of unspecified kind", &pb.Actor{}, "t1", []*pb.BatchOp{monitorOp("m1", 300, 1)}, ""},
		{"no tenant", userActor(), "", []*pb.BatchOp{monitorOp("m1", 300, 1)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := batchFSM(t)
			cmd := NewBatch("batch-invalid", tt.tn, tt.actor, tt.ops)
			cmd.AuditAction = tt.batchHint
			res := h.apply(cmd, 2000)
			checkCode(t, res.Err, CodeInvalid)
			if iv := h.intervalOf("m1"); iv != 60 {
				t.Errorf("m1 interval = %d after a rejected batch, want 60", iv)
			}
		})
	}
}

// TestBatchAcceptsAPITokenActor covers ADR 0031, section 1: a token may change
// monitors, also in a batch.
func TestBatchAcceptsAPITokenActor(t *testing.T) {
	h := batchFSM(t)
	actor := &pb.Actor{Kind: pb.Actor_KIND_API_TOKEN, Id: tokID(1), Name: "ci"}
	res := h.apply(NewBatch("batch-token", "t1", actor, []*pb.BatchOp{monitorOp("m1", 300, 1)}), 2000)
	checkCode(t, res.Err, "")
	if iv := h.intervalOf("m1"); iv != 300 {
		t.Errorf("m1 interval = %d, want 300", iv)
	}
}

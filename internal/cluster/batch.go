// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// MaxBatchOps is the largest number of operations of one Batch (ADR 0031, section 3).
const MaxBatchOps = 500

// BatchResult is the result of Batch: one PutResult per operation, in the order
// of the operations.
type BatchResult struct {
	Results []PutResult `json:"results"`
}

// batchOpProblem returns why op is not allowed in a Batch, or "". Only monitor
// puts and deletes are allowed (ADR 0031, section 3).
func batchOpProblem(op *pb.BatchOp) string {
	switch {
	case op.GetPutEntity() != nil:
		if op.GetPutEntity().GetKind() != pb.EntityKind_ENTITY_KIND_MONITOR {
			return "only monitors can be changed in a batch"
		}
	case op.GetDeleteEntity() != nil:
		if op.GetDeleteEntity().GetKind() != pb.EntityKind_ENTITY_KIND_MONITOR {
			return "only monitors can be changed in a batch"
		}
	default:
		return "operation without put or delete"
	}
	return ""
}

// applyBatch applies the operations of a Batch in one transaction: all of them
// or none (ADR 0031, section 3). Each operation runs through the same code as a
// single command, with the same checks (limits, revisions, actor rules). The
// first failing operation rejects the whole batch with its position and ID.
func applyBatch(ctx context.Context, tx store.StateTx, cmd *pb.Command, b *pb.Batch) (json.RawMessage, error) {
	if err := requireCommandLevel(ctx, tx, "batch commands", AuditCommandLevel); err != nil {
		return nil, err
	}
	switch cmd.GetActor().GetKind() {
	case pb.Actor_KIND_USER, pb.Actor_KIND_API_TOKEN:
	default:
		return nil, invalid("batch commands are only allowed for users and API tokens")
	}
	if cmd.GetTenantId() == "" {
		return nil, invalid("batch command without tenant")
	}
	ops := b.GetOps()
	if len(ops) == 0 || len(ops) > MaxBatchOps {
		return nil, invalid("a batch needs 1 to %d operations, got %d", MaxBatchOps, len(ops))
	}
	for i, op := range ops {
		if msg := batchOpProblem(op); msg != "" {
			return nil, invalid("operation %d: %s", i, msg)
		}
	}

	results := make([]PutResult, 0, len(ops))
	for i, op := range ops {
		var raw json.RawMessage
		var err error
		var id string
		if put := op.GetPutEntity(); put != nil {
			id = put.GetId()
			raw, err = applyPut(ctx, tx, cmd, put)
		} else {
			id = op.GetDeleteEntity().GetId()
			raw, err = applyDelete(ctx, tx, cmd, op.GetDeleteEntity())
		}
		if err != nil {
			var cmdErr *CommandError
			if errors.As(err, &cmdErr) {
				return nil, &CommandError{Code: cmdErr.Code, Message: fmt.Sprintf("operation %d (monitor %s): %s", i, id, cmdErr.Message)}
			}
			return nil, err
		}
		var res PutResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("decode result of operation %d: %w", i, err)
		}
		results = append(results, res)
	}
	return json.Marshal(BatchResult{Results: results})
}

// NewBatch builds the command that applies ops atomically to the monitors of
// tenantID (ADR 0031, section 3). The operations carry their expected revisions
// and optional audit hints.
func NewBatch(requestID, tenantID string, actor *pb.Actor, ops []*pb.BatchOp) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		TenantId:  tenantID,
		Actor:     actor,
		Payload:   &pb.Command_Batch{Batch: &pb.Batch{Ops: ops}},
	})
}

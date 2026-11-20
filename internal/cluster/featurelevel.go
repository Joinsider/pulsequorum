// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// FeatureLevelResult is the result of SetFeatureLevel: the command level of
// the cluster after the command (ADR 0018).
type FeatureLevelResult struct {
	Level uint32 `json:"level"`
}

// NewSetFeatureLevel builds the command that raises the cluster command level
// to level (ADR 0018). The leader proposes it once all voters support the level.
func NewSetFeatureLevel(requestID string, level uint32) *pb.Command {
	return stamp(&pb.Command{
		RequestId: requestID,
		Payload:   &pb.Command_SetFeatureLevel{SetFeatureLevel: &pb.SetFeatureLevel{Level: level}},
	})
}

// applySetFeatureLevel raises the stored command level to level. A level that
// is not above the current one is a success without a write, so the level only
// ever grows.
func applySetFeatureLevel(ctx context.Context, tx store.StateTx, _ *pb.Command, s *pb.SetFeatureLevel) (json.RawMessage, error) {
	if s.GetLevel() == 0 {
		return nil, invalid("feature level must be at least 1")
	}
	stored, err := tx.CommandLevel(ctx)
	if err != nil {
		return nil, fmt.Errorf("read command level: %w", err)
	}
	current := EffectiveCommandLevel(stored)
	if s.GetLevel() > current {
		if err := tx.SetCommandLevel(ctx, s.GetLevel()); err != nil {
			return nil, fmt.Errorf("store command level: %w", err)
		}
		current = s.GetLevel()
	}
	return json.Marshal(FeatureLevelResult{Level: current})
}

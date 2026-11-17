// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// ListNodes returns the node registry ordered by node ID (ADR 0017).
func (s *StateStore) ListNodes(ctx context.Context) ([]store.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return listNodes(ctx, s.q)
}

// ListNodes returns the node registry inside the transaction (ADR 0024).
func (t *stateTx) ListNodes(ctx context.Context) ([]store.Node, error) {
	return listNodes(ctx, t.q)
}

func listNodes(ctx context.Context, q *statedb.Queries) ([]store.Node, error) {
	rows, err := q.ListNodes(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]store.Node, 0, len(rows))
	for _, r := range rows {
		n, err := nodeFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// GetNode returns a registered node or store.ErrNotFound.
func (t *stateTx) GetNode(ctx context.Context, id string) (store.Node, error) {
	r, err := t.q.GetNode(ctx, id)
	if err != nil {
		return store.Node{}, mapErr(err)
	}
	return nodeFromRow(r)
}

// PutNode creates or replaces a registry entry. Capabilities are stored sorted.
func (t *stateTx) PutNode(ctx context.Context, n store.Node) error {
	if n.ID == "" {
		return errors.New("put node: node ID is required")
	}
	caps := slices.Clone(n.Capabilities)
	if caps == nil {
		caps = []string{}
	}
	slices.Sort(caps)
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return fmt.Errorf("put node %s: encode capabilities: %w", n.ID, err)
	}
	labelsJSON, err := json.Marshal(nonNilMap(n.Labels))
	if err != nil {
		return fmt.Errorf("put node %s: encode labels: %w", n.ID, err)
	}
	return mapErr(t.q.PutNode(ctx, statedb.PutNodeParams{
		ID:                    n.ID,
		Name:                  n.Name,
		Role:                  n.Role,
		CapabilitiesJson:      string(capsJSON),
		LabelsJson:            string(labelsJSON),
		Region:                n.Region,
		Version:               n.Version,
		MaxCommandVersion:     int64(n.MaxCommandVersion),
		Healthy:               boolToInt(n.Healthy),
		HealthChangedMs:       n.HealthChangedMs,
		RegisteredMs:          n.RegisteredMs,
		UpdatedMs:             n.UpdatedMs,
		ConnectivityImpaired:  boolToInt(n.ConnectivityImpaired),
		ConnectivityReason:    n.ConnectivityReason,
		ConnectivityChangedMs: n.ConnectivityChangedMs,
	}))
}

// SetNodeConnectivity records the connectivity of a registered node (ADR 0022).
// It sets the time of the change as UpdatedMs too, like a health change does.
func (t *stateTx) SetNodeConnectivity(ctx context.Context, nodeID string, impaired bool, reason string, atMs int64) error {
	n, err := t.q.SetNodeConnectivity(ctx, statedb.SetNodeConnectivityParams{
		ConnectivityImpaired:  boolToInt(impaired),
		ConnectivityReason:    reason,
		ConnectivityChangedMs: atMs,
		UpdatedMs:             atMs,
		ID:                    nodeID,
	})
	if err != nil {
		return mapErr(err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteNode removes a registry entry; an unknown ID is not an error.
func (t *stateTx) DeleteNode(ctx context.Context, id string) error {
	_, err := t.q.DeleteNode(ctx, id)
	return mapErr(err)
}

func nodeFromRow(r statedb.Node) (store.Node, error) {
	var caps []string
	if err := json.Unmarshal([]byte(r.CapabilitiesJson), &caps); err != nil {
		return store.Node{}, fmt.Errorf("decode node %s capabilities: %w", r.ID, err)
	}
	if caps == nil {
		caps = []string{}
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(r.LabelsJson), &labels); err != nil {
		return store.Node{}, fmt.Errorf("decode node %s labels: %w", r.ID, err)
	}
	if r.MaxCommandVersion < 0 || r.MaxCommandVersion > math.MaxUint32 {
		return store.Node{}, fmt.Errorf("decode node %s: max command version %d out of range", r.ID, r.MaxCommandVersion)
	}
	return store.Node{
		ID:                    r.ID,
		Name:                  r.Name,
		Role:                  r.Role,
		Capabilities:          caps,
		Labels:                nonNilMap(labels),
		Region:                r.Region,
		Version:               r.Version,
		MaxCommandVersion:     uint32(r.MaxCommandVersion),
		Healthy:               r.Healthy != 0,
		HealthChangedMs:       r.HealthChangedMs,
		ConnectivityImpaired:  r.ConnectivityImpaired != 0,
		ConnectivityReason:    r.ConnectivityReason,
		ConnectivityChangedMs: r.ConnectivityChangedMs,
		RegisteredMs:          r.RegisteredMs,
		UpdatedMs:             r.UpdatedMs,
	}, nil
}

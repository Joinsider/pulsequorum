// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/joinsider/uptimecluster/internal/store"
)

// DefaultTenantID is the only tenant in local auth mode (E13, FA-AUTH-01).
const DefaultTenantID = "default"

// bootstrapTenantRequestID makes the creation idempotent: a retry or a second
// node attempting it is deduplicated by the FSM.
const bootstrapTenantRequestID = "bootstrap:tenant:" + DefaultTenantID

// Ready reports whether the node can serve: the FSM works and a leader is known.
func (n *Node) Ready() error {
	if err := n.fsm.Healthy(); err != nil {
		return err
	}
	if addr, _ := n.raft.LeaderWithID(); addr == "" {
		return errors.New("raft: no leader")
	}
	return nil
}

// EnsureDefaultTenant waits for a leader and, if this node leads and the
// default tenant does not exist yet, creates it through Raft.
func EnsureDefaultTenant(ctx context.Context, n *Node, state store.State) error {
	if err := n.WaitLeader(ctx); err != nil {
		return err
	}
	if _, err := state.GetTenant(ctx, DefaultTenantID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("read default tenant: %w", err)
	}
	if !n.IsLeader() {
		return nil // the leader creates it; the follower receives it via Raft
	}
	cmd, err := NewPutTenant(bootstrapTenantRequestID, DefaultTenantID, TenantDocument{Name: "Default", Plan: "default"})
	if err != nil {
		return err
	}
	if _, err := n.Apply(ctx, cmd); err != nil {
		return fmt.Errorf("create default tenant: %w", err)
	}
	return nil
}

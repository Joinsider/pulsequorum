// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"errors"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/clusterpki"
)

// Timings of the revocation after a recovery (ADR 0019).
const (
	recoveryRevokeBackoff    = time.Second
	recoveryRevokeBackoffMax = 30 * time.Second
	recoveryRevokeTimeout    = 10 * time.Second
)

// revokeRecovered revokes the members that "admin recover" dropped from the
// configuration (ADR 0019, FA-PKI-04). Their certificates stay valid until
// then, so a returning old node with stale state would still pass mTLS. It
// waits for a leader and the cluster CA, retries until every node is revoked,
// and removes the list afterwards. Without a list it returns at once.
func (h *Host) revokeRecovered(ctx context.Context) {
	list, ok, err := cluster.ReadRecoveryRevoke(h.dataDir)
	if err != nil {
		h.log.Error("reading the recovery revocation list failed; revoke the old nodes through the admin API", "err", err)
		return
	}
	if !ok {
		return
	}
	backoff := recoveryRevokeBackoff
	for {
		err := h.revokeList(ctx, list)
		if err == nil {
			if err := cluster.RemoveRecoveryRevoke(h.dataDir); err != nil {
				h.log.Warn("removing the recovery revocation list failed", "err", err)
			}
			h.log.Info("old members revoked after recovery", "node_ids", list.NodeIDs)
			return
		}
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("revoking old members after recovery failed, retrying", "retry_in", backoff, "err", err)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, recoveryRevokeBackoffMax)
	}
}

// revokeList proposes RevokeNode for every listed node. The request ID is
// fixed per node, so a retry after an unknown outcome is deduplicated.
func (h *Host) revokeList(ctx context.Context, list cluster.RecoveryRevoke) error {
	if addr, _ := h.Node.Leader(); addr == "" {
		return errNoLeader
	}
	if _, _, err := h.Authority.Bundle(ctx); errors.Is(err, clusterpki.ErrNoCA) {
		return err
	}
	for _, id := range list.NodeIDs {
		if id == h.Node.ID() {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, recoveryRevokeTimeout)
		_, err := h.Node.Apply(callCtx, cluster.NewRevokeNode("recovery-revoke:"+id, id))
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

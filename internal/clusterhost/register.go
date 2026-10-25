// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package clusterhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// Timings of the self-registration (ADR 0017).
const (
	// registryCheck is how often a registered node compares its entry with its data.
	registryCheck      = time.Minute
	registerBackoff    = time.Second
	registerBackoffMax = 30 * time.Second
	registerTimeout    = 10 * time.Second
)

// errNoLeader means the registration has to wait for a leader.
var errNoLeader = errors.New("no leader known")

// selfNode is the registry entry this node proposes for itself (ADR 0017).
func selfNode(opts Options) store.Node {
	caps := slices.Clone(opts.Capabilities)
	slices.Sort(caps)
	return store.Node{
		ID:                opts.NodeID,
		Name:              opts.Name,
		Role:              store.NodeRoleVoter,
		Capabilities:      slices.Compact(caps),
		Version:           opts.Version,
		MaxCommandVersion: commandVersion(opts.MaxCommandVersion),
	}
}

// commandVersion returns the command version this node announces: the test
// override if set, otherwise cluster.CommandVersion (ADR 0018).
func commandVersion(override uint32) uint32 {
	if override != 0 {
		return override
	}
	return cluster.CommandVersion
}

// registerRequestID derives the request ID from the content of the entry, so
// that the same content is proposed once and a change is proposed again.
func registerRequestID(n store.Node) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s\x00%d",
		n.Name, n.Role, n.Version, n.Capabilities, n.MaxCommandVersion))
	return "register:" + n.ID + ":" + hex.EncodeToString(sum[:])[:16]
}

// sameRegistration reports whether the registry entry has the content of n.
// Health and timestamps are the leader's business and are not compared.
func sameRegistration(entry, n store.Node) bool {
	return entry.Name == n.Name && entry.Role == n.Role && entry.Version == n.Version &&
		entry.MaxCommandVersion == n.MaxCommandVersion && slices.Equal(entry.Capabilities, n.Capabilities)
}

// registerSelf registers this node in the node registry once a leader is
// known, and again whenever its data change (ADR 0017). It retries with
// backoff until a registration succeeded. A revoked node cannot register and
// stops trying. It returns when ctx ends.
func (h *Host) registerSelf(ctx context.Context) {
	backoff := registerBackoff
	for {
		err := h.register(ctx)
		var cmdErr *cluster.CommandError
		switch {
		case err == nil:
			backoff = registerBackoff
			if !sleep(ctx, registryCheck) {
				return
			}
			continue
		case errors.As(err, &cmdErr) && cmdErr.Code == cluster.CodeConflict:
			h.log.Error("this node cannot register in the cluster", "node_id", h.self.ID, "err", err)
			return
		case ctx.Err() != nil:
			return
		}
		h.log.Warn("registering this node failed, retrying", "retry_in", backoff, "err", err)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, registerBackoffMax)
	}
}

// register proposes the entry of this node unless the registry already has it.
func (h *Host) register(ctx context.Context) error {
	if addr, _ := h.Node.Leader(); addr == "" {
		return errNoLeader
	}
	nodes, err := h.state.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("read node registry: %w", err)
	}
	for _, n := range nodes {
		if n.ID == h.self.ID && sameRegistration(n, h.self) {
			return nil
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	if _, err := h.Node.Apply(callCtx, cluster.NewRegisterNode(registerRequestID(h.self), h.self)); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	h.log.Info("node registered", "node_id", h.self.ID, "version", h.self.Version, "capabilities", h.self.Capabilities)
	return nil
}

// sleep pauses for d and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

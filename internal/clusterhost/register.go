// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
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
		PublicURL:         publicOrigin(opts.PublicURL),
	}
}

// publicOrigin returns the normalized public origin of raw (ADR 0036), or
// empty if raw is empty or not a valid origin. Start warns about the latter.
func publicOrigin(raw string) string {
	if raw == "" {
		return ""
	}
	origin, err := cluster.NormalizePublicOrigin(raw)
	if err != nil {
		return ""
	}
	return origin
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
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		n.Name, n.Role, n.Version, n.Capabilities, n.MaxCommandVersion, n.PublicURL))
	return "register:" + n.ID + ":" + hex.EncodeToString(sum[:])[:16]
}

// entryAtLevel returns the entry to propose at the stored command level: the
// public URL is left out below cluster.PublicURLCommandLevel, where the FSM
// ignores it (ADR 0036).
func entryAtLevel(self store.Node, level uint32) store.Node {
	if cluster.EffectiveCommandLevel(level) < cluster.PublicURLCommandLevel {
		self.PublicURL = ""
	}
	return self
}

// sameRegistration reports whether the registry entry has the content of n.
// Health and timestamps are the leader's business and are not compared.
func sameRegistration(entry, n store.Node) bool {
	return entry.Name == n.Name && entry.Role == n.Role && entry.Version == n.Version &&
		entry.MaxCommandVersion == n.MaxCommandVersion && slices.Equal(entry.Capabilities, n.Capabilities) &&
		entry.PublicURL == n.PublicURL
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
// Below cluster.PublicURLCommandLevel the public URL is not proposed, because
// the FSM ignores it there; once the cluster reaches that level, the entry
// differs from the registry and this node registers its URL.
func (h *Host) register(ctx context.Context) error {
	if addr, _ := h.Node.Leader(); addr == "" {
		return errNoLeader
	}
	level, err := h.Node.CommandLevel(ctx)
	if err != nil {
		return fmt.Errorf("read command level: %w", err)
	}
	self := entryAtLevel(h.self, level)
	nodes, err := h.state.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("read node registry: %w", err)
	}
	for _, n := range nodes {
		if n.ID == self.ID && sameRegistration(n, self) {
			return nil
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	if _, err := h.Node.Apply(callCtx, cluster.NewRegisterNode(registerRequestID(self), self)); err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	h.log.Info("node registered", "node_id", self.ID, "version", self.Version, "capabilities", self.Capabilities)
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

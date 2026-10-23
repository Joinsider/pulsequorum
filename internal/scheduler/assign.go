// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"

	"github.com/joinsider/uptimecluster/internal/store"
)

// Assigner decides which nodes run a monitor (ADR 0017, FA-SCH-01/02).
// It is pure: the result depends only on the monitor, the registry and the
// capability mapping, never on registry order.
type Assigner struct {
	// Capability returns the capability a monitor type requires, "" if none.
	Capability func(monitorType string) string
}

// Nodes returns the IDs of the nodes that run m, sorted by ID, given the
// registry. With an empty registry no node IDs are known, so it returns nil;
// Runs treats that case as "every node runs every monitor".
func (a Assigner) Nodes(m store.Monitor, registry []store.Node) []string {
	if len(registry) == 0 {
		return nil
	}
	candidates := a.candidates(m, registry)

	var ids []string
	switch m.Assignment.Mode {
	case "count":
		ids = rendezvous(m.ID, healthy(candidates), m.Assignment.Count)
	case "nodes":
		ids = listed(m.Assignment.Nodes, candidates)
	default: // "all", "" and unknown modes
		for _, n := range candidates {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Runs reports whether nodeID runs m. With an empty registry every node runs
// every monitor (single voter before registration).
func (a Assigner) Runs(m store.Monitor, registry []store.Node, nodeID string) bool {
	if len(registry) == 0 {
		return true
	}
	return slices.Contains(a.Nodes(m, registry), nodeID)
}

// candidates returns the voters of the registry that have the capability the
// monitor type requires. Probes join in P5 (ADR 0017).
func (a Assigner) candidates(m store.Monitor, registry []store.Node) []store.Node {
	capability := ""
	if a.Capability != nil {
		capability = a.Capability(m.Type)
	}
	var out []store.Node
	for _, n := range registry {
		if n.Role == store.NodeRoleVoter && n.HasCapability(capability) {
			out = append(out, n)
		}
	}
	return out
}

// healthy returns the nodes that are healthy.
func healthy(nodes []store.Node) []store.Node {
	var out []store.Node
	for _, n := range nodes {
		if n.Healthy {
			out = append(out, n)
		}
	}
	return out
}

// rendezvous picks the count nodes with the highest rendezvous weight for
// monitorID (highest random weight hashing). Ties are broken by the smaller
// node ID. A count below 1 is treated as 1.
func rendezvous(monitorID string, nodes []store.Node, count int) []string {
	if count < 1 {
		count = 1
	}
	slices.SortFunc(nodes, func(x, y store.Node) int {
		wx, wy := rendezvousWeight(monitorID, x.ID), rendezvousWeight(monitorID, y.ID)
		switch {
		case wx != wy:
			if wx > wy {
				return -1
			}
			return 1
		case x.ID < y.ID:
			return -1
		case x.ID > y.ID:
			return 1
		default:
			return 0
		}
	})
	if count > len(nodes) {
		count = len(nodes)
	}
	ids := make([]string, 0, count)
	for _, n := range nodes[:count] {
		ids = append(ids, n.ID)
	}
	return ids
}

// listed returns the IDs from wanted that are candidates, each once.
func listed(wanted []string, candidates []store.Node) []string {
	known := make(map[string]bool, len(candidates))
	for _, n := range candidates {
		known[n.ID] = true
	}
	seen := make(map[string]bool, len(wanted))
	var ids []string
	for _, id := range wanted {
		if known[id] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// rendezvousWeight is the first 8 bytes of SHA-256(monitorID NUL nodeID) as a
// big-endian integer. Changing this function moves monitors between nodes, so
// its output is pinned by a golden test.
func rendezvousWeight(monitorID, nodeID string) uint64 {
	sum := sha256.Sum256([]byte(monitorID + "\x00" + nodeID))
	return binary.BigEndian.Uint64(sum[:8])
}

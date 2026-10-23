// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package scheduler

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// assignCapability maps "icmp" monitors to the icmp capability, all others to none.
func assignCapability(monitorType string) string {
	if monitorType == "icmp" {
		return "icmp"
	}
	return ""
}

func voter(id string, healthy bool, caps ...string) store.Node {
	return store.Node{ID: id, Role: store.NodeRoleVoter, Healthy: healthy, Capabilities: caps}
}

func probe(id string, healthy bool) store.Node {
	return store.Node{ID: id, Role: store.NodeRoleProbe, Healthy: healthy}
}

func monitorWith(id, monitorType string, a store.Assignment) store.Monitor {
	return store.Monitor{ID: id, Type: monitorType, Assignment: a}
}

func TestAssignerNodes(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	ids := func(ns ...string) []string { return ns }

	tests := []struct {
		name     string
		monitor  store.Monitor
		registry []store.Node
		want     []string
	}{
		{
			name:     "all runs on every voter, healthy or not",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "all"}),
			registry: []store.Node{voter("v2", false), voter("v1", true)},
			want:     ids("v1", "v2"),
		},
		{
			name:     "empty mode is all",
			monitor:  monitorWith("m", "http", store.Assignment{}),
			registry: []store.Node{voter("v1", true), voter("v2", false)},
			want:     ids("v1", "v2"),
		},
		{
			name:     "unknown mode is all",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "bogus"}),
			registry: []store.Node{voter("v1", false), voter("v2", true)},
			want:     ids("v1", "v2"),
		},
		{
			name:     "all skips probes until P5",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "all"}),
			registry: []store.Node{voter("v1", true), probe("p1", true)},
			want:     ids("v1"),
		},
		{
			name:     "all filters by capability",
			monitor:  monitorWith("m", "icmp", store.Assignment{Mode: "all"}),
			registry: []store.Node{voter("v1", true, "icmp"), voter("v2", true)},
			want:     ids("v1"),
		},
		{
			name:     "all without capability requirement ignores capabilities",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "all"}),
			registry: []store.Node{voter("v1", true, "icmp"), voter("v2", true)},
			want:     ids("v1", "v2"),
		},
		{
			name:     "count 1 picks one of the healthy nodes",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "count", Count: 1}),
			registry: []store.Node{voter("v1", true), voter("v2", true), voter("v3", true)},
			want:     ids("v3"), // highest SHA-256 weight for monitor "m", see TestRendezvousWeightGolden
		},
		{
			name:     "count excludes unhealthy nodes",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "count", Count: 3}),
			registry: []store.Node{voter("v1", false), voter("v2", true), voter("v3", true)},
			want:     ids("v2", "v3"),
		},
		{
			name:     "count larger than healthy candidates returns all healthy",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "count", Count: 5}),
			registry: []store.Node{voter("v1", true), voter("v2", false), voter("v3", true)},
			want:     ids("v1", "v3"),
		},
		{
			name:     "count with no healthy candidate runs nowhere",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "count", Count: 2}),
			registry: []store.Node{voter("v1", false), voter("v2", false)},
			want:     nil,
		},
		{
			name:     "count filters by capability before health",
			monitor:  monitorWith("m", "icmp", store.Assignment{Mode: "count", Count: 2}),
			registry: []store.Node{voter("v1", true, "icmp"), voter("v2", true), voter("v3", false, "icmp")},
			want:     ids("v1"),
		},
		{
			name:     "nodes lists only known voters, each once",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v3", "v1", "v1", "ghost", "p1"}}),
			registry: []store.Node{voter("v1", true), voter("v2", true), voter("v3", false), probe("p1", true)},
			want:     ids("v1", "v3"),
		},
		{
			name:     "nodes ignores health",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v2"}}),
			registry: []store.Node{voter("v1", true), voter("v2", false)},
			want:     ids("v2"),
		},
		{
			name:     "nodes filters by capability",
			monitor:  monitorWith("m", "icmp", store.Assignment{Mode: "nodes", Nodes: []string{"v1", "v2"}}),
			registry: []store.Node{voter("v1", true, "icmp"), voter("v2", true)},
			want:     ids("v1"),
		},
		{
			name:     "nodes with empty list runs nowhere",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "nodes"}),
			registry: []store.Node{voter("v1", true)},
			want:     nil,
		},
		{
			name:     "empty registry has no node IDs",
			monitor:  monitorWith("m", "http", store.Assignment{Mode: "all"}),
			registry: nil,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := a.Nodes(tt.monitor, tt.registry)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Nodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAssignerCountBelowOneRunsOnOneNode(t *testing.T) {
	a := Assigner{}
	registry := []store.Node{voter("v1", true), voter("v2", true), voter("v3", true)}
	for i := range 100 {
		id := fmt.Sprintf("monitor-%03d", i)
		one := a.Nodes(monitorWith(id, "http", store.Assignment{Mode: "count", Count: 1}), registry)
		for _, c := range []int{0, -3} {
			got := a.Nodes(monitorWith(id, "http", store.Assignment{Mode: "count", Count: c}), registry)
			if !slices.Equal(got, one) {
				t.Errorf("monitor %s: Count %d gives %v, want %v (count 1)", id, c, got, one)
			}
		}
		if len(one) != 1 {
			t.Errorf("monitor %s: count 1 gives %v, want exactly one node", id, one)
		}
	}
}

func TestAssignerNilCapability(t *testing.T) {
	a := Assigner{}
	m := monitorWith("m", "icmp", store.Assignment{Mode: "all"})
	registry := []store.Node{voter("v1", true), voter("v2", true)}
	if got, want := a.Nodes(m, registry), []string{"v1", "v2"}; !slices.Equal(got, want) {
		t.Errorf("Nodes() = %v, want %v", got, want)
	}
}

func TestAssignerRuns(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	registry := []store.Node{voter("v1", true, "icmp"), voter("v2", false), voter("v3", true)}

	tests := []struct {
		name    string
		monitor store.Monitor
		node    string
		want    bool
	}{
		{"all runs on listed voter", monitorWith("m", "http", store.Assignment{Mode: "all"}), "v2", true},
		{"nodes runs on listed node", monitorWith("m", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v3"}}), "v3", true},
		{"nodes does not run elsewhere", monitorWith("m", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v3"}}), "v1", false},
		{"capability missing blocks run", monitorWith("m", "icmp", store.Assignment{Mode: "all"}), "v2", false},
		{"unknown node does not run", monitorWith("m", "http", store.Assignment{Mode: "all"}), "ghost", false},
		{"unhealthy node does not run count", monitorWith("m", "http", store.Assignment{Mode: "count", Count: 3}), "v2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.Runs(tt.monitor, registry, tt.node); got != tt.want {
				t.Errorf("Runs(%s) = %v, want %v", tt.node, got, tt.want)
			}
		})
	}
}

func TestAssignerRunsEmptyRegistry(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	for _, m := range []store.Monitor{
		monitorWith("m", "http", store.Assignment{Mode: "all"}),
		monitorWith("m", "icmp", store.Assignment{Mode: "count", Count: 1}),
		monitorWith("m", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v1"}}),
	} {
		if !a.Runs(m, nil, "v1") || !a.Runs(m, nil, "anything") {
			t.Errorf("Runs with empty registry for mode %q = false, want true", m.Assignment.Mode)
		}
	}
}

func TestAssignerRunsAgreesWithNodes(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	registry := []store.Node{voter("v1", true, "icmp"), voter("v2", false), voter("v3", true), probe("p1", true)}
	monitors := []store.Monitor{
		monitorWith("m1", "http", store.Assignment{Mode: "all"}),
		monitorWith("m2", "icmp", store.Assignment{Mode: "count", Count: 1}),
		monitorWith("m3", "http", store.Assignment{Mode: "count", Count: 2}),
		monitorWith("m4", "http", store.Assignment{Mode: "nodes", Nodes: []string{"v2", "p1", "v3"}}),
	}
	for _, m := range monitors {
		nodes := a.Nodes(m, registry)
		for _, n := range append([]store.Node{probe("unknown", true)}, registry...) {
			if got, want := a.Runs(m, registry, n.ID), slices.Contains(nodes, n.ID); got != want {
				t.Errorf("monitor %s: Runs(%s) = %v, Nodes() = %v", m.ID, n.ID, got, nodes)
			}
		}
	}
}

func TestAssignerDeterministicAcrossRegistryOrder(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	base := []store.Node{
		voter("v1", true, "icmp"), voter("v2", true), voter("v3", false),
		voter("v4", true, "icmp"), voter("v5", true), probe("p1", true),
	}
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: reproducible shuffle in a test

	for i := range 200 {
		id := fmt.Sprintf("mon-%03d", i)
		for _, m := range []store.Monitor{
			monitorWith(id, "http", store.Assignment{Mode: "count", Count: 2}),
			monitorWith(id, "icmp", store.Assignment{Mode: "count", Count: 2}),
			monitorWith(id, "http", store.Assignment{Mode: "all"}),
			monitorWith(id, "http", store.Assignment{Mode: "nodes", Nodes: []string{"v5", "v1", "v3"}}),
		} {
			want := a.Nodes(m, base)
			for range 5 {
				shuffled := slices.Clone(base)
				rng.Shuffle(len(shuffled), func(x, y int) { shuffled[x], shuffled[y] = shuffled[y], shuffled[x] })
				if got := a.Nodes(m, shuffled); !slices.Equal(got, want) {
					t.Fatalf("monitor %s (%s, %s): order-dependent result %v, want %v", id, m.Type, m.Assignment.Mode, got, want)
				}
			}
		}
	}
}

func TestAssignerStabilityAndDistribution(t *testing.T) {
	a := Assigner{Capability: assignCapability}
	nodeIDs := []string{"n1", "n2", "n3", "n4", "n5"}
	healthyRegistry := make([]store.Node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		healthyRegistry = append(healthyRegistry, voter(id, true))
	}
	degraded := slices.Clone(healthyRegistry)
	degraded[2].Healthy = false // n3 becomes unhealthy

	const monitors = 1000
	counts := make(map[string]int, len(nodeIDs))
	moved, stayed := 0, 0
	for i := range monitors {
		m := monitorWith(fmt.Sprintf("monitor-%04d", i), "http", store.Assignment{Mode: "count", Count: 1})
		before := a.Nodes(m, healthyRegistry)
		after := a.Nodes(m, degraded)
		if len(before) != 1 || len(after) != 1 {
			t.Fatalf("monitor %s: want exactly one node, got before=%v after=%v", m.ID, before, after)
		}
		counts[before[0]]++

		if before[0] == "n3" {
			moved++
			if after[0] == "n3" {
				t.Errorf("monitor %s still runs on unhealthy node n3", m.ID)
			}
		} else {
			stayed++
			if after[0] != before[0] {
				t.Errorf("monitor %s moved from %s to %s although its node stayed healthy", m.ID, before[0], after[0])
			}
		}
	}

	for _, id := range nodeIDs {
		if c := counts[id]; c < 120 || c > 280 {
			t.Errorf("node %s runs %d of %d monitors, want between 120 and 280", id, c, monitors)
		}
	}
	if moved == 0 || stayed == 0 {
		t.Errorf("degenerate split: moved=%d stayed=%d", moved, stayed)
	}
}

func TestRendezvousWeightGolden(t *testing.T) {
	// Pinned: changing the hash moves monitors between nodes on upgrade.
	const want uint64 = 0x707077214230132c
	if got := rendezvousWeight("monitor-1", "node-a"); got != want {
		t.Errorf("rendezvousWeight() = %#x, want %#x", got, want)
	}
}

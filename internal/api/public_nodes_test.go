// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/publicnodes"
	"github.com/joinsider/pulsequorum/internal/store"
)

// fixedLister returns a fixed node registry.
type fixedLister struct{ nodes []store.Node }

func (f fixedLister) ListNodes(context.Context) ([]store.Node, error) { return f.nodes, nil }

// TestListPublicNodesIsPublic checks that the operation needs no login (ADR 0036).
func TestListPublicNodesIsPublic(t *testing.T) {
	t.Parallel()
	e := newEnvTweaked(t, false, func(d *Deps) { d.PublicNodes = nil })
	res := e.do("GET", "/api/v1/cluster/nodes", "", nil)
	if res.code != http.StatusOK {
		t.Fatalf("unauthenticated status = %d, want 200", res.code)
	}
}

// TestListPublicNodesEmpty checks that a missing source lists no node, as an
// empty array and not null.
func TestListPublicNodesEmpty(t *testing.T) {
	t.Parallel()
	e := newEnvTweaked(t, false, func(d *Deps) { d.PublicNodes = nil })
	res := e.do("GET", "/api/v1/cluster/nodes", "", nil)
	if got := string(res.body); got != "{\"nodes\":[]}\n" {
		t.Errorf("body = %q, want empty node list", got)
	}
}

// TestListPublicNodesFields checks the response: only id, url, voter and
// healthy, self first, the others sorted by ID.
func TestListPublicNodesFields(t *testing.T) {
	t.Parallel()
	list := fixedLister{nodes: []store.Node{
		{ID: "n3", Role: store.NodeRoleVoter, PublicURL: "https://n3.example.com", Healthy: false, Name: "secret-name"},
		{ID: "n2", Role: store.NodeRoleVoter, PublicURL: "https://n2.example.com", Healthy: true},
		{ID: "p1", Role: store.NodeRoleProbe, PublicURL: ""},
	}}
	src := publicnodes.New("n1", "https://n1.example.com", list, nil, nil)
	e := newEnvTweaked(t, false, func(d *Deps) { d.PublicNodes = src })

	res := e.do("GET", "/api/v1/cluster/nodes", "", nil)
	if res.code != http.StatusOK {
		t.Fatalf("status = %d", res.code)
	}

	var raw struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(res.body, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Nodes) != 3 {
		t.Fatalf("got %d nodes, want 3: %s", len(raw.Nodes), res.body)
	}
	for _, n := range raw.Nodes {
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"healthy", "id", "url", "voter"}; !slices.Equal(keys, want) {
			t.Errorf("node keys = %v, want %v", keys, want)
		}
	}

	nodes := decode[gen.PublicNodeList](t, res).Nodes
	want := []gen.PublicNode{
		{Id: "n1", Url: "https://n1.example.com", Voter: true, Healthy: true},
		{Id: "n2", Url: "https://n2.example.com", Voter: true, Healthy: true},
		{Id: "n3", Url: "https://n3.example.com", Voter: true, Healthy: false},
	}
	if !slices.Equal(nodes, want) {
		t.Errorf("nodes = %+v, want %+v", nodes, want)
	}
}

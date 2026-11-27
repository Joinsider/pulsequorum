// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/joinsider/pulsequorum/internal/api"
	"github.com/joinsider/pulsequorum/internal/publicnodes"
	"github.com/joinsider/pulsequorum/internal/store"
)

// nodeList is a fixed node nodeList for the public node source.
type nodeList []store.Node

func (r nodeList) ListNodes(context.Context) ([]store.Node, error) { return r, nil }

func TestFrontendConfigNodesFromPublicNodes(t *testing.T) {
	t.Parallel()
	list := nodeList{
		{ID: "n2", Role: store.NodeRoleVoter, PublicURL: "https://n2.example.com", Healthy: true},
		{ID: "n1", Role: store.NodeRoleVoter, PublicURL: "https://mon1.example.com", Healthy: true},
	}
	src := publicnodes.New("n1", "https://mon1.example.com", list, nil, nil)
	s := testServer().WithAPI(api.Deps{PublicNodes: src})

	rec := get(t, s.Handler(), "/config.json")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var fc FrontendConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://mon1.example.com", "https://n2.example.com"}
	if len(fc.Nodes) != len(want) {
		t.Fatalf("nodes = %v, want %v", fc.Nodes, want)
	}
	for i := range want {
		if fc.Nodes[i] != want[i] {
			t.Errorf("nodes[%d] = %q, want %q (self first)", i, fc.Nodes[i], want[i])
		}
	}
}

func TestFrontendConfigNodesWithoutPublicNodesKeepsPublicURL(t *testing.T) {
	t.Parallel()
	s := testServer().WithAPI(api.Deps{})
	rec := get(t, s.Handler(), "/config.json")
	var fc FrontendConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	if len(fc.Nodes) != 1 || fc.Nodes[0] != "https://mon1.example.com" {
		t.Errorf("nodes = %v, want the PUBLIC_URL only", fc.Nodes)
	}
}

func TestFrontendConfigNodesEmptyIsArray(t *testing.T) {
	t.Parallel()
	s := testServer().WithAPI(api.Deps{PublicNodes: publicnodes.New("", "", nil, nil, nil)})
	s.cfg.PublicURL = ""
	rec := get(t, s.Handler(), "/config.json")
	if body := rec.Body.String(); body != "{\"nodes\":[],\"auth\":{\"mode\":\"local\"}}\n" {
		t.Errorf("body = %q, want an empty node array", body)
	}
}

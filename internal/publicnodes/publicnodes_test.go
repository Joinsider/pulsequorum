// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package publicnodes

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

type fakeLister struct {
	nodes []store.Node
	err   error
	calls int
}

func (f *fakeLister) ListNodes(context.Context) ([]store.Node, error) {
	f.calls++
	return f.nodes, f.err
}

func voter(id, url string, healthy bool) store.Node {
	return store.Node{ID: id, Role: store.NodeRoleVoter, PublicURL: url, Healthy: healthy}
}

func TestNodesOrderAndFilter(t *testing.T) {
	l := &fakeLister{nodes: []store.Node{
		voter("n3", "https://n3.example.com", true),
		voter("n1", "https://old.example.com", false), // self: config wins, registry health kept
		voter("n2", "https://n2.example.com", false),
		voter("n4", "", true),
		voter("n5", "https://N5.example.com", true),     // not normalized: not trusted
		voter("n6", "https://n6.example.com/app", true), // invalid
		voter("n7", "https://n2.example.com", true),     // duplicate origin
		{ID: "p1", Role: store.NodeRoleProbe, PublicURL: "https://p1.example.com", Healthy: true},
	}}
	s := New("n1", "https://N1.example.com:443/", l, nil, nil)
	got := s.Nodes(context.Background())
	want := []Node{
		{ID: "n1", URL: "https://n1.example.com", Healthy: false},
		{ID: "n2", URL: "https://n2.example.com", Healthy: false},
		{ID: "n3", URL: "https://n3.example.com", Healthy: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Nodes = %+v, want %+v", got, want)
	}
	if o := s.Origins(context.Background()); !slices.Equal(o, []string{"https://n1.example.com", "https://n2.example.com", "https://n3.example.com"}) {
		t.Fatalf("Origins = %v", o)
	}
}

func TestSelfWithoutValidURL(t *testing.T) {
	l := &fakeLister{nodes: []store.Node{voter("n1", "https://n1.example.com", true), voter("n2", "https://n2.example.com", true)}}
	s := New("n1", "https://n1.example.com/sub", l, nil, nil)
	got := s.Origins(context.Background())
	if !slices.Equal(got, []string{"https://n2.example.com"}) {
		t.Fatalf("Origins = %v", got)
	}
}

func TestCacheAndErrors(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &fakeLister{nodes: []store.Node{voter("n2", "https://n2.example.com", true)}}
	s := New("n1", "https://n1.example.com", l, func() time.Time { return now }, nil)
	ctx := context.Background()
	s.Nodes(ctx)
	s.Nodes(ctx)
	if l.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", l.calls)
	}
	now = now.Add(cacheTTL)
	l.err = errors.New("boom")
	if got := s.Origins(ctx); len(got) != 2 {
		t.Fatalf("on error want last good list, got %v", got)
	}
	if l.calls != 2 {
		t.Fatalf("calls = %d, want 2", l.calls)
	}

	fresh := New("n1", "https://n1.example.com", &fakeLister{err: errors.New("boom")}, nil, nil)
	if got := fresh.Origins(ctx); !slices.Equal(got, []string{"https://n1.example.com"}) {
		t.Fatalf("first read failing: want self only, got %v", got)
	}
}

func TestNilAndNoLister(t *testing.T) {
	var s *Source
	if s.Nodes(context.Background()) != nil || s.Contains(context.Background(), "https://a.example") {
		t.Fatal("nil source must be empty")
	}
	only := New("n1", "http://localhost:8080", nil, nil, nil)
	if got := only.Origins(context.Background()); !slices.Equal(got, []string{"http://localhost:8080"}) {
		t.Fatalf("Origins = %v", got)
	}
}

func TestContains(t *testing.T) {
	l := &fakeLister{nodes: []store.Node{voter("n2", "https://n2.example.com:8443", true)}}
	s := New("n1", "https://n1.example.com", l, nil, nil)
	ctx := context.Background()
	for origin, want := range map[string]bool{
		"https://n1.example.com":       true,
		"https://N1.example.com":       true,
		"https://n2.example.com:8443":  true,
		"https://n2.example.com":       false,
		"https://n1.example.com:443":   false, // browsers never send the default port
		"https://n1.example.com/":      false,
		"http://n1.example.com":        false,
		"null":                         false,
		"":                             false,
		"https://evil.example":         false,
		"https://n1.example.com.evil.": false,
	} {
		if got := s.Contains(ctx, origin); got != want {
			t.Errorf("Contains(%q) = %v, want %v", origin, got, want)
		}
	}
}

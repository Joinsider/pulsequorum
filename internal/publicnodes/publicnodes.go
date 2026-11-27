// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package publicnodes provides the public origins of the voters for client
// failover (ADR 0036). The list steers where the embedded SPA sends bearer
// tokens and feeds the CORS allow-list, the CSP connect-src and config.json,
// so all of them use this one source. It is security code.
package publicnodes

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// cacheTTL bounds how long a registry read is reused. CORS runs on every
// API request; a few seconds of delay for a new voter are acceptable.
const cacheTTL = 5 * time.Second

// Node is a voter with a public origin.
type Node struct {
	ID      string
	URL     string
	Healthy bool
}

// Lister reads the node registry; store.State satisfies it.
type Lister interface {
	ListNodes(ctx context.Context) ([]store.Node, error)
}

// Source returns the voters with a public origin. The zero value and a nil
// *Source return no nodes.
type Source struct {
	selfID  string
	selfURL string
	list    Lister
	now     func() time.Time
	log     *slog.Logger

	mu       sync.Mutex
	cached   []Node
	cachedAt time.Time
	valid    bool
}

// New creates a source. selfURL is this voter's PUBLIC_URL; it is listed
// first, also before the registry has it, and left out if it is not a plain
// origin. list may be nil (no registry, e.g. in tests): then only this voter
// is listed. now and log may be nil.
func New(selfID, selfURL string, list Lister, now func() time.Time, log *slog.Logger) *Source {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	self := ""
	if selfURL != "" {
		if o, err := cluster.NormalizePublicOrigin(selfURL); err == nil {
			self = o
		}
	}
	return &Source{selfID: selfID, selfURL: self, list: list, now: now, log: log}
}

// Nodes returns this voter first (if it has a public origin), then the other
// voters of the registry with a public origin, sorted by ID. Every registry
// value is checked again with cluster.NormalizePublicOrigin, and an origin is
// listed once. On a registry error the last good list is returned, or only
// this voter. The returned slice must not be modified.
func (s *Source) Nodes(ctx context.Context) []Node {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.valid && now.Sub(s.cachedAt) < cacheTTL && !now.Before(s.cachedAt) {
		return s.cached
	}
	nodes, err := s.load(ctx)
	if err != nil {
		s.log.Warn("read node registry for public node URLs", "err", err)
		if s.valid {
			return s.cached
		}
		return s.selfOnly()
	}
	s.cached, s.cachedAt, s.valid = nodes, now, true
	return nodes
}

// Origins returns the origins of Nodes in the same order.
func (s *Source) Origins(ctx context.Context) []string {
	nodes := s.Nodes(ctx)
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.URL
	}
	return out
}

// ConfigOrigins returns the origins the embedded SPA of this voter may fail
// over to: Origins, or nothing if this voter has no valid public origin
// itself (ADR 0036). Without PUBLIC_URL the SPA stays on the page's origin,
// because the operator gave no public address to come back to. config.json
// and the CSP connect-src use this list; CORS uses Origins.
func (s *Source) ConfigOrigins(ctx context.Context) []string {
	if s == nil || s.selfURL == "" {
		return nil
	}
	return s.Origins(ctx)
}

// Contains reports whether origin (as sent by a browser) is one of Origins.
// The comparison is exact after lower-casing; "null" never matches.
func (s *Source) Contains(ctx context.Context, origin string) bool {
	if origin == "" || origin == "null" {
		return false
	}
	o, err := cluster.NormalizePublicOrigin(origin)
	if err != nil || o != strings.ToLower(origin) {
		return false
	}
	return slices.Contains(s.Origins(ctx), o)
}

func (s *Source) selfOnly() []Node {
	if s.selfURL == "" {
		return nil
	}
	return []Node{{ID: s.selfID, URL: s.selfURL, Healthy: true}}
}

func (s *Source) load(ctx context.Context) ([]Node, error) {
	if s.list == nil {
		return s.selfOnly(), nil
	}
	entries, err := s.list.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var self *Node
	others := make([]Node, 0, len(entries))
	for _, e := range entries {
		if e.Role != store.NodeRoleVoter || e.PublicURL == "" {
			continue
		}
		o, err := cluster.NormalizePublicOrigin(e.PublicURL)
		if err != nil || o != e.PublicURL {
			// The FSM stores only normalized origins; anything else is not
			// trusted.
			continue
		}
		n := Node{ID: e.ID, URL: o, Healthy: e.Healthy}
		if e.ID == s.selfID {
			self = &n
			continue
		}
		others = append(others, n)
	}
	slices.SortFunc(others, func(a, b Node) int { return cmp.Compare(a.ID, b.ID) })

	out := make([]Node, 0, len(others)+1)
	if s.selfURL != "" {
		// The local configuration wins over the registry entry of this voter,
		// which may lag behind a changed PUBLIC_URL.
		healthy := true
		if self != nil {
			healthy = self.Healthy
		}
		out = append(out, Node{ID: s.selfID, URL: s.selfURL, Healthy: healthy})
	}
	for _, n := range others {
		if !slices.ContainsFunc(out, func(x Node) bool { return x.URL == n.URL }) {
			out = append(out, n)
		}
	}
	return out, nil
}

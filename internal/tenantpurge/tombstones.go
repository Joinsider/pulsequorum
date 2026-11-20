// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package tenantpurge deletes the metrics of deleted tenants on every node and
// keeps heartbeats of deleted tenants out of metrics.db (ADR 0029).
package tenantpurge

import (
	"context"
	"log/slog"
	"sync"

	"github.com/joinsider/pulsequorum/internal/store"
)

// Tombstones is the node-local cache of the deleted tenant IDs. It is replaced
// by the purge job; the ingest filter reads it under a shared lock, so a
// replacement waits for the inserts in progress and every later insert sees it.
type Tombstones struct {
	mu  sync.RWMutex
	ids map[string]bool
}

// NewTombstones returns an empty cache.
func NewTombstones() *Tombstones {
	return &Tombstones{ids: map[string]bool{}}
}

// Replace sets the deleted tenant IDs.
func (t *Tombstones) Replace(ids []string) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ids = set
}

// Has reports whether the tenant was deleted.
func (t *Tombstones) Has(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ids[id]
}

// IngestFilter is a store.Metrics whose InsertHeartbeats drops the heartbeats
// of deleted tenants (ADR 0029). Everything else is passed through. Local
// checks, replication pushes and anti-entropy all insert through it.
//
// Dropped heartbeats leave holes in their stream. Replication repairs them
// with explicit position advances, so the holes do not stall it.
type IngestFilter struct {
	store.Metrics
	tombs *Tombstones
	log   *slog.Logger
}

// NewIngestFilter wraps m. log may be nil.
func NewIngestFilter(m store.Metrics, tombs *Tombstones, log *slog.Logger) *IngestFilter {
	if log == nil {
		log = slog.Default()
	}
	return &IngestFilter{Metrics: m, tombs: tombs, log: log}
}

// InsertHeartbeats stores the heartbeats of tenants that still exist.
func (f *IngestFilter) InsertHeartbeats(ctx context.Context, hbs []store.Heartbeat) (int, error) {
	// The shared lock keeps the purge job from finishing a tenant while an
	// insert of that tenant is in flight.
	f.tombs.mu.RLock()
	defer f.tombs.mu.RUnlock()
	kept := hbs
	if len(f.tombs.ids) > 0 {
		kept = make([]store.Heartbeat, 0, len(hbs))
		dropped := 0
		for _, hb := range hbs {
			if f.tombs.ids[hb.TenantID] {
				dropped++
				continue
			}
			kept = append(kept, hb)
		}
		if dropped > 0 {
			f.log.Debug("dropped heartbeats of deleted tenants", "count", dropped)
		}
	}
	if len(kept) == 0 {
		return 0, nil
	}
	return f.Metrics.InsertHeartbeats(ctx, kept)
}

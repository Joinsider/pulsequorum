// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"

	"github.com/joinsider/uptimecluster/internal/store"
)

// StateEvent describes a committed change of a monitor's consensus status.
// It is delivered after the transaction committed; it is a notification for
// local listeners (live stream) and never influences the replicated state.
type StateEvent struct {
	TenantID  string
	MonitorID string
	Status    store.MonitorStatus
	Confirmed bool
	SinceMs   int64
	// IncidentOpened is the ID of the incident opened with this change (status DOWN), or "".
	IncidentOpened string
	// IncidentResolved is true if an open incident of the monitor was resolved with this change.
	IncidentResolved bool
}

// OnStateChange registers fn to receive StateEvents. A later call replaces the
// previous observer; nil removes it.
//
// Events are only delivered for entries applied after registration. Entries
// applied before fn is registered (for example during Start, while Raft
// replays its log) are not delivered, so a listener must read the current
// state from the store when it connects. fn is called synchronously from Apply
// after the commit and must not block.
func (f *FSM) OnStateChange(fn func(StateEvent)) {
	if fn == nil {
		f.observer.Store(nil)
		return
	}
	f.observer.Store(&fn)
}

// emit delivers committed events to the observer, if one is registered.
func (f *FSM) emit(events []StateEvent) {
	fn := f.observer.Load()
	if fn == nil {
		return
	}
	for _, ev := range events {
		(*fn)(ev)
	}
}

// stateEventCollector gathers the events of one apply transaction. It is only
// read after the transaction committed.
type stateEventCollector struct {
	events []StateEvent
}

type stateEventsKey struct{}

// withStateEvents returns a context that records events into c.
func withStateEvents(ctx context.Context, c *stateEventCollector) context.Context {
	return context.WithValue(ctx, stateEventsKey{}, c)
}

// recordStateEvent appends ev to the collector of ctx; without a collector it
// does nothing.
func recordStateEvent(ctx context.Context, ev StateEvent) {
	if c, ok := ctx.Value(stateEventsKey{}).(*stateEventCollector); ok {
		c.events = append(c.events, ev)
	}
}

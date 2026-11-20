// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package stream fans out live events (heartbeats, status changes, incidents)
// to the WebSocket clients of this node (FA-API-04). Every event belongs to
// exactly one tenant, and a subscription only ever receives events of the
// tenant it was opened for (FA-TEN-05).
package stream

import (
	"sync"
)

// DefaultBuffer is the number of events a subscriber may lag behind before it
// is dropped.
const DefaultBuffer = 256

// Event is one message for the clients of a tenant. Data is the encoded JSON
// message; it is shared between subscribers and must not be modified.
type Event struct {
	TenantID  string
	MonitorID string
	Data      []byte
}

// Hub distributes events to subscriptions. Publish never blocks: a subscriber
// that cannot keep up is closed, and its client reconnects and reloads.
type Hub struct {
	buffer int

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{} // tenant ID → subscriptions
}

// NewHub returns a hub whose subscriptions buffer up to buffer events
// (DefaultBuffer if buffer <= 0).
func NewHub(buffer int) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Hub{buffer: buffer, subs: map[string]map[*Subscription]struct{}{}}
}

// Subscription receives the events of one tenant.
type Subscription struct {
	hub      *Hub
	tenantID string
	ch       chan []byte
	done     chan struct{}

	mu       sync.Mutex
	monitors map[string]bool // nil = all monitors of the tenant
	closed   bool
	overflow bool
}

// Subscribe opens a subscription for tenantID. The tenant must come from the
// authenticated principal, never from the client. An empty tenant ID yields a
// subscription that is already closed.
func (h *Hub) Subscribe(tenantID string) *Subscription {
	s := &Subscription{hub: h, tenantID: tenantID, ch: make(chan []byte, h.buffer), done: make(chan struct{})}
	if tenantID == "" {
		s.closeLocked()
		return s
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[tenantID]
	if set == nil {
		set = map[*Subscription]struct{}{}
		h.subs[tenantID] = set
	}
	set[s] = struct{}{}
	return s
}

// Publish delivers e to the subscriptions of e.TenantID. Events without a
// tenant are dropped.
func (h *Hub) Publish(e Event) {
	if e.TenantID == "" {
		return
	}
	var full []*Subscription
	h.mu.Lock()
	for s := range h.subs[e.TenantID] {
		if !s.deliver(e) {
			full = append(full, s)
		}
	}
	h.mu.Unlock()
	for _, s := range full {
		s.drop()
	}
}

// Subscribers returns the number of open subscriptions (for metrics and tests).
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

func (h *Hub) remove(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[s.tenantID]
	delete(set, s)
	if len(set) == 0 {
		delete(h.subs, s.tenantID)
	}
}

// deliver queues e if it passes the monitor filter; it returns false if the
// buffer is full. Called with the hub lock held.
func (s *Subscription) deliver(e Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || e.TenantID != s.tenantID {
		return true
	}
	if s.monitors != nil && !s.monitors[e.MonitorID] {
		return true
	}
	select {
	case s.ch <- e.Data:
		return true
	default:
		return false
	}
}

// drop closes a subscriber that fell behind.
func (s *Subscription) drop() {
	s.mu.Lock()
	s.overflow = true
	s.mu.Unlock()
	s.Close()
}

// Events returns the channel of encoded messages. It is closed together with
// Done, after which buffered messages may still be read.
func (s *Subscription) Events() <-chan []byte { return s.ch }

// Done is closed when the subscription ended (Close or overflow).
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Overflowed reports whether the subscription was closed because the client
// could not keep up.
func (s *Subscription) Overflowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflow
}

// SetMonitors limits the subscription to the given monitors of its tenant;
// an empty list means all monitors. IDs of other tenants never match because
// events are filtered by tenant first.
func (s *Subscription) SetMonitors(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(ids) == 0 {
		s.monitors = nil
		return
	}
	s.monitors = make(map[string]bool, len(ids))
	for _, id := range ids {
		s.monitors[id] = true
	}
}

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.remove(s)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
}

func (s *Subscription) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	close(s.done)
	close(s.ch)
}

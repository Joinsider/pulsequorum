// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// TicketTTL is the validity of a WebSocket ticket (FA-AUTH-11).
const TicketTTL = 30 * time.Second

// ErrInvalidTicket is returned for unknown, expired or already used tickets.
var ErrInvalidTicket = errors.New("invalid or expired stream ticket")

// Ticket is a redeemed WebSocket ticket.
type Ticket struct {
	Principal Principal
	// SessionEnd is when the credentials the ticket was issued for expire;
	// the stream is closed then (FA-AUTH-11).
	SessionEnd time.Time
}

// Tickets issues short-lived, single-use tickets that let a browser open the
// WebSocket stream without putting a long-lived token into the URL (PF-32).
// Tickets live in memory of the issuing node; only their SHA-256 hash is kept.
type Tickets struct {
	TTL   time.Duration
	Limit int // tickets kept at most; issuing fails beyond that
	Now   func() time.Time

	mu      sync.Mutex
	pending map[[sha256.Size]byte]ticketEntry
}

type ticketEntry struct {
	ticket  Ticket
	expires time.Time
}

// NewTickets returns a ticket store with TicketTTL.
func NewTickets() *Tickets {
	return &Tickets{TTL: TicketTTL, Limit: 10_000, Now: time.Now}
}

// ErrTooManyTickets is returned when the store is full of unexpired tickets.
var ErrTooManyTickets = errors.New("too many pending stream tickets")

// Issue creates a ticket for p, valid until TTL or sessionEnd, whichever is
// earlier.
func (t *Tickets) Issue(p Principal, sessionEnd time.Time) (string, time.Time, error) {
	if p.TenantID == "" || p.UserID == "" {
		return "", time.Time{}, ErrUnauthenticated
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	now := t.Now()
	expires := now.Add(t.TTL)
	if !sessionEnd.IsZero() && sessionEnd.Before(expires) {
		expires = sessionEnd
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pending == nil {
		t.pending = map[[sha256.Size]byte]ticketEntry{}
	}
	if len(t.pending) >= t.Limit {
		t.purge(now)
		if len(t.pending) >= t.Limit {
			return "", time.Time{}, ErrTooManyTickets
		}
	}
	t.pending[sha256.Sum256([]byte(token))] = ticketEntry{ticket: Ticket{Principal: p, SessionEnd: sessionEnd}, expires: expires}
	return token, expires, nil
}

// Redeem consumes a ticket. Every ticket can be redeemed at most once.
func (t *Tickets) Redeem(token string) (Ticket, error) {
	if token == "" || len(token) > 128 {
		return Ticket{}, ErrInvalidTicket
	}
	key := sha256.Sum256([]byte(token))
	now := t.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.pending[key]
	if !ok {
		return Ticket{}, ErrInvalidTicket
	}
	delete(t.pending, key)
	if !now.Before(e.expires) {
		return Ticket{}, ErrInvalidTicket
	}
	return e.ticket, nil
}

func (t *Tickets) purge(now time.Time) {
	for k, e := range t.pending {
		if !now.Before(e.expires) {
			delete(t.pending, k)
		}
	}
}

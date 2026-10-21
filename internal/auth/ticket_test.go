// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"errors"
	"testing"
	"time"
)

func TestTickets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tk := NewTickets()
	tk.Now = func() time.Time { return now }
	p := Principal{UserID: "u1", TenantID: "t1", Roles: []Role{RoleTenantViewer}}

	// Single use (FA-AUTH-11).
	token, exp, err := tk.Issue(p, now.Add(time.Hour))
	if err != nil || !exp.Equal(now.Add(TicketTTL)) || len(token) < 40 {
		t.Fatalf("Issue = %q, %v, %v", token, exp, err)
	}
	got, err := tk.Redeem(token)
	if err != nil || got.Principal.TenantID != "t1" || got.Principal.UserID != "u1" || !got.SessionEnd.Equal(now.Add(time.Hour)) {
		t.Fatalf("Redeem = %+v, %v", got, err)
	}
	if _, err := tk.Redeem(token); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("second redeem: %v", err)
	}

	// Expiry after TTL.
	token, _, _ = tk.Issue(p, time.Time{})
	now = now.Add(TicketTTL)
	if _, err := tk.Redeem(token); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("expired ticket: %v", err)
	}

	// A session ending sooner shortens the ticket.
	token, exp, _ = tk.Issue(p, now.Add(5*time.Second))
	if !exp.Equal(now.Add(5 * time.Second)) {
		t.Errorf("expires = %v", exp)
	}
	now = now.Add(5 * time.Second)
	if _, err := tk.Redeem(token); !errors.Is(err, ErrInvalidTicket) {
		t.Errorf("ticket outlived session: %v", err)
	}

	for _, bad := range []string{"", "nope", string(make([]byte, 200))} {
		if _, err := tk.Redeem(bad); !errors.Is(err, ErrInvalidTicket) {
			t.Errorf("Redeem(%q): %v", bad, err)
		}
	}
	if _, _, err := tk.Issue(Principal{UserID: "u"}, time.Time{}); err == nil {
		t.Error("ticket without tenant issued")
	}
}

func TestTicketsLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tk := NewTickets()
	tk.Limit = 2
	tk.Now = func() time.Time { return now }
	p := Principal{UserID: "u1", TenantID: "t1"}
	for range 2 {
		if _, _, err := tk.Issue(p, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := tk.Issue(p, time.Time{}); !errors.Is(err, ErrTooManyTickets) {
		t.Fatalf("over limit: %v", err)
	}
	now = now.Add(TicketTTL)
	if _, _, err := tk.Issue(p, time.Time{}); err != nil {
		t.Errorf("expired tickets not purged: %v", err)
	}
}

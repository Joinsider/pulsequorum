// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

// auditEntry returns a user action on an object of the tenant.
func auditEntry(index uint64, sub int, atMs int64, tenant, action, objectKind, objectID, actorID string) store.AuditEntry {
	return store.AuditEntry{
		Index: index, Sub: sub, AtMs: atMs, TenantID: tenant,
		ActorKind: "user", ActorID: actorID, ActorName: "Actor " + actorID,
		Action: action, ObjectKind: objectKind, ObjectID: objectID, RequestID: fmt.Sprintf("req-%d", index),
	}
}

// insertAudit appends the entries in one transaction.
func insertAudit(t *testing.T, s *StateStore, entries ...store.AuditEntry) error {
	t.Helper()
	ctx := context.Background()
	return s.Update(ctx, func(tx store.StateTx) error { return tx.InsertAudit(ctx, entries) })
}

// auditStore holds six entries: a batch of two at index 5, two tenants and the system tenant.
func auditStore(t *testing.T) *StateStore {
	t.Helper()
	s := openState(t, t.TempDir())
	entries := []store.AuditEntry{
		auditEntry(1, 0, 1000, "t1", "monitor.create", "monitor", "m1", "u1"),
		auditEntry(2, 0, 2000, "t1", "monitor.update", "monitor", "m1", "u2"),
		auditEntry(3, 0, 3000, "t2", "monitor.create", "monitor", "m2", "u3"),
		auditEntry(4, 0, 4000, store.SystemTenantID, "tenant.create", "tenant", "t2", "u1"),
		auditEntry(5, 0, 5000, "t1", "monitor.pause", "monitor", "m2", "tok_1"),
		auditEntry(5, 1, 5000, "t1", "monitor.pause", "monitor", "m2", "tok_1"),
	}
	entries[4].ActorKind = "api_token"
	entries[5].ActorKind = "api_token"
	if err := insertAudit(t, s, entries...); err != nil {
		t.Fatal(err)
	}
	return s
}

// auditKeys names the entries as "index.sub" in order.
func auditKeys(entries []store.AuditEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%d.%d", e.Index, e.Sub))
	}
	return out
}

func TestListAuditFilters(t *testing.T) {
	tests := []struct {
		name string
		q    store.AuditQuery
		want []string
	}{
		{"no filter, newest first", store.AuditQuery{}, []string{"5.1", "5.0", "4.0", "3.0", "2.0", "1.0"}},
		{"tenant t1", store.AuditQuery{TenantID: "t1"}, []string{"5.1", "5.0", "2.0", "1.0"}},
		{"tenant t2", store.AuditQuery{TenantID: "t2"}, []string{"3.0"}},
		{"system tenant", store.AuditQuery{TenantID: store.SystemTenantID}, []string{"4.0"}},
		{"unknown tenant", store.AuditQuery{TenantID: "t9"}, nil},
		{"action", store.AuditQuery{Action: "monitor.create"}, []string{"3.0", "1.0"}},
		{"object kind and ID", store.AuditQuery{ObjectKind: "monitor", ObjectID: "m2"}, []string{"5.1", "5.0", "3.0"}},
		{"object kind only", store.AuditQuery{ObjectKind: "tenant"}, []string{"4.0"}},
		{"actor", store.AuditQuery{ActorID: "u1"}, []string{"4.0", "1.0"}},
		{"api token actor", store.AuditQuery{ActorID: "tok_1"}, []string{"5.1", "5.0"}},
		{"time window [2000, 4000)", store.AuditQuery{FromMs: 2000, ToMs: 4000}, []string{"3.0", "2.0"}},
		{"from only", store.AuditQuery{FromMs: 5000}, []string{"5.1", "5.0"}},
		{"tenant and action", store.AuditQuery{TenantID: "t1", Action: "monitor.pause"}, []string{"5.1", "5.0"}},
		{"tenant and actor", store.AuditQuery{TenantID: "t2", ActorID: "u1"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := auditStore(t)
			page, err := s.ListAudit(context.Background(), tc.q)
			if err != nil {
				t.Fatalf("ListAudit: %v", err)
			}
			if got := auditKeys(page.Entries); !slices.Equal(got, tc.want) {
				t.Errorf("entries = %v, want %v", got, tc.want)
			}
			if page.Next != nil {
				t.Errorf("Next = %+v on a page with all entries", page.Next)
			}
		})
	}
}

func TestListAuditFieldsAndTenantSeparation(t *testing.T) {
	s := auditStore(t)
	page, err := s.ListAudit(context.Background(), store.AuditQuery{TenantID: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	want := store.AuditEntry{Index: 3, Sub: 0, AtMs: 3000, TenantID: "t2", ActorKind: "user", ActorID: "u3",
		ActorName: "Actor u3", Action: "monitor.create", ObjectKind: "monitor", ObjectID: "m2", RequestID: "req-3"}
	if len(page.Entries) != 1 || page.Entries[0] != want {
		t.Errorf("t2 entries = %+v, want [%+v]", page.Entries, want)
	}
}

func TestListAuditCursorPaging(t *testing.T) {
	tests := []struct {
		name  string
		q     store.AuditQuery
		pages [][]string
	}{
		{"pages of two", store.AuditQuery{TenantID: "t1", Limit: 2},
			[][]string{{"5.1", "5.0"}, {"2.0", "1.0"}}},
		{"pages of one", store.AuditQuery{Limit: 1},
			[][]string{{"5.1"}, {"5.0"}, {"4.0"}, {"3.0"}, {"2.0"}, {"1.0"}}},
		{"page size equals entries", store.AuditQuery{TenantID: "t2", Limit: 1},
			[][]string{{"3.0"}}},
		{"batch sub-entries are ordered", store.AuditQuery{Action: "monitor.pause", Limit: 1},
			[][]string{{"5.1"}, {"5.0"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := auditStore(t)
			ctx := context.Background()
			q := tc.q
			var got [][]string
			for {
				page, err := s.ListAudit(ctx, q)
				if err != nil {
					t.Fatalf("ListAudit: %v", err)
				}
				got = append(got, auditKeys(page.Entries))
				if page.Next == nil {
					break
				}
				if len(got) > len(tc.pages) {
					t.Fatalf("more pages than %d", len(tc.pages))
				}
				q.After = page.Next
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.pages) {
				t.Errorf("pages = %v, want %v", got, tc.pages)
			}
		})
	}
}

func TestListAuditCursorIsStrict(t *testing.T) {
	s := auditStore(t)
	ctx := context.Background()
	tests := []struct {
		name   string
		cursor store.AuditCursor
		want   []string
	}{
		{"after 5.1", store.AuditCursor{Index: 5, Sub: 1}, []string{"5.0", "4.0", "3.0", "2.0", "1.0"}},
		{"after 5.0", store.AuditCursor{Index: 5, Sub: 0}, []string{"4.0", "3.0", "2.0", "1.0"}},
		{"after the oldest", store.AuditCursor{Index: 1, Sub: 0}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cursor := tc.cursor
			page, err := s.ListAudit(ctx, store.AuditQuery{After: &cursor, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if got := auditKeys(page.Entries); !slices.Equal(got, tc.want) {
				t.Errorf("entries = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListAuditLimit(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	var entries []store.AuditEntry
	for i := 1; i <= 205; i++ {
		entries = append(entries, auditEntry(uint64(i), 0, int64(i), "t1", "monitor.update", "monitor", "m1", "u1"))
	}
	if err := insertAudit(t, s, entries...); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		limit    int
		wantLen  int
		wantNext bool
	}{
		{"zero uses the default of 50", 0, 50, true},
		{"negative uses the default of 50", -3, 50, true},
		{"explicit 10", 10, 10, true},
		{"above the maximum is clamped to 200", 1000, 200, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.ListAudit(ctx, store.AuditQuery{TenantID: "t1", Limit: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Entries) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(page.Entries), tc.wantLen)
			}
			if (page.Next != nil) != tc.wantNext {
				t.Errorf("Next = %+v, want set = %v", page.Next, tc.wantNext)
			}
			if page.Next != nil && page.Entries[len(page.Entries)-1].Index != page.Next.Index {
				t.Errorf("Next = %+v does not name the last entry of the page", page.Next)
			}
		})
	}
}

func TestInsertAuditDuplicate(t *testing.T) {
	s := auditStore(t)
	err := insertAudit(t, s, auditEntry(5, 1, 9, "t1", "monitor.pause", "monitor", "m1", "tok_1"))
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate (index, sub): err = %v, want ErrConflict", err)
	}
	// A failed insert leaves nothing behind.
	page, err := s.ListAudit(context.Background(), store.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 6 {
		t.Errorf("entries after failed insert = %d, want 6", len(page.Entries))
	}
}

func TestPruneAudit(t *testing.T) {
	ctx := context.Background()
	s := openState(t, t.TempDir())
	if err := insertAudit(t, s,
		auditEntry(1, 0, 100, "t1", "monitor.create", "monitor", "m1", "u1"),
		auditEntry(2, 0, 200, "t1", "monitor.update", "monitor", "m1", "u1"),
		auditEntry(3, 0, 300, "t1", "monitor.delete", "monitor", "m1", "u1"),
	); err != nil {
		t.Fatal(err)
	}
	// AtMs before 250 goes: entries 1 and 2.
	if err := s.Update(ctx, func(tx store.StateTx) error { return tx.PruneAudit(ctx, 250) }); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListAudit(ctx, store.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := auditKeys(page.Entries); !slices.Equal(got, []string{"3.0"}) {
		t.Errorf("after prune = %v, want [3.0]", got)
	}
}

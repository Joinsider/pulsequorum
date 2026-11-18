// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite/statedb"
)

// Page sizes of the audit log (ADR 0031): 50 by default, at most 200.
const (
	defaultAuditPageSize = 50
	maxAuditPageSize     = 200
)

// ListAudit returns one page of the audit log, newest first. Empty filter
// fields do not filter; the cursor continues strictly after its entry.
// Next is set only if more entries exist.
func (s *StateStore) ListAudit(ctx context.Context, q store.AuditQuery) (store.AuditPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := clampLimit(q.Limit, defaultAuditPageSize, maxAuditPageSize)

	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if q.TenantID != "" {
		add("tenant_id = ?", q.TenantID)
	}
	if q.FromMs != 0 {
		add("at_ms >= ?", q.FromMs)
	}
	if q.ToMs != 0 {
		add("at_ms < ?", q.ToMs)
	}
	if q.Action != "" {
		add("action = ?", q.Action)
	}
	if q.ObjectKind != "" {
		add("object_kind = ?", q.ObjectKind)
	}
	if q.ObjectID != "" {
		add("object_id = ?", q.ObjectID)
	}
	if q.ActorID != "" {
		add("actor_id = ?", q.ActorID)
	}
	if q.After != nil {
		index, err := toInt64(q.After.Index)
		if err != nil {
			return store.AuditPage{}, fmt.Errorf("list audit: cursor: %w", err)
		}
		conds = append(conds, "(raft_index, sub) < (?, ?)")
		args = append(args, index, int64(q.After.Sub))
	}

	var sb strings.Builder
	sb.WriteString(`SELECT raft_index, sub, at_ms, tenant_id, actor_kind, actor_id, actor_name,
		action, object_kind, object_id, request_id FROM audit_log`)
	if len(conds) > 0 {
		sb.WriteString(" WHERE " + strings.Join(conds, " AND "))
	}
	sb.WriteString(" ORDER BY raft_index DESC, sub DESC LIMIT ?")
	args = append(args, int64(limit+1))

	// The conditions are fixed SQL fragments; every value is a bound parameter.
	rows, err := s.db.reader.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return store.AuditPage{}, mapErr(err)
	}
	defer func() { _ = rows.Close() }()

	page := store.AuditPage{Entries: make([]store.AuditEntry, 0, min(limit, 64))}
	for rows.Next() {
		var e store.AuditEntry
		var index, sub int64
		if err := rows.Scan(&index, &sub, &e.AtMs, &e.TenantID, &e.ActorKind, &e.ActorID, &e.ActorName,
			&e.Action, &e.ObjectKind, &e.ObjectID, &e.RequestID); err != nil {
			return store.AuditPage{}, mapErr(err)
		}
		e.Index = uint64(max(index, 0))
		e.Sub = int(sub)
		if len(page.Entries) == limit {
			// One entry more than the page: there is a next page.
			last := page.Entries[limit-1]
			page.Next = &store.AuditCursor{Index: last.Index, Sub: last.Sub}
			break
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return store.AuditPage{}, mapErr(err)
	}
	return page, nil
}

// InsertAudit appends the entries inside the caller's transaction. A duplicate
// (Index, Sub) is ErrConflict and the whole call fails.
func (t *stateTx) InsertAudit(ctx context.Context, entries []store.AuditEntry) error {
	for _, e := range entries {
		if e.Sub < 0 {
			return fmt.Errorf("insert audit: negative sub %d", e.Sub)
		}
		index, err := toInt64(e.Index)
		if err != nil {
			return fmt.Errorf("insert audit: %w", err)
		}
		if err := t.q.InsertAuditEntry(ctx, statedb.InsertAuditEntryParams{
			RaftIndex: index, Sub: int64(e.Sub), AtMs: e.AtMs, TenantID: e.TenantID,
			ActorKind: e.ActorKind, ActorID: e.ActorID, ActorName: e.ActorName,
			Action: e.Action, ObjectKind: e.ObjectKind, ObjectID: e.ObjectID, RequestID: e.RequestID,
		}); err != nil {
			return fmt.Errorf("insert audit %d/%d: %w", e.Index, e.Sub, mapErr(err))
		}
	}
	return nil
}

// PruneAudit deletes the audit entries with AtMs before beforeMs.
func (t *stateTx) PruneAudit(ctx context.Context, beforeMs int64) error {
	return mapErr(t.q.PruneAudit(ctx, beforeMs))
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const metaAppliedIndex = "applied_index"

// fingerprintTables lists the replicated tables with a total order. fsm_meta
// is excluded: the applied index legitimately differs between replicas that
// are at different log positions.
var fingerprintTables = []struct{ name, orderBy string }{
	{"tenants", "id"},
	{"monitors", "tenant_id, id"},
	{"monitor_tags", "tenant_id, monitor_id, tag"},
	{"request_dedup", "request_id"},
	{"secret_keys", "key_id"},
	{"monitor_states", "tenant_id, monitor_id"},
	{"incidents", "tenant_id, id"},
	{"monitor_status_history", "tenant_id, monitor_id, since_ms"},
	{"notification_channels", "tenant_id, id"},
	{"monitor_channels", "tenant_id, monitor_id, channel_id"},
	{"alert_outbox", "alert_key, channel_id"},
	{"maintenance_windows", "tenant_id, id"},
	{"users", "tenant_id, id"},
	{"signing_keys", "kid"},
	{"refresh_sessions", "token_hash"},
	{"cluster_ca", "fingerprint"},
	{"join_tokens", "id"},
	{"revoked_certs", "serial"},
}

// AppliedIndex returns the last applied Raft index stored in state.db.
func (s *StateStore) AppliedIndex(ctx context.Context) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, err := s.q.GetFSMMeta(ctx, metaAppliedIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read applied index: %w", err)
	}
	return uint64(max(v, 0)), nil
}

// WriteSnapshot writes a consistent copy of state.db to w using VACUUM INTO
// (ADR 0012 A1). It runs on the read pool and does not block writers, so the
// copy may be newer than the Raft index the snapshot is taken at; that is safe
// because the applied index is part of the copy.
func (s *StateStore) WriteSnapshot(ctx context.Context, w io.Writer) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tmp, err := tempPath(filepath.Dir(s.path), ".snapshot-*.db")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()

	conn, err := s.db.reader.Conn(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// VACUUM INTO writes a new file and is allowed despite query_only.
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = 0"); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	_, vacErr := conn.ExecContext(ctx, "VACUUM INTO ?", tmp)
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		// Never return a writable connection to the read pool.
		_ = conn.Raw(func(any) error { return errors.New("discard connection") })
		return fmt.Errorf("snapshot: restore query_only: %w", err)
	}
	if vacErr != nil {
		return fmt.Errorf("snapshot: vacuum into: %w", vacErr)
	}

	f, err := os.Open(tmp) //nolint:gosec // G304: temp file created above inside the data directory
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("snapshot: copy: %w", err)
	}
	return nil
}

// RestoreSnapshot replaces state.db with the database read from r and reopens
// it (ADR 0012 A1). Readers block for the duration of the swap.
func (s *StateStore) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".restore-*.db")
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restore: copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restore: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	// Validate before touching the current database, so a corrupt snapshot
	// never replaces good state.
	if err := validateSnapshot(ctx, tmp.Name()); err != nil {
		return fmt.Errorf("restore: invalid snapshot: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("restore: close current state: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(s.path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("restore: remove %s: %w", suffix, err)
		}
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("restore: replace state.db: %w", err)
	}
	if err := s.open(ctx); err != nil {
		return fmt.Errorf("restore: reopen: %w", err)
	}
	return nil
}

// validateSnapshot opens the file read-only and checks that it is an intact
// SQLite database with the FSM tables.
func validateSnapshot(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check: %s", result)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('fsm_meta', 'tenants')").Scan(&n); err != nil {
		return err
	}
	if n != 2 {
		return errors.New("not an UptimeCluster state database")
	}
	return nil
}

// Fingerprint hashes all replicated rows in a fixed order. Two replicas that
// applied the same log entries have the same fingerprint (FA-CLU-07, ADR 0012 A7).
func (s *StateStore) Fingerprint(ctx context.Context) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tx, err := s.db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", fmt.Errorf("fingerprint: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	h := sha256.New()
	for _, t := range fingerprintTables {
		if err := hashTable(ctx, tx, h, t.name, t.orderBy); err != nil {
			return "", fmt.Errorf("fingerprint %s: %w", t.name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashTable(ctx context.Context, tx *sql.Tx, w io.Writer, table, orderBy string) error {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY "+orderBy) //nolint:gosec // G202: table and order come from the fixed list above
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "table %s %v\n", table, cols)
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range values {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			_, _ = fmt.Fprintf(w, "%T:%v\x1f", v, v)
		}
		_, _ = io.WriteString(w, "\x1e")
	}
	return rows.Err()
}

// tempPath returns a unique, non-existing path in dir (VACUUM INTO refuses to
// overwrite an existing file).
func tempPath(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	return name, nil
}

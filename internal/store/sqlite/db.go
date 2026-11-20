// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package sqlite implements the store interfaces on SQLite with the CGO-free
// modernc.org/sqlite driver (ADR 0004, 0013).
//
// Every database file is opened twice: a writer pool with exactly one
// connection and immediate transactions, so writers never race for the lock
// (PF-22), and a read-only pool for concurrent readers.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Synchronous is the value of PRAGMA synchronous.
type Synchronous int

// Durability levels (ADR 0004: FULL for state.db, NORMAL for metrics.db).
const (
	SyncNormal Synchronous = 1
	SyncFull   Synchronous = 2
)

const (
	busyTimeoutMs    = 5000
	journalSizeLimit = 64 << 20
)

// Options configure one database file.
type Options struct {
	Path        string
	Synchronous Synchronous
	// ReadConns is the size of the read pool (ADR 0013 C4: 4 for metrics.db).
	ReadConns int
	// CacheSizeKiB sets PRAGMA cache_size on the writer; 0 keeps the SQLite default.
	CacheSizeKiB int
	// Migrations holds goose SQL migrations at the root of the FS.
	Migrations fs.FS
}

// DB is one SQLite file with a single-connection writer and a read pool.
type DB struct {
	path   string
	opts   Options
	writer *sql.DB
	reader *sql.DB
}

// Open opens the file, applies migrations and verifies the effective pragmas on
// every pooled connection (FA-NODE-06). The directory must exist.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if strings.ContainsAny(opts.Path, "?#") {
		return nil, fmt.Errorf("sqlite: path %q must not contain '?' or '#'", opts.Path)
	}
	if opts.ReadConns < 1 {
		opts.ReadConns = 1
	}

	writer, err := sql.Open("sqlite", writerDSN(opts))
	if err != nil {
		return nil, fmt.Errorf("open writer %s: %w", opts.Path, err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)

	// The writer creates the file and switches it to WAL before any reader opens it.
	if err := writer.PingContext(ctx); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open writer %s: %w", opts.Path, err)
	}

	reader, err := sql.Open("sqlite", readerDSN(opts))
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader %s: %w", opts.Path, err)
	}
	reader.SetMaxOpenConns(opts.ReadConns)
	reader.SetMaxIdleConns(opts.ReadConns)

	db := &DB{path: opts.Path, opts: opts, writer: writer, reader: reader}
	if opts.Migrations != nil {
		if err := db.migrate(ctx, opts.Migrations); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := db.SelfTest(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Pragma order matters: auto_vacuum must precede journal_mode, otherwise some
// drivers leave auto_vacuum at 0 (ADR 0013 C3).
func basePragmas(opts Options) []string {
	return []string{
		"auto_vacuum(INCREMENTAL)",
		"journal_mode(WAL)",
		"busy_timeout(" + strconv.Itoa(busyTimeoutMs) + ")",
		"synchronous(" + strconv.Itoa(int(opts.Synchronous)) + ")",
		"foreign_keys(1)",
	}
}

func writerDSN(opts Options) string {
	pragmas := append(basePragmas(opts), "journal_size_limit("+strconv.Itoa(journalSizeLimit)+")")
	if opts.CacheSizeKiB > 0 {
		pragmas = append(pragmas, "cache_size(-"+strconv.Itoa(opts.CacheSizeKiB)+")")
	}
	return dsn(opts.Path, pragmas, "_txlock=immediate")
}

func readerDSN(opts Options) string {
	return dsn(opts.Path, append(basePragmas(opts), "query_only(1)"))
}

func dsn(path string, pragmas []string, extra ...string) string {
	params := make([]string, 0, len(pragmas)+len(extra))
	for _, p := range pragmas {
		params = append(params, "_pragma="+p)
	}
	params = append(params, extra...)
	return "file:" + path + "?" + strings.Join(params, "&")
}

func (db *DB) migrate(ctx context.Context, migrations fs.FS) error {
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.writer, migrations)
	if err != nil {
		return fmt.Errorf("migrations for %s: %w", db.path, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate %s: %w", db.path, err)
	}
	return nil
}

// SelfTest reads back the pragmas on the writer and on every reader connection.
// A typo in the DSN would otherwise silently leave the database without WAL or
// busy timeout (PF-22).
func (db *DB) SelfTest(ctx context.Context) error {
	want := map[string]string{
		"journal_mode": "wal",
		"busy_timeout": strconv.Itoa(busyTimeoutMs),
		"synchronous":  strconv.Itoa(int(db.opts.Synchronous)),
		"foreign_keys": "1",
		"auto_vacuum":  "2",
	}

	wconn, err := db.writer.Conn(ctx)
	if err != nil {
		return fmt.Errorf("self-test %s: %w", db.path, err)
	}
	writerWant := map[string]string{"journal_size_limit": strconv.Itoa(journalSizeLimit)}
	for k, v := range want {
		writerWant[k] = v
	}
	err = checkPragmas(ctx, wconn, writerWant)
	_ = wconn.Close()
	if err != nil {
		return fmt.Errorf("self-test %s writer: %w", db.path, err)
	}

	// Hold all reader connections at once so every pooled connection is checked.
	conns := make([]*sql.Conn, 0, db.opts.ReadConns)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < db.opts.ReadConns; i++ {
		c, err := db.reader.Conn(ctx)
		if err != nil {
			return fmt.Errorf("self-test %s: %w", db.path, err)
		}
		conns = append(conns, c)
		if err := checkPragmas(ctx, c, want); err != nil {
			return fmt.Errorf("self-test %s reader %d: %w", db.path, i, err)
		}
	}
	return nil
}

func checkPragmas(ctx context.Context, conn *sql.Conn, want map[string]string) error {
	var errs []error
	for name, expected := range want {
		var got string
		if err := conn.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", name, err))
			continue
		}
		if !strings.EqualFold(got, expected) {
			errs = append(errs, fmt.Errorf("pragma %s = %s, want %s", name, got, expected))
		}
	}
	return errors.Join(errs...)
}

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// CheckpointResult reports the outcome of a WAL checkpoint.
type CheckpointResult struct {
	// Busy is true if readers prevented a complete checkpoint.
	Busy         bool
	LogFrames    int
	Checkpointed int
}

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) on the writer (ADR 0013 C4).
func (db *DB) Checkpoint(ctx context.Context) (CheckpointResult, error) {
	var busy, logFrames, checkpointed int
	err := db.writer.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		return CheckpointResult{}, fmt.Errorf("checkpoint %s: %w", db.path, err)
	}
	return CheckpointResult{Busy: busy != 0, LogFrames: logFrames, Checkpointed: checkpointed}, nil
}

// IncrementalVacuum returns up to pages free pages to the file system
// (0 = all free pages).
func (db *DB) IncrementalVacuum(ctx context.Context, pages int) error {
	if _, err := db.writer.ExecContext(ctx, "PRAGMA incremental_vacuum("+strconv.Itoa(pages)+")"); err != nil {
		return fmt.Errorf("incremental vacuum %s: %w", db.path, err)
	}
	return nil
}

// WALSize returns the size of the write-ahead log in bytes (0 if absent).
func (db *DB) WALSize() (int64, error) {
	info, err := os.Stat(db.path + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat wal %s: %w", db.path, err)
	}
	return info.Size(), nil
}

// RunCheckpointer checkpoints periodically until ctx is cancelled and logs
// incomplete checkpoints, which indicate checkpoint starvation (PF-24).
func (db *DB) RunCheckpointer(ctx context.Context, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		res, err := db.Checkpoint(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("wal checkpoint failed", "db", db.path, "err", err)
			}
			continue
		}
		if res.Busy {
			size, _ := db.WALSize()
			log.Warn("wal checkpoint incomplete, readers still active", "db", db.path,
				"log_frames", res.LogFrames, "checkpointed", res.Checkpointed, "wal_bytes", size)
		}
	}
}

// Close closes both pools.
func (db *DB) Close() error {
	return errors.Join(db.reader.Close(), db.writer.Close())
}

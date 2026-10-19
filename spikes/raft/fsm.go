package main

// Deterministic FSM (ADR 0005): commands carry leader-stamped time and
// UUIDv7; Apply materialises into SQLite (state.db, data class A) in one
// transaction per log entry together with the applied raft index and the
// request-ID dedup row. Snapshots are VACUUM INTO copies of state.db.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
	_ "modernc.org/sqlite"
)

const dedupRetentionMs = 24 * 60 * 60 * 1000

type Command struct {
	RequestID  string `json:"request_id"`
	IssuedAtMs int64  `json:"issued_at_ms"` // stamped by the leader
	VersionID  string `json:"version_id"`   // UUIDv7, stamped by the leader
	Op         string `json:"op"`           // "set" | "bulk"
	Key        string `json:"key"`
	Value      string `json:"value"`
	N          int    `json:"n,omitempty"` // bulk: number of rows
}

type ApplyResult struct {
	Index     uint64 `json:"index"`
	VersionID string `json:"version_id"`
	Deduped   bool   `json:"deduped"`
	Err       string `json:"err,omitempty"`
}

const stateSchema = `
CREATE TABLE IF NOT EXISTS config(
  key TEXT PRIMARY KEY, value TEXT NOT NULL, version_id TEXT NOT NULL, updated_at_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS request_dedup(
  request_id TEXT PRIMARY KEY, applied_index INTEGER NOT NULL, issued_at_ms INTEGER NOT NULL, result TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS request_dedup_issued ON request_dedup(issued_at_ms);
CREATE TABLE IF NOT EXISTS raft_meta(id INTEGER PRIMARY KEY CHECK(id=1), applied_index INTEGER NOT NULL);
INSERT OR IGNORE INTO raft_meta(id, applied_index) VALUES(1, 0);
`

type FSM struct {
	dir string

	mu      sync.RWMutex // write-locked only during Restore (file swap)
	w       *sql.DB      // single writer connection
	r       *sql.DB      // reader pool
	applied atomic.Uint64

	skipped atomic.Int64 // log entries skipped because state.db was already ahead
}

func openSQLite(path string, maxConns int) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	return db, nil
}

func newFSM(dir string) (*FSM, error) {
	f := &FSM{dir: dir}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *FSM) path() string { return filepath.Join(f.dir, "state.db") }

func (f *FSM) open() error {
	var err error
	if f.w, err = openSQLite(f.path(), 1); err != nil {
		return err
	}
	if f.r, err = openSQLite(f.path(), 4); err != nil {
		return err
	}
	if _, err = f.w.Exec(stateSchema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	var applied uint64
	if err := f.w.QueryRow(`SELECT applied_index FROM raft_meta WHERE id=1`).Scan(&applied); err != nil {
		return err
	}
	f.applied.Store(applied)
	return nil
}

func (f *FSM) close() {
	_ = f.w.Close()
	_ = f.r.Close()
}

func (f *FSM) AppliedIndex() uint64 {
	return f.applied.Load()
}

// Apply must be deterministic: no clock, no randomness, only the local store.
func (f *FSM) Apply(l *raft.Log) interface{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if l.Index <= f.applied.Load() {
		// state.db already contains this entry (fast restart or fuzzy snapshot).
		f.skipped.Add(1)
		return ApplyResult{Index: l.Index, Err: "skipped"}
	}
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		return ApplyResult{Index: l.Index, Err: "decode: " + err.Error()}
	}
	res, err := f.applyTx(l.Index, cmd)
	if err != nil {
		// A real implementation must panic/halt here: a diverging FSM is worse than a dead node.
		slog.Error("fsm apply failed", "index", l.Index, "err", err)
		return ApplyResult{Index: l.Index, Err: err.Error()}
	}
	f.applied.Store(l.Index)
	return res
}

func (f *FSM) applyTx(index uint64, cmd Command) (ApplyResult, error) {
	tx, err := f.w.Begin()
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var stored string
	err = tx.QueryRow(`SELECT result FROM request_dedup WHERE request_id=?`, cmd.RequestID).Scan(&stored)
	switch {
	case err == nil:
		var prev ApplyResult
		_ = json.Unmarshal([]byte(stored), &prev)
		prev.Deduped = true
		if _, err := tx.Exec(`UPDATE raft_meta SET applied_index=? WHERE id=1`, index); err != nil {
			return ApplyResult{}, err
		}
		return prev, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return ApplyResult{}, err
	}
	err = nil

	switch cmd.Op {
	case "set":
		_, err = tx.Exec(`INSERT INTO config(key,value,version_id,updated_at_ms) VALUES(?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, version_id=excluded.version_id, updated_at_ms=excluded.updated_at_ms`,
			cmd.Key, cmd.Value, cmd.VersionID, cmd.IssuedAtMs)
	case "bulk":
		// Deterministic bulk load to get a realistically sized state (FA-CLU-09: 10k monitors).
		stmt, perr := tx.Prepare(`INSERT INTO config(key,value,version_id,updated_at_ms) VALUES(?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, version_id=excluded.version_id, updated_at_ms=excluded.updated_at_ms`)
		if perr != nil {
			return ApplyResult{}, perr
		}
		for i := 0; i < cmd.N && err == nil; i++ {
			_, err = stmt.Exec(fmt.Sprintf("%s-%06d", cmd.Key, i),
				fmt.Sprintf(`{"name":"monitor %d","url":"https://example.com/%d","interval":60,"pad":"%0400d"}`, i, i, i),
				cmd.VersionID, cmd.IssuedAtMs)
		}
		_ = stmt.Close()
	default:
		err = fmt.Errorf("unknown op %q", cmd.Op)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{Index: index, VersionID: cmd.VersionID}
	resJSON, _ := json.Marshal(res)
	if _, err := tx.Exec(`INSERT INTO request_dedup(request_id,applied_index,issued_at_ms,result) VALUES(?,?,?,?)`,
		cmd.RequestID, index, cmd.IssuedAtMs, string(resJSON)); err != nil {
		return ApplyResult{}, err
	}
	// Retention based on the command's own timestamp keeps the purge deterministic.
	if _, err := tx.Exec(`DELETE FROM request_dedup WHERE issued_at_ms < ?`, cmd.IssuedAtMs-dedupRetentionMs); err != nil {
		return ApplyResult{}, err
	}
	if _, err := tx.Exec(`UPDATE raft_meta SET applied_index=? WHERE id=1`, index); err != nil {
		return ApplyResult{}, err
	}
	return res, tx.Commit()
}

// Snapshot is called on the FSM goroutine and must be cheap. The actual
// VACUUM INTO happens in Persist, concurrently with further Applies. The copy
// may therefore contain entries newer than the snapshot index ("fuzzy"
// snapshot); that is safe because raft_meta.applied_index travels inside the
// copy and Apply skips everything at or below it.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{f: f}, nil
}

type fsmSnapshot struct{ f *FSM }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	start := time.Now()
	tmp := filepath.Join(s.f.dir, fmt.Sprintf("snap-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	s.f.mu.RLock()
	_, err := s.f.r.Exec(`VACUUM INTO ?`, tmp)
	s.f.mu.RUnlock()
	if err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("vacuum into: %w", err)
	}
	vacuumDur := time.Since(start)
	fh, err := os.Open(tmp)
	if err != nil {
		_ = sink.Cancel()
		return err
	}
	defer fh.Close()
	n, err := io.Copy(sink, fh)
	if err != nil {
		_ = sink.Cancel()
		return err
	}
	slog.Info("snapshot persisted", "bytes", n, "vacuum_ms", vacuumDur.Milliseconds(), "total_ms", time.Since(start).Milliseconds())
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// Restore swaps state.db atomically with the received snapshot file.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	start := time.Now()
	tmp := f.path() + ".restore"
	fh, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(fh, rc)
	if err == nil {
		err = fh.Sync()
	}
	_ = fh.Close()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.close()
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(f.path() + suffix)
	}
	if err := os.Rename(tmp, f.path()); err != nil {
		return err
	}
	if err := f.open(); err != nil {
		return err
	}
	slog.Info("snapshot restored", "bytes", n, "applied_index", f.applied.Load(), "ms", time.Since(start).Milliseconds())
	return nil
}

func (f *FSM) Get(key string) (value, version string, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	err := f.r.QueryRow(`SELECT value, version_id FROM config WHERE key=?`, key).Scan(&value, &version)
	return value, version, err == nil
}

func (f *FSM) Stats() map[string]any {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var dedup int
	_ = f.r.QueryRow(`SELECT count(*) FROM request_dedup`).Scan(&dedup)
	// content fingerprint to compare FSMs across nodes
	h := sha256.New()
	rows := 0
	if q, err := f.r.Query(`SELECT key, value, version_id, updated_at_ms FROM config ORDER BY key`); err == nil {
		for q.Next() {
			var k, v, ver string
			var ts int64
			_ = q.Scan(&k, &v, &ver, &ts)
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\n", k, v, ver, ts)
			rows++
		}
		_ = q.Close()
	}
	return map[string]any{
		"rows": rows, "dedup_rows": dedup, "applied_index": f.applied.Load(),
		"fingerprint": hex.EncodeToString(h.Sum(nil))[:16], "skipped_on_replay": f.skipped.Load(),
	}
}

package main

// Benchmark-Kern: Setup nach ADR 0004, Datengenerator, Messphasen.
// Wegwerf-Spike, keine Produktlogik.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numMonitors     = 1000
	numNodes        = 3
	numTenants      = 20
	intervalMs      = 20_000 // 20-s-Prüfintervall
	rowsPerInterval = numMonitors * numNodes
	batchSize       = 500 // Insert-Batch: Zeilen pro Transaktion
	numReaders      = 8
	deleteBatchSize = 5000
	epochMs         = 1_800_000_000_000
	dayMs           = 24 * 3600 * 1000
	incarnation     = "inc-1"
)

const insertSQL = `INSERT INTO heartbeats
  (node_id, incarnation, seq, tenant_id, monitor_id, ts_ms, status, latency_ms, error_class, message)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`

// Erwartete Pragma-Werte nach dem Setup (Read-back, ADR 0004 / FA-NODE-06).
var expectedPragmas = map[string]string{
	"journal_mode":       "wal",
	"busy_timeout":       "5000",
	"synchronous":        "1", // NORMAL
	"foreign_keys":       "1",
	"auto_vacuum":        "2", // INCREMENTAL
	"journal_size_limit": "67108864",
}

type drvCfg struct {
	sqlName   string
	writerDSN func(path string) string
	readerDSN func(path string) string
}

func configFor(driver string) (drvCfg, error) {
	common := "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	switch driver {
	case "modernc":
		return drvCfg{
			sqlName: "sqlite",
			writerDSN: func(p string) string {
				return fmt.Sprintf("file:%s?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(67108864)&%s&_txlock=immediate", p, common)
			},
			readerDSN: func(p string) string { return fmt.Sprintf("file:%s?mode=ro&%s", p, common) },
		}, nil
	case "ncruces":
		return drvCfg{
			sqlName: "sqlite3",
			writerDSN: func(p string) string {
				return fmt.Sprintf("file:%s?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(67108864)&%s&_txlock=immediate", p, common)
			},
			readerDSN: func(p string) string { return fmt.Sprintf("file:%s?mode=ro&%s", p, common) },
		}, nil
	}
	return drvCfg{}, fmt.Errorf("unknown driver %q (want modernc or ncruces)", driver)
}

func heartbeatsDDL(variant string) string {
	suffix := ""
	if variant == "without-rowid" {
		suffix = " WITHOUT ROWID"
	}
	return `CREATE TABLE heartbeats (
  node_id TEXT NOT NULL,
  incarnation TEXT NOT NULL,
  seq INTEGER NOT NULL,
  tenant_id TEXT NOT NULL,
  monitor_id TEXT NOT NULL,
  ts_ms INTEGER NOT NULL,
  status INTEGER NOT NULL,
  latency_ms INTEGER,
  error_class TEXT,
  message TEXT,
  PRIMARY KEY (node_id, incarnation, seq)
)` + suffix
}

func createSchema(ctx context.Context, w *sql.DB, variant string) error {
	stmts := []string{
		"PRAGMA auto_vacuum = INCREMENTAL", // vor den Tabellen, ADR 0004
		heartbeatsDDL(variant),
		"CREATE INDEX idx_hb_monitor_ts ON heartbeats (monitor_id, ts_ms)",
		"CREATE INDEX idx_hb_ts ON heartbeats (ts_ms)",
	}
	for _, s := range stmts {
		if _, err := w.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("schema %q: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// readPragmas liest die Pragmas auf einer konkreten Verbindung zurück.
func readPragmas(ctx context.Context, c *sql.Conn) (map[string]string, error) {
	out := make(map[string]string, len(expectedPragmas))
	for p := range expectedPragmas {
		var v string
		if err := c.QueryRowContext(ctx, "PRAGMA "+p).Scan(&v); err != nil {
			return nil, fmt.Errorf("PRAGMA %s: %w", p, err)
		}
		out[p] = v
	}
	return out, nil
}

// checkPragmas vergleicht die Read-back-Werte. journal_size_limit ist eine
// Verbindungseinstellung und wird nur auf dem Schreib-Handle geprueft.
func checkPragmas(got map[string]string, reader bool) error {
	var bad []string
	for k, want := range expectedPragmas {
		if reader && k == "journal_size_limit" {
			continue
		}
		if got[k] != want {
			bad = append(bad, fmt.Sprintf("%s=%q (want %q)", k, got[k], want))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("pragma mismatch: %s", strings.Join(bad, ", "))
	}
	return nil
}

// ---------- Datengenerator ----------

type hb struct {
	node, inc, tenant, monitor string
	seq, ts, status, latency   int64
	errClass, msg              any // nil -> SQL NULL
}

type generator struct {
	rng      *rand.Rand
	nodes    [numNodes]string
	monitors [numMonitors]string
	tenants  [numMonitors]string
	k        int64 // Prüfintervall-Index
	n, m     int   // Cursor innerhalb des Intervalls: Knoten, Monitor
}

func newGenerator(seed int64) *generator {
	g := &generator{rng: rand.New(rand.NewSource(seed))}
	for i := range g.nodes {
		g.nodes[i] = fmt.Sprintf("node-%d", i+1)
	}
	for i := range g.monitors {
		g.monitors[i] = fmt.Sprintf("mon-%04d", i)
		g.tenants[i] = fmt.Sprintf("tenant-%02d", i%numTenants)
	}
	return g
}

// next liefert den nächsten Heartbeat in fester Reihenfolge:
// Intervall -> Knoten -> Monitor. Jeder der 3 Knoten prüft jeden Monitor
// alle 20 s (3000 Zeilen je Intervall, ~13 Mio./Tag).
func (g *generator) next() hb {
	h := hb{
		node:    g.nodes[g.n],
		inc:     incarnation,
		tenant:  g.tenants[g.m],
		monitor: g.monitors[g.m],
		seq:     g.k*numMonitors + int64(g.m),
		ts:      epochMs + g.k*intervalMs + int64(g.m)*20 + int64(g.n),
	}
	switch r := g.rng.Intn(1000); {
	case r < 5:
		h.status, h.latency = 2, 10000
		h.errClass, h.msg = "timeout", "no response within 10000 ms"
	case r < 10:
		h.status, h.latency = 1, 50+int64(g.rng.Intn(200))
		h.errClass, h.msg = "http_5xx", "HTTP 503"
	default:
		h.status, h.latency = 0, 20+int64(g.rng.Intn(280))
	}
	g.m++
	if g.m == numMonitors {
		g.m = 0
		g.n++
		if g.n == numNodes {
			g.n = 0
			g.k++
		}
	}
	return h
}

func (g *generator) batch(buf []hb) []hb {
	buf = buf[:0]
	for i := 0; i < batchSize; i++ {
		buf = append(buf, g.next())
	}
	return buf
}

// ---------- Schreiben / Lesen ----------

func insertBatch(ctx context.Context, w *sql.DB, rows []hb) error {
	tx, err := w.BeginTx(ctx, nil) // _txlock=immediate aus dem DSN
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare: %w", err)
	}
	for i := range rows {
		h := &rows[i]
		if _, err := stmt.ExecContext(ctx, h.node, h.inc, h.seq, h.tenant, h.monitor,
			h.ts, h.status, h.latency, h.errClass, h.msg); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("insert: %w", err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("close stmt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func queryRange(ctx context.Context, r *sql.DB, mon string, from, to int64) (int, error) {
	rows, err := r.QueryContext(ctx,
		`SELECT ts_ms, status, latency_ms FROM heartbeats WHERE monitor_id = ? AND ts_ms BETWEEN ? AND ?`,
		mon, from, to)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	var ts, st, lat int64
	for rows.Next() {
		if err := rows.Scan(&ts, &st, &lat); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func isBusy(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "busy") || strings.Contains(s, "locked")
}

// ---------- Messphasen ----------

type loadStats struct {
	sec   float64
	txDur []time.Duration
	rows  int64
}

func loadPhase(ctx context.Context, w *sql.DB, g *generator, intervals int64) (loadStats, error) {
	var st loadStats
	total := intervals * rowsPerInterval
	buf := make([]hb, 0, batchSize)
	start := time.Now()
	for done := int64(0); done < total; done += batchSize {
		buf = g.batch(buf)
		t := time.Now()
		if err := insertBatch(ctx, w, buf); err != nil {
			return st, fmt.Errorf("load at row %d: %w", done, err)
		}
		st.txDur = append(st.txDur, time.Since(t))
		st.rows += batchSize
	}
	st.sec = time.Since(start).Seconds()
	return st, nil
}

type concStats struct {
	sec        float64
	writerRows int64
	writerTx   []time.Duration
	readerDur  []time.Duration
	busy       int64
	other      int64
	firstOther string
	maxWALMB   float64
}

type concParams struct {
	w        *sql.DB
	r        *sql.DB
	g        *generator
	monitors []string
	dur      time.Duration
	think    time.Duration
	lastTs   *atomic.Int64
	dbPath   string
}

// concurrentPhase: ein Schreiber fügt kontinuierlich neue Intervalle ein,
// während numReaders Leser 24-h-Bereichsabfragen auf zufälligen Monitoren fahren.
func concurrentPhase(ctx context.Context, p concParams) concStats {
	var (
		st    concStats
		stop  atomic.Bool
		busy  atomic.Int64
		other atomic.Int64
		mu    sync.Mutex
		wg    sync.WaitGroup
	)
	noteErr := func(err error) {
		if isBusy(err) {
			busy.Add(1)
			return
		}
		other.Add(1)
		mu.Lock()
		if st.firstOther == "" {
			st.firstOther = err.Error()
		}
		mu.Unlock()
	}

	start := time.Now()
	timer := time.AfterFunc(p.dur, func() { stop.Store(true) })
	defer timer.Stop()

	wg.Add(1)
	go runConcurrentWriter(ctx, p, &stop, &wg, &st, noteErr)

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go runConcurrentReader(ctx, p, int64(i+1), &stop, &wg, &mu, &st, noteErr)
	}

	// WAL-Groesse waehrend der Last beobachten (Wachstum durch ueberlappende Leser).
	sampleDone := make(chan struct{})
	sampleExited := make(chan struct{})
	go sampleConcurrentWAL(p.dbPath, sampleDone, sampleExited, &st)

	wg.Wait()
	close(sampleDone)
	<-sampleExited
	st.sec = time.Since(start).Seconds()
	st.busy = busy.Load()
	st.other = other.Load()
	return st
}

func runConcurrentWriter(ctx context.Context, p concParams, stop *atomic.Bool, wg *sync.WaitGroup, st *concStats, noteErr func(error)) {
	defer wg.Done()
	buf := make([]hb, 0, batchSize)
	for !stop.Load() {
		buf = p.g.batch(buf)
		t := time.Now()
		if err := insertBatch(ctx, p.w, buf); err != nil {
			noteErr(err)
			continue
		}
		st.writerTx = append(st.writerTx, time.Since(t))
		st.writerRows += batchSize
		var mx int64
		for i := range buf {
			if buf[i].ts > mx {
				mx = buf[i].ts
			}
		}
		p.lastTs.Store(mx)
	}
}

func runConcurrentReader(ctx context.Context, p concParams, seed int64, stop *atomic.Bool, wg *sync.WaitGroup, mu *sync.Mutex, st *concStats, noteErr func(error)) {
	defer wg.Done()
	rng := rand.New(rand.NewSource(seed))
	var local []time.Duration
	for !stop.Load() {
		mon := p.monitors[rng.Intn(len(p.monitors))]
		end := p.lastTs.Load()
		t := time.Now()
		if _, err := queryRange(ctx, p.r, mon, end-dayMs, end); err != nil {
			noteErr(err)
			continue
		}
		local = append(local, time.Since(t))
		if p.think > 0 {
			time.Sleep(p.think) // optionales Denkpause-Intervall pro Leser (nicht gemessen)
		}
	}
	mu.Lock()
	st.readerDur = append(st.readerDur, local...)
	mu.Unlock()
}

func sampleConcurrentWAL(dbPath string, sampleDone <-chan struct{}, sampleExited chan<- struct{}, st *concStats) {
	defer close(sampleExited)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-sampleDone:
			return
		case <-tick.C:
			if mb := fileMB(dbPath + "-wal"); mb > st.maxWALMB {
				st.maxWALMB = mb
			}
		}
	}
}

type retStats struct {
	toDelete int64
	deleted  int64
	batches  int
	sec      float64
	batchDur []time.Duration
}

// retention löscht in Batches à 5.000 Zeilen, jeweils in eigener Immediate-Transaktion.
func retention(ctx context.Context, w *sql.DB, variant string, cutoff int64) (retStats, error) {
	var st retStats
	if err := w.QueryRowContext(ctx, "SELECT COUNT(*) FROM heartbeats WHERE ts_ms < ?", cutoff).Scan(&st.toDelete); err != nil {
		return st, fmt.Errorf("count: %w", err)
	}
	var delSQL string
	if variant == "without-rowid" {
		delSQL = fmt.Sprintf(`DELETE FROM heartbeats WHERE (node_id, incarnation, seq) IN
		  (SELECT node_id, incarnation, seq FROM heartbeats WHERE ts_ms < ? LIMIT %d)`, deleteBatchSize)
	} else {
		delSQL = fmt.Sprintf(`DELETE FROM heartbeats WHERE rowid IN
		  (SELECT rowid FROM heartbeats WHERE ts_ms < ? LIMIT %d)`, deleteBatchSize)
	}
	start := time.Now()
	for {
		t := time.Now()
		tx, err := w.BeginTx(ctx, nil)
		if err != nil {
			return st, fmt.Errorf("begin: %w", err)
		}
		res, err := tx.ExecContext(ctx, delSQL, cutoff)
		if err != nil {
			_ = tx.Rollback()
			return st, fmt.Errorf("delete batch %d: %w", st.batches, err)
		}
		if err := tx.Commit(); err != nil {
			return st, fmt.Errorf("commit batch %d: %w", st.batches, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return st, fmt.Errorf("rows affected: %w", err)
		}
		if n == 0 {
			break
		}
		st.deleted += n
		st.batches++
		st.batchDur = append(st.batchDur, time.Since(t))
	}
	st.sec = time.Since(start).Seconds()
	return st, nil
}

func checkpointTruncate(ctx context.Context, w *sql.DB) error {
	var busy, logFrames, ckpt int64
	if err := w.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckpt); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal_checkpoint(TRUNCATE) returned busy=%d", busy)
	}
	return nil
}

func fileMB(path string) float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(fi.Size()) / (1 << 20)
}

func pragmaInt(ctx context.Context, w *sql.DB, name string) int64 {
	var v int64
	_ = w.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v)
	return v
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func maxDur(d []time.Duration) time.Duration {
	var m time.Duration
	for _, x := range d {
		if x > m {
			m = x
		}
	}
	return m
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "WARN: "+format+"\n", a...)
}

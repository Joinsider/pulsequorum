// Spike AP-0.15 (O-07): Benchmark modernc.org/sqlite vs. github.com/ncruces/go-sqlite3
// mit Heartbeat-Last nach ADR 0004. Wegwerf-Code unter spikes/, nicht fuer Produktion.
//
//	go run . -driver=modernc -variant=rowid -rows=1080000
//	go run . -driver=ncruces -variant=without-rowid -rows=6480000
//	go run . -summarize            # schreibt RESULTS.md aus results/*.json
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver" // Register the ncruces sqlite3 driver with database/sql.
	_ "modernc.org/sqlite"                   // Register the modernc sqlite driver with database/sql.
)

type result struct {
	Driver   string            `json:"driver"`
	Variant  string            `json:"variant"`
	Rows     int64             `json:"rows"`
	SimHours float64           `json:"sim_hours"`
	Pragmas  map[string]string `json:"pragmas"`
	Verified int               `json:"reader_conns_verified"`
	ThinkMs  int64             `json:"reader_think_ms"`

	LoadSec        float64 `json:"load_sec"`
	LoadRowsPerSec float64 `json:"load_rows_per_sec"`
	LoadTxP50Ms    float64 `json:"load_tx_p50_ms"`
	LoadTxP95Ms    float64 `json:"load_tx_p95_ms"`
	LoadTxMaxMs    float64 `json:"load_tx_max_ms"`

	DBMBAfterLoad    float64 `json:"db_mb_after_load"`
	MBPerMillionRows float64 `json:"mb_per_million_rows"`

	ConcSec              float64 `json:"conc_sec"`
	ConcWriterRows       int64   `json:"conc_writer_rows"`
	ConcWriterRowsPerSec float64 `json:"conc_writer_rows_per_sec"`
	ConcWriterTxP95Ms    float64 `json:"conc_writer_tx_p95_ms"`
	ConcReaderQueries    int     `json:"conc_reader_queries"`
	ConcReaderP50Ms      float64 `json:"conc_reader_p50_ms"`
	ConcReaderP95Ms      float64 `json:"conc_reader_p95_ms"`
	ConcReaderMaxMs      float64 `json:"conc_reader_max_ms"`
	ConcBusyErrors       int64   `json:"conc_busy_errors"`
	ConcOtherErrors      int64   `json:"conc_other_errors"`
	ConcFirstOtherError  string  `json:"conc_first_other_error,omitempty"`
	ConcMaxWALMB         float64 `json:"conc_max_wal_mb"`
	CkptAfterConcSec     float64 `json:"ckpt_after_conc_sec"`

	RetRows          int64   `json:"ret_rows"`
	RetBatches       int     `json:"ret_batches"`
	RetTotalSec      float64 `json:"ret_total_sec"`
	RetBatchP95Ms    float64 `json:"ret_batch_p95_ms"`
	RetBatchMaxMs    float64 `json:"ret_batch_max_ms"`
	WALMBBeforeCkpt  float64 `json:"wal_mb_before_checkpoint"`
	DBMBAfterDelete  float64 `json:"db_mb_after_delete"`
	FreelistBeforeVC int64   `json:"freelist_before_vacuum"`
	VacuumSec        float64 `json:"incremental_vacuum_sec"`
	DBMBAfterVacuum  float64 `json:"db_mb_after_vacuum"`
	FreelistAfterVC  int64   `json:"freelist_after_vacuum"`
	WALMBAfterVacuum float64 `json:"wal_mb_after_vacuum"`
}

func main() {
	driver := flag.String("driver", "modernc", "modernc | ncruces")
	variant := flag.String("variant", "rowid", "rowid | without-rowid")
	rows := flag.Int64("rows", 1_080_000, "Zeilen laden (aufgerundet auf volle 20-s-Intervalle a 3000 Zeilen)")
	concSec := flag.Int("conc-seconds", 30, "Dauer der Last-Phase (1 Schreiber + 8 Leser)")
	thinkMs := flag.Int("think-ms", 0, "Pause zwischen Leser-Queries in ms (0 = Leser laufen am Anschlag)")
	dir := flag.String("dir", "", "Verzeichnis fuer DB-Dateien (Default: Temp-Verzeichnis, danach geloescht)")
	keep := flag.Bool("keep", false, "DB-Dateien nach dem Lauf behalten")
	resultsDir := flag.String("results", "results", "Verzeichnis fuer JSON-Ergebnisse")
	summarize := flag.Bool("summarize", false, "nur RESULTS.md aus dem Ergebnisverzeichnis schreiben")
	resultsMD := flag.String("results-md", "RESULTS.md", "Ziel fuer -summarize")
	flag.Parse()

	if *summarize {
		if err := summarizeResults(*resultsDir, *resultsMD); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", *resultsMD)
		return
	}
	if err := run(runOptions{
		driver:     *driver,
		variant:    *variant,
		rows:       *rows,
		concDur:    time.Duration(*concSec) * time.Second,
		think:      time.Duration(*thinkMs) * time.Millisecond,
		dir:        *dir,
		keep:       *keep,
		resultsDir: *resultsDir,
	}); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

type runOptions struct {
	driver     string
	variant    string
	rows       int64
	concDur    time.Duration
	think      time.Duration
	dir        string
	keep       bool
	resultsDir string
}

func run(opts runOptions) error {
	driver, variant, rows, concDur, think, dir, keep, resultsDir := opts.driver, opts.variant, opts.rows, opts.concDur, opts.think, opts.dir, opts.keep, opts.resultsDir
	ctx := context.Background()
	cfg, err := configFor(driver)
	if err != nil {
		return err
	}
	if variant != "rowid" && variant != "without-rowid" {
		return fmt.Errorf("unknown variant %q", variant)
	}

	intervals := (rows + rowsPerInterval - 1) / rowsPerInterval
	loadRows := intervals * rowsPerInterval
	simHours := float64(intervals*intervalMs) / 3600_000

	base := dir
	if base == "" {
		base, err = os.MkdirTemp("", "sqlite-spike-*")
		if err != nil {
			return err
		}
	} else if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	if !keep && dir == "" {
		defer os.RemoveAll(base)
	}
	dbPath := filepath.Join(base, fmt.Sprintf("hb-%s-%s.db", driver, variant))
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	fmt.Printf("== driver=%s variant=%s rows=%d (%d Intervalle, %.2f h simuliert) db=%s\n",
		driver, variant, loadRows, intervals, simHours, dbPath)

	// ---- Setup: Schreib-Handle (1 Verbindung) ----
	w, err := sql.Open(cfg.sqlName, cfg.writerDSN(dbPath))
	if err != nil {
		return err
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := createSchema(ctx, w, variant); err != nil {
		return err
	}

	res := result{Driver: driver, Variant: variant, Rows: loadRows, SimHours: simHours}

	wc, err := w.Conn(ctx)
	if err != nil {
		return err
	}
	got, err := readPragmas(ctx, wc)
	_ = wc.Close()
	if err != nil {
		return err
	}
	if err := checkPragmas(got, false); err != nil {
		return fmt.Errorf("writer: %w", err)
	}
	res.Pragmas = got
	fmt.Printf("writer pragmas OK: %v\n", got)

	// ---- Setup: Lese-Pool (max 8), mode=ro ----
	r, err := sql.Open(cfg.sqlName, cfg.readerDSN(dbPath))
	if err != nil {
		return err
	}
	defer r.Close()
	r.SetMaxOpenConns(numReaders)
	r.SetMaxIdleConns(numReaders)

	// Alle 8 Leser-Verbindungen gleichzeitig halten, damit jede einzeln geprueft wird.
	conns := make([]*sql.Conn, 0, numReaders)
	for i := 0; i < numReaders; i++ {
		c, err := r.Conn(ctx)
		if err != nil {
			return fmt.Errorf("reader conn %d: %w", i, err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		rp, err := readPragmas(ctx, c)
		if err != nil {
			return err
		}
		if err := checkPragmas(rp, true); err != nil {
			return fmt.Errorf("reader conn %d: %w", i, err)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
	res.Verified = len(conns)
	fmt.Printf("reader pragmas OK on %d/%d conns\n", res.Verified, numReaders)

	// ---- 1. Batch-Insert ----
	g := newGenerator(42)
	ls, err := loadPhase(ctx, w, g, intervals)
	if err != nil {
		return err
	}
	res.LoadSec = ls.sec
	res.LoadRowsPerSec = float64(ls.rows) / ls.sec
	res.LoadTxP50Ms = ms(pct(ls.txDur, 0.50))
	res.LoadTxP95Ms = ms(pct(ls.txDur, 0.95))
	res.LoadTxMaxMs = ms(maxDur(ls.txDur))
	fmt.Printf("load: %d rows in %.1f s = %.0f rows/s\n", ls.rows, ls.sec, res.LoadRowsPerSec)

	// ---- 4. DB-Groesse nach Load (nach Checkpoint) ----
	if err := checkpointTruncate(ctx, w); err != nil {
		return err
	}
	dbPathMB := fileMB(dbPath)
	res.DBMBAfterLoad = dbPathMB
	res.MBPerMillionRows = dbPathMB / (float64(loadRows) / 1e6)
	fmt.Printf("db after load: %.1f MB (%.1f MB per 1 Mio. Zeilen)\n", dbPathMB, res.MBPerMillionRows)

	// ---- 2. Gleichzeitige Last ----
	monitors := g.monitors[:]
	var lastTs atomic.Int64
	lastTs.Store(epochMs + intervals*intervalMs)
	cs := concurrentPhase(ctx, concParams{
		w:        w,
		r:        r,
		g:        g,
		monitors: monitors,
		dur:      concDur,
		think:    think,
		lastTs:   &lastTs,
		dbPath:   dbPath,
	})
	res.ConcMaxWALMB = cs.maxWALMB
	res.ThinkMs = think.Milliseconds()
	res.ConcSec = cs.sec
	res.ConcWriterRows = cs.writerRows
	res.ConcWriterRowsPerSec = float64(cs.writerRows) / cs.sec
	res.ConcWriterTxP95Ms = ms(pct(cs.writerTx, 0.95))
	res.ConcReaderQueries = len(cs.readerDur)
	res.ConcReaderP50Ms = ms(pct(cs.readerDur, 0.50))
	res.ConcReaderP95Ms = ms(pct(cs.readerDur, 0.95))
	res.ConcReaderMaxMs = ms(maxDur(cs.readerDur))
	res.ConcBusyErrors = cs.busy
	res.ConcOtherErrors = cs.other
	res.ConcFirstOtherError = cs.firstOther
	fmt.Printf("concurrent %.0f s: writer %.0f rows/s, readers %d queries p50=%.2f ms p95=%.2f ms, busy=%d other=%d, max WAL %.1f MB\n",
		cs.sec, res.ConcWriterRowsPerSec, res.ConcReaderQueries, res.ConcReaderP50Ms, res.ConcReaderP95Ms,
		cs.busy, cs.other, cs.maxWALMB)

	// WAL nach der Lastphase zuruecksetzen, damit die Retention nicht fuer das
	// aufgelaufene WAL bezahlt (Dauer separat gemessen, nicht in Retention-Zeit).
	ckT := time.Now()
	if err := checkpointTruncate(ctx, w); err != nil {
		return err
	}
	res.CkptAfterConcSec = time.Since(ckT).Seconds()
	fmt.Printf("checkpoint(TRUNCATE) after concurrent phase: %.2f s\n", res.CkptAfterConcSec)

	// ---- 3. Retention: Hälfte der geladenen Daten ----
	cutoff := epochMs + (intervals/2)*intervalMs
	rs, err := retention(ctx, w, variant, cutoff)
	if err != nil {
		return err
	}
	if rs.deleted != rs.toDelete {
		warnf("retention deleted %d rows, expected %d", rs.deleted, rs.toDelete)
	}
	res.RetRows = rs.deleted
	res.RetBatches = rs.batches
	res.RetTotalSec = rs.sec
	res.RetBatchP95Ms = ms(pct(rs.batchDur, 0.95))
	res.RetBatchMaxMs = ms(maxDur(rs.batchDur))
	res.WALMBBeforeCkpt = fileMB(dbPath + "-wal")
	fmt.Printf("retention: %d rows in %d batches, total %.1f s, batch p95=%.1f ms max=%.1f ms, WAL before ckpt %.1f MB\n",
		rs.deleted, rs.batches, rs.sec, res.RetBatchP95Ms, res.RetBatchMaxMs, res.WALMBBeforeCkpt)

	if err := checkpointTruncate(ctx, w); err != nil {
		return err
	}
	res.DBMBAfterDelete = fileMB(dbPath)
	res.FreelistBeforeVC = pragmaInt(ctx, w, "freelist_count")

	vt := time.Now()
	if _, err := w.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return fmt.Errorf("incremental_vacuum: %w", err)
	}
	res.VacuumSec = time.Since(vt).Seconds()
	if err := checkpointTruncate(ctx, w); err != nil {
		return err
	}
	res.DBMBAfterVacuum = fileMB(dbPath)
	res.FreelistAfterVC = pragmaInt(ctx, w, "freelist_count")
	res.WALMBAfterVacuum = fileMB(dbPath + "-wal")
	fmt.Printf("after delete: db %.1f MB, freelist %d pages; incremental_vacuum %.2f s -> db %.1f MB, freelist %d, wal %.1f MB\n",
		res.DBMBAfterDelete, res.FreelistBeforeVC, res.VacuumSec, res.DBMBAfterVacuum, res.FreelistAfterVC, res.WALMBAfterVacuum)

	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(resultsDir, fmt.Sprintf("%s-%s-%d-think%d.json", driver, variant, loadRows, res.ThinkMs))
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", out)
	return nil
}

// ---------- Zusammenfassung ----------

func summarizeResults(resultsDir, target string) error {
	files, err := filepath.Glob(filepath.Join(resultsDir, "*.json"))
	if err != nil {
		return err
	}
	var all []result
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var r result
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Rows != all[j].Rows {
			return all[i].Rows < all[j].Rows
		}
		if all[i].Driver != all[j].Driver {
			return all[i].Driver < all[j].Driver
		}
		return all[i].Variant < all[j].Variant
	})
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer f.Close()
	p := func(format string, a ...any) { fmt.Fprintf(f, format, a...) }

	p("# Ergebnisse: SQLite-Spike (AP-0.15, O-07)\n\n")
	p("Generiert von `go run . -summarize` aus `results/*.json`. Setup, Maschine und Interpretation: siehe `README.md`.\n\n")
	p("## Insert, Lese-/Schreiblast\n\n")
	p("| Treiber | Tabelle | Zeilen (Std. simuliert) | Insert Zeilen/s | Tx p50 / p95 / max (ms) | Last: Schreiben Zeilen/s | Last: Schreib-Tx p95 (ms) | Last: Leser-Queries | Leser p50 / p95 / max (ms) | SQLITE_BUSY | Sonstige Fehler | Max. WAL unter Last (MB) |\n")
	p("|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range all {
		p("| %s | %s | %d (%.2f h) | %.0f | %.2f / %.2f / %.1f | %.0f | %.2f | %d | %.2f / %.2f / %.1f | %d | %d | %.1f |\n",
			r.Driver, variantLabel(r), r.Rows, r.SimHours, r.LoadRowsPerSec,
			r.LoadTxP50Ms, r.LoadTxP95Ms, r.LoadTxMaxMs,
			r.ConcWriterRowsPerSec, r.ConcWriterTxP95Ms, r.ConcReaderQueries,
			r.ConcReaderP50Ms, r.ConcReaderP95Ms, r.ConcReaderMaxMs,
			r.ConcBusyErrors, r.ConcOtherErrors, r.ConcMaxWALMB)
	}
	p("\n## Retention, Speichergroesse\n\n")
	p("| Treiber | Tabelle | Zeilen | Geloescht | Batches (5k) | Retention gesamt (s) | Batch p95 / max (ms) | DB nach Load (MB) | MB je 1 Mio. Zeilen | WAL vor Checkpoint (MB) | DB nach Delete (MB) | incremental_vacuum (s) | DB nach VACUUM (MB) | Freelist vor / nach (Seiten) | WAL nach VACUUM (MB) |\n")
	p("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range all {
		p("| %s | %s | %d | %d | %d | %.1f | %.1f / %.1f | %.1f | %.1f | %.1f | %.1f | %.2f | %.1f | %d / %d | %.1f |\n",
			r.Driver, variantLabel(r), r.Rows, r.RetRows, r.RetBatches, r.RetTotalSec,
			r.RetBatchP95Ms, r.RetBatchMaxMs, r.DBMBAfterLoad, r.MBPerMillionRows,
			r.WALMBBeforeCkpt, r.DBMBAfterDelete, r.VacuumSec, r.DBMBAfterVacuum,
			r.FreelistBeforeVC, r.FreelistAfterVC, r.WALMBAfterVacuum)
	}
	return nil
}

func variantLabel(r result) string {
	if r.ThinkMs > 0 {
		return fmt.Sprintf("%s (Leser-Pause %d ms)", r.Variant, r.ThinkMs)
	}
	return r.Variant
}

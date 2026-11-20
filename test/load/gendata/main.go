// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command gendata fills the data directory of a stopped voter with synthetic
// history for the monitors that already exist in its state.db. It supports the
// acceptance scenario AS-10 (docs/pflichtenheft.md): 30 days of data, then a
// retention run, then API measurements. It is a load test tool, not product code.
//
// The history is written as the voter's own heartbeat stream (node ID from
// DIR/node_id, incarnation of DIR/metrics.db). After every simulated day the
// real uptime job (internal/uptime) rolls up, compacts and applies retention
// with a fake clock, as it would run live. state.db is only read.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/node"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
	"github.com/joinsider/pulsequorum/internal/uptime"
)

const (
	// insertBatch is the number of heartbeats per transaction.
	insertBatch = 5000
	// seedStream selects the PCG stream, so that the seed alone gives a stable sequence.
	seedStream = 0x67656e64617461
)

type options struct {
	DataDir  string
	Days     int
	Tenant   string
	DownRate float64
	Seed     uint64
	// CPUProfile writes a CPU profile of the run if set.
	CPUProfile string
}

func (o options) validate() error {
	switch {
	case o.DataDir == "":
		return errors.New("-data is required")
	case o.Days < 1:
		return errors.New("-days must be at least 1")
	case o.Tenant == "":
		return errors.New("-tenant must not be empty")
	case o.DownRate < 0 || o.DownRate >= 1:
		return fmt.Errorf("-down-rate must be in [0, 1), got %v", o.DownRate)
	}
	return nil
}

// summary is the result of one run.
type summary struct {
	Monitors int
	Skipped  int
	Rows     int64
	Elapsed  time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.DataDir, "data", "", "data directory of a stopped voter (state.db, node_id; metrics.db is created if missing)")
	flag.IntVar(&opts.Days, "days", 30, "days of history to generate, ending now")
	flag.StringVar(&opts.Tenant, "tenant", "default", "tenant whose monitors get history")
	flag.Float64Var(&opts.DownRate, "down-rate", 0.002, "probability per check that a DOWN streak starts")
	flag.Uint64Var(&opts.Seed, "seed", 1, "seed of the random generator")
	flag.StringVar(&opts.CPUProfile, "cpuprofile", "", "write a CPU profile of the run to this file")
	flag.Parse()
	if opts.CPUProfile != "" {
		f, err := os.Create(opts.CPUProfile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gendata:", err)
			os.Exit(1)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintln(os.Stderr, "gendata:", err)
			os.Exit(1)
		}
		defer func() { pprof.StopCPUProfile(); _ = f.Close() }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	sum, err := run(ctx, opts, time.Now(), os.Stdout, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendata:", err)
		os.Exit(1)
	}
	fmt.Printf("done: %d monitors, %d heartbeats in %s\n", sum.Monitors, sum.Rows, sum.Elapsed.Round(time.Second))
}

// run generates opts.Days of history that ends at end.
func run(ctx context.Context, opts options, end time.Time, out io.Writer, log *slog.Logger) (summary, error) {
	if err := opts.validate(); err != nil {
		return summary{}, err
	}
	started := time.Now()
	nodeID, err := readNodeID(opts.DataDir)
	if err != nil {
		return summary{}, err
	}
	if _, err := os.Stat(filepath.Join(opts.DataDir, sqlite.StateFile)); err != nil {
		return summary{}, fmt.Errorf("state.db: %w (stop the voter and point -data at its data directory)", err)
	}

	state, err := sqlite.OpenState(ctx, opts.DataDir)
	if err != nil {
		return summary{}, err
	}
	defer func() { _ = state.Close() }()
	metrics, err := sqlite.OpenMetrics(ctx, opts.DataDir, sqlite.MetricsOptions{})
	if err != nil {
		return summary{}, err
	}
	defer func() { _ = metrics.Close() }()

	gens, sum, err := loadMonitors(ctx, state, opts.Tenant)
	if err != nil {
		return summary{}, err
	}
	if len(gens) == 0 {
		return summary{}, fmt.Errorf("tenant %s has no active monitors in %s", opts.Tenant, sqlite.StateFile)
	}
	last, err := metrics.LastSeq(ctx, nodeID, metrics.Incarnation())
	if err != nil {
		return summary{}, err
	}
	if last > 0 {
		return summary{}, fmt.Errorf("metrics.db already holds %d heartbeats of node %s; generate into a data directory without history (see README)", last, nodeID)
	}

	endMs := end.UnixMilli()
	startMs := endMs - int64(opts.Days)*store.BucketDay
	rng := rand.New(rand.NewPCG(opts.Seed, seedStream))
	for _, g := range gens {
		g.next = startMs + rng.Int64N(g.intervalMs) // first check at a random offset
	}

	// The job judges a monitor's history by its CreatedMs in state.db, which
	// is the real creation time. The backfilled history lies before it, so the
	// job would skip it. backfillState gives the job the start of the history
	// as creation time; state.db itself is not changed.
	var clock time.Time
	job := uptime.NewJob(uptime.JobOptions{
		State:      backfillState{State: state, createdMs: startMs},
		Metrics:    metrics,
		Logger:     log,
		Compaction: true,
		Retention:  uptime.DefaultRetention,
		Now:        func() time.Time { return clock },
		Vacuum:     func(ctx context.Context) error { return metrics.DB().IncrementalVacuum(ctx, 0) },
	})
	w := &heartbeatWriter{ctx: ctx, metrics: metrics, nodeID: nodeID, incarnation: metrics.Incarnation(), size: insertBatch}

	if _, err := fmt.Fprintf(out, "generating %d days for %d monitors of tenant %s (%d paused skipped), node %s, down-rate %g, seed %d\n",
		opts.Days, len(gens), opts.Tenant, sum.Skipped, nodeID, opts.DownRate, opts.Seed); err != nil {
		return summary{}, err
	}
	// Hourly steps, as the live job runs: the job rolls up at most 24 hours per
	// run, so a daily step would fall behind.
	hours := opts.Days * 24
	for h := range hours {
		if err := ctx.Err(); err != nil {
			return summary{}, err
		}
		from, to := windowBounds(startMs, h, store.BucketHour)
		if err := generateWindow(gens, from, to, rng, opts.DownRate, w.add); err != nil {
			return summary{}, fmt.Errorf("hour %d: %w", h+1, err)
		}
		if err := w.flush(); err != nil {
			return summary{}, fmt.Errorf("hour %d: %w", h+1, err)
		}
		clock = time.UnixMilli(to)
		if err := job.RunOnce(ctx); err != nil {
			return summary{}, fmt.Errorf("uptime job at hour %d: %w", h+1, err)
		}
		if (h+1)%24 == 0 {
			if err := printProgress(out, (h+1)/24, opts.Days, w.rows, time.Since(started), opts.DataDir); err != nil {
				return summary{}, err
			}
		}
	}

	// Checkpoint so that the WAL files do not hold the bulk of the data.
	if _, err := metrics.DB().Checkpoint(ctx); err != nil {
		return summary{}, err
	}
	if _, err := state.DB().Checkpoint(ctx); err != nil {
		return summary{}, err
	}
	if err := printSizes(out, opts.DataDir); err != nil {
		return summary{}, err
	}

	sum.Rows = w.rows
	sum.Elapsed = time.Since(started)
	return sum, nil
}

// loadMonitors returns the generator state of the tenant's monitors, all pages.
// Paused monitors produce no checks and are counted in Skipped.
func loadMonitors(ctx context.Context, state store.State, tenant string) ([]*monitorGen, summary, error) {
	var gens []*monitorGen
	var sum summary
	q := store.MonitorQuery{Limit: 500}
	for {
		page, err := state.ListMonitors(ctx, tenant, q)
		if err != nil {
			return nil, summary{}, fmt.Errorf("list monitors of %s: %w", tenant, err)
		}
		for _, m := range page.Items {
			if m.Paused {
				sum.Skipped++
				continue
			}
			g, err := newMonitorGen(m)
			if err != nil {
				return nil, summary{}, err
			}
			gens = append(gens, g)
		}
		if page.NextAfterID == "" {
			break
		}
		q.AfterID = page.NextAfterID
	}
	sum.Monitors = len(gens)
	return gens, sum, nil
}

// readNodeID returns the node ID the voter stored in DIR/node_id.
func readNodeID(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, node.IDFile))
	if err != nil {
		return "", fmt.Errorf("read node ID: %w (the voter creates it on its first start)", err)
	}
	id := strings.TrimSpace(string(raw))
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("node ID in %s is invalid: %w", filepath.Join(dataDir, node.IDFile), err)
	}
	return id, nil
}

// backfillState is the store.State the uptime job sees: every monitor is
// reported as created no later than startMs. Everything else is passed through.
type backfillState struct {
	store.State
	createdMs int64
}

func (s backfillState) ListMonitors(ctx context.Context, tenantID string, q store.MonitorQuery) (store.MonitorPage, error) {
	page, err := s.State.ListMonitors(ctx, tenantID, q)
	if err != nil {
		return page, err
	}
	for i := range page.Items {
		page.Items[i].CreatedMs = min(page.Items[i].CreatedMs, s.createdMs)
	}
	return page, nil
}

func printProgress(out io.Writer, day, days int, rows int64, elapsed time.Duration, dataDir string) error {
	rate := float64(rows) / max(elapsed.Seconds(), 1e-9)
	_, err := fmt.Fprintf(out, "day %2d/%d  rows %12d  elapsed %8s  %8.0f rows/s  %s\n",
		day, days, rows, elapsed.Round(time.Second), rate, sizeLine(dataDir))
	return err
}

// sizeLine lists the sizes of state.db, metrics.db and their WAL files.
func sizeLine(dataDir string) string {
	var parts []string
	var total int64
	for _, name := range []string{sqlite.StateFile, sqlite.StateFile + "-wal", sqlite.MetricsFile, sqlite.MetricsFile + "-wal"} {
		n := fileSize(filepath.Join(dataDir, name))
		total += n
		parts = append(parts, fmt.Sprintf("%s %s", name, humanBytes(n)))
	}
	return strings.Join(parts, "  ") + "  total " + humanBytes(total)
}

func printSizes(out io.Writer, dataDir string) error {
	if _, err := fmt.Fprintln(out, "final sizes:"); err != nil {
		return err
	}
	for _, name := range []string{sqlite.StateFile, sqlite.StateFile + "-wal", sqlite.MetricsFile, sqlite.MetricsFile + "-wal"} {
		if _, err := fmt.Fprintf(out, "  %-18s %12d bytes  %10s\n", name, fileSize(filepath.Join(dataDir, name)), humanBytes(fileSize(filepath.Join(dataDir, name)))); err != nil {
			return err
		}
	}
	var total int64
	for _, name := range []string{sqlite.StateFile, sqlite.StateFile + "-wal", sqlite.MetricsFile, sqlite.MetricsFile + "-wal"} {
		total += fileSize(filepath.Join(dataDir, name))
	}
	_, err := fmt.Fprintf(out, "  %-18s %12d bytes  %10s\n", "total", total, humanBytes(total))
	return err
}

// fileSize returns the size of a file, 0 if it does not exist.
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

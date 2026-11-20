// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package scheduler runs the checks this node is responsible for (FA-SCH-*).
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// DefaultMaxConcurrent is the default limit of simultaneous checks (FA-SCH-07).
const DefaultMaxConcurrent = 256

// Outcome is one finished check.
type Outcome struct {
	Monitor store.Monitor
	// Scheduled is the planned start; Started may be later if the
	// concurrency limit delayed the check.
	Scheduled time.Time
	Started   time.Time
	Result    checker.Result
}

// Sink receives outcomes. Record must not block for long; it is called from
// the check goroutines.
type Sink interface {
	Record(ctx context.Context, o Outcome)
}

// Sinks passes every outcome to each sink in order.
type Sinks []Sink

// Record implements Sink.
func (s Sinks) Record(ctx context.Context, o Outcome) {
	for _, sink := range s {
		sink.Record(ctx, o)
	}
}

// CheckObserver is told when a check starts and when it finishes. It is
// optional; metrics implement it. Calls come from the check goroutines and
// must not block for long. The result is the lower-case status (up, down, degraded).
type CheckObserver interface {
	CheckStarted(typ string)
	CheckFinished(typ, result string, d time.Duration)
}

// Options configure a Scheduler.
type Options struct {
	Registry      *checker.Registry
	Env           *checker.Env
	Sink          Sink
	Logger        *slog.Logger
	MaxConcurrent int
	// Observer receives check start and finish events; nil disables them.
	Observer CheckObserver
	// Unit is the length of one interval second; tests shrink it.
	Unit time.Duration
	// Resolve returns the configuration with its secrets decrypted (ADR 0015);
	// nil uses the stored configuration as it is.
	Resolve func(ctx context.Context, m store.Monitor) (json.RawMessage, error)
	// Now is the wall clock for the schedule and the start time of a check
	// (heartbeat timestamps); nil means time.Now. Durations stay monotonic (ADR 0020).
	Now func() time.Time
}

// Stats are counters for metrics and tests.
type Stats struct {
	Started int64
	// Skipped counts runs dropped because the previous run of the same
	// monitor was still in progress (FA-SCH-04).
	Skipped int64
	Running int64
}

// Scheduler runs one timer loop per monitor.
type Scheduler struct {
	opts Options
	sem  chan struct{}

	mu      sync.Mutex
	entries map[string]*entry
	ctx     context.Context
	wg      sync.WaitGroup

	started, skipped, running atomic.Int64
}

type entry struct {
	monitor store.Monitor
	cancel  context.CancelFunc
	busy    atomic.Bool
	done    chan struct{}
}

// New creates a scheduler; call Run to start it.
func New(opts Options) *Scheduler {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.Unit <= 0 {
		opts.Unit = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Scheduler{opts: opts, sem: make(chan struct{}, opts.MaxConcurrent), entries: map[string]*entry{}}
}

// Run starts the scheduler and blocks until ctx ends. It then stops all
// timers and waits for running checks, whose contexts are cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	for _, e := range s.entries {
		s.startLocked(e)
	}
	s.mu.Unlock()

	<-ctx.Done()

	s.mu.Lock()
	for _, e := range s.entries {
		e.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Sync sets the monitors to run. New monitors start, removed or paused ones
// stop, and monitors whose revision changed restart with the new
// configuration (FA-SCH-06).
func (s *Scheduler) Sync(monitors []store.Monitor) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wanted := make(map[string]store.Monitor, len(monitors))
	for _, m := range monitors {
		if !m.Paused && m.IntervalSeconds > 0 {
			wanted[m.ID] = m
		}
	}
	for id, e := range s.entries {
		if m, ok := wanted[id]; !ok || m.Revision != e.monitor.Revision {
			if e.cancel != nil {
				e.cancel()
			}
			delete(s.entries, id)
		}
	}
	for id, m := range wanted {
		if _, ok := s.entries[id]; ok {
			continue
		}
		e := &entry{monitor: m}
		s.entries[id] = e
		if s.ctx != nil {
			s.startLocked(e)
		}
	}
}

// Stats returns the current counters.
func (s *Scheduler) Stats() Stats {
	return Stats{Started: s.started.Load(), Skipped: s.skipped.Load(), Running: s.running.Load()}
}

// Len returns the number of scheduled monitors.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Scheduler) startLocked(e *entry) {
	ctx, cancel := context.WithCancel(s.ctx)
	e.cancel = cancel
	e.done = make(chan struct{})
	s.wg.Add(1)
	go s.loop(ctx, e)
}

// loop fires at phase + k*interval in wall-clock time. The phase is derived
// from the monitor ID, so first runs are spread over the interval instead of
// all starting at once (FA-SCH-03, PF-15), and every node checks a monitor at
// the same moment, which keeps consensus votes comparable.
func (s *Scheduler) loop(ctx context.Context, e *entry) {
	defer s.wg.Done()
	defer close(e.done)

	interval := time.Duration(e.monitor.IntervalSeconds) * s.opts.Unit
	phase := Phase(e.monitor.ID, interval)
	next := NextRun(s.opts.Now(), interval, phase)
	timer := time.NewTimer(next.Sub(s.opts.Now()))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		scheduled := next
		if e.busy.CompareAndSwap(false, true) {
			s.wg.Add(1)
			go s.run(ctx, e, scheduled)
		} else {
			s.skipped.Add(1)
			s.opts.Logger.Debug("check still running, skipping this run", "monitor", e.monitor.ID)
		}
		// Plan from the schedule, not from "now", so latency does not drift;
		// after a long pause (e.g. suspend) jump to the next slot.
		next = next.Add(interval)
		if now := s.opts.Now(); !next.After(now) {
			next = NextRun(now, interval, phase)
		}
		timer.Reset(next.Sub(s.opts.Now()))
	}
}

func (s *Scheduler) run(ctx context.Context, e *entry, scheduled time.Time) {
	defer s.wg.Done()
	defer e.busy.Store(false)

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.sem }()

	s.started.Add(1)
	s.running.Add(1)
	defer s.running.Add(-1)

	m := e.monitor
	timeout := time.Duration(m.TimeoutSeconds) * s.opts.Unit
	if timeout <= 0 {
		timeout = time.Duration(m.IntervalSeconds) * s.opts.Unit * 8 / 10
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := s.opts.Now()
	began := time.Now() // monotonic clock for the duration metric (ADR 0020)
	obs := s.opts.Observer
	var typ string
	if obs != nil {
		typ = s.observedType(m.Type)
		obs.CheckStarted(typ)
	}
	res := s.execute(checkCtx, m)
	if obs != nil {
		obs.CheckFinished(typ, strings.ToLower(res.Status.String()), time.Since(began))
	}
	if ctx.Err() != nil {
		return // monitor removed or node shutting down; the result is meaningless
	}
	s.opts.Sink.Record(ctx, Outcome{Monitor: m, Scheduled: scheduled, Started: started, Result: res})
}

// observedType keeps metric labels bounded: unregistered monitor types are
// reported as "unknown" instead of their stored string.
func (s *Scheduler) observedType(typ string) string {
	if _, err := s.opts.Registry.Lookup(typ); err != nil {
		return "unknown"
	}
	return typ
}

func (s *Scheduler) execute(ctx context.Context, m store.Monitor) (res checker.Result) {
	defer func() {
		if r := recover(); r != nil {
			s.opts.Logger.Error("checker panicked", "monitor", m.ID, "type", m.Type, "panic", r, "stack", string(debug.Stack()))
			res = checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassInternal, Message: fmt.Sprintf("checker panicked: %v", r)}
		}
	}()
	c, err := s.opts.Registry.Lookup(m.Type)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassInternal, Message: err.Error()}
	}
	cfg := m.Config
	if s.opts.Resolve != nil {
		if cfg, err = s.opts.Resolve(ctx, m); err != nil {
			// The error names pointers and keys, never secret values.
			s.opts.Logger.Error("cannot decrypt monitor secrets", "monitor", m.ID, "err", err)
			return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassInternal,
				Message: "the stored credentials of this monitor cannot be decrypted"}
		}
	}
	return c.Check(ctx, s.opts.Env, cfg)
}

// Phase returns the deterministic offset of a monitor within its interval.
func Phase(monitorID string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(monitorID))
	return time.Duration(h.Sum64() % uint64(interval)) //nolint:gosec // G115: interval is positive
}

// NextRun returns the first time strictly after now that lies on the grid
// phase + k*interval (Unix epoch based).
func NextRun(now time.Time, interval, phase time.Duration) time.Time {
	if interval <= 0 {
		return now
	}
	since := time.Duration(now.UnixNano()) - phase
	k := since / interval
	next := time.Unix(0, int64(k*interval+phase))
	for !next.After(now) {
		next = next.Add(interval)
	}
	return next
}

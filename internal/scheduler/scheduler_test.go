// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fakeChecker sleeps for the configured number of milliseconds and tracks
// concurrency.
type fakeChecker struct {
	running, maxRunning atomic.Int64
	perMonitor          sync.Map // monitor config -> *atomic.Int64 running
	overlap             atomic.Bool
}

func (*fakeChecker) Type() string                  { return "fake" }
func (*fakeChecker) Name() string                  { return "Fake" }
func (*fakeChecker) ConfigSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*fakeChecker) Validate(json.RawMessage) error {
	return nil
}

func (f *fakeChecker) Check(ctx context.Context, _ *checker.Env, cfg json.RawMessage) checker.Result {
	var c struct {
		SleepMs int    `json:"sleepMs"`
		Key     string `json:"key"`
		Panic   bool   `json:"panic"`
	}
	_ = json.Unmarshal(cfg, &c)
	if c.Panic {
		panic("boom")
	}
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		m := f.maxRunning.Load()
		if n <= m || f.maxRunning.CompareAndSwap(m, n) {
			break
		}
	}
	v, _ := f.perMonitor.LoadOrStore(c.Key, new(atomic.Int64))
	if v.(*atomic.Int64).Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer v.(*atomic.Int64).Add(-1)

	select {
	case <-time.After(time.Duration(c.SleepMs) * time.Millisecond):
		return checker.Result{Status: checker.StatusUp}
	case <-ctx.Done():
		return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassTimeout}
	}
}

type recorder struct {
	mu       sync.Mutex
	outcomes []Outcome
}

func (r *recorder) Record(_ context.Context, o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, o)
}

func (r *recorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.outcomes)
}

func (r *recorder) count(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, o := range r.outcomes {
		if o.Monitor.ID == id {
			n++
		}
	}
	return n
}

func (r *recorder) last(id string) (Outcome, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.outcomes) - 1; i >= 0; i-- {
		if r.outcomes[i].Monitor.ID == id {
			return r.outcomes[i], true
		}
	}
	return Outcome{}, false
}

const unit = 10 * time.Millisecond

func newScheduler(t *testing.T, maxConcurrent int) (*Scheduler, *fakeChecker, *recorder) {
	t.Helper()
	reg := checker.NewRegistry()
	fake := &fakeChecker{}
	reg.Register(fake)
	rec := &recorder{}
	s := New(Options{Registry: reg, Env: &checker.Env{}, Sink: rec, MaxConcurrent: maxConcurrent, Unit: unit,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return s, fake, rec
}

func monitor(id string, interval, timeout int, cfg string, rev uint64) store.Monitor {
	return store.Monitor{ID: id, Type: "fake", IntervalSeconds: interval, TimeoutSeconds: timeout, Config: json.RawMessage(cfg), Revision: rev}
}

func start(t *testing.T, s *Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler did not stop")
		}
	}
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunsChecksPeriodically(t *testing.T) {
	s, _, rec := newScheduler(t, 0)
	s.Sync([]store.Monitor{monitor("m1", 5, 4, `{"key":"m1"}`, 1)})
	stop := start(t, s)
	eventually(t, func() bool { return rec.count("m1") >= 3 }, "monitor did not run three times")
	stop()

	o, _ := rec.last("m1")
	if o.Result.Status != checker.StatusUp {
		t.Errorf("result = %+v", o.Result)
	}
	// Runs are on the monitor's grid (FA-SCH-05).
	interval := 5 * unit
	phase := Phase("m1", interval)
	if off := (time.Duration(o.Scheduled.UnixNano()) - phase) % interval; off != 0 {
		t.Errorf("scheduled time %v is off the grid by %v", o.Scheduled, off)
	}
}

func TestNoSelfOverlap(t *testing.T) {
	s, fake, _ := newScheduler(t, 0)
	// Runs every 2 units but takes ~5 units: overlapping runs must be skipped.
	s.Sync([]store.Monitor{monitor("slow", 2, 100, `{"key":"slow","sleepMs":50}`, 1)})
	stop := start(t, s)
	eventually(t, func() bool { return s.Stats().Skipped >= 3 }, "no runs were skipped")
	stop()
	if fake.overlap.Load() {
		t.Error("a monitor ran in parallel with itself")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	s, fake, rec := newScheduler(t, 3)
	var ms []store.Monitor
	for i := range 12 {
		id := fmt.Sprintf("m%d", i)
		ms = append(ms, monitor(id, 4, 100, fmt.Sprintf(`{"key":%q,"sleepMs":30}`, id), 1))
	}
	s.Sync(ms)
	stop := start(t, s)
	eventually(t, func() bool { return rec.total() >= 12 }, "not enough checks ran")
	stop()
	if m := fake.maxRunning.Load(); m > 3 {
		t.Errorf("max concurrent checks = %d, want <= 3", m)
	}
}

func TestTimeoutCancelsCheck(t *testing.T) {
	s, _, rec := newScheduler(t, 0)
	s.Sync([]store.Monitor{monitor("t", 20, 2, `{"key":"t","sleepMs":5000}`, 1)})
	stop := start(t, s)
	eventually(t, func() bool { return rec.count("t") >= 1 }, "timed out check not recorded")
	stop()
	if o, _ := rec.last("t"); o.Result.ErrorClass != checker.ClassTimeout {
		t.Errorf("result = %+v, want timeout", o.Result)
	}
}

func TestSyncChangesMonitors(t *testing.T) {
	s, _, rec := newScheduler(t, 0)
	stop := start(t, s)
	defer stop()

	s.Sync([]store.Monitor{monitor("a", 3, 2, `{"key":"a"}`, 1), monitor("b", 3, 2, `{"key":"b"}`, 1)})
	eventually(t, func() bool { return rec.count("a") > 0 && rec.count("b") > 0 }, "monitors did not start")

	// Revision change restarts with the new configuration; b is removed; c is paused.
	paused := monitor("c", 3, 2, `{"key":"c"}`, 1)
	paused.Paused = true
	s.Sync([]store.Monitor{monitor("a", 3, 2, `{"key":"a2"}`, 2), paused})
	if s.Len() != 1 {
		t.Fatalf("scheduled monitors = %d, want 1", s.Len())
	}
	eventually(t, func() bool {
		o, ok := rec.last("a")
		return ok && o.Monitor.Revision == 2
	}, "new revision not used")
	bCount := rec.count("b")
	time.Sleep(10 * unit)
	if rec.count("b") != bCount || rec.count("c") != 0 {
		t.Errorf("removed or paused monitor still runs: b %d→%d, c %d", bCount, rec.count("b"), rec.count("c"))
	}
}

func TestPanicsAndUnknownTypes(t *testing.T) {
	s, _, rec := newScheduler(t, 0)
	unknown := monitor("u", 3, 2, `{}`, 1)
	unknown.Type = "nope"
	s.Sync([]store.Monitor{monitor("p", 3, 2, `{"panic":true}`, 1), unknown})
	stop := start(t, s)
	eventually(t, func() bool { return rec.count("p") > 0 && rec.count("u") > 0 }, "outcomes missing")
	stop()
	for _, id := range []string{"p", "u"} {
		if o, _ := rec.last(id); o.Result.Status != checker.StatusDown || o.Result.ErrorClass != checker.ClassInternal {
			t.Errorf("%s = %+v, want DOWN/internal", id, o.Result)
		}
	}
}

// TestPhaseSpread covers the FA-SCH-03 acceptance criterion: 1,000 monitors
// at 60 s produce at most three times the average load in any second.
func TestPhaseSpread(t *testing.T) {
	const monitors, interval = 1000, 60
	buckets := make([]int, interval)
	for i := range monitors {
		p := Phase(fmt.Sprintf("0192d5e0-%04x-7000-8000-%012x", i, i*7919), interval*time.Second)
		buckets[int(p/time.Second)]++
	}
	avg := float64(monitors) / interval
	for sec, n := range buckets {
		if float64(n) > 3*avg {
			t.Errorf("second %d has %d checks, average %.1f", sec, n, avg)
		}
	}
	first, second := Phase("x", time.Minute), Phase("x", time.Minute)
	if first != second {
		t.Error("phase is not deterministic")
	}
}

func TestNextRun(t *testing.T) {
	base := time.Unix(960_000, 0) // 16,000 × 60 s
	tests := []struct {
		now   time.Time
		phase time.Duration
		want  time.Time
	}{
		{base, 0, base.Add(60 * time.Second)},
		{base, 5 * time.Second, base.Add(5 * time.Second)},
		{base.Add(5 * time.Second), 5 * time.Second, base.Add(65 * time.Second)},
		{base.Add(59 * time.Second), 0, base.Add(60 * time.Second)},
	}
	for _, tt := range tests {
		if got := NextRun(tt.now, time.Minute, tt.phase); !got.Equal(tt.want) {
			t.Errorf("NextRun(%v, phase %v) = %v, want %v", tt.now.Unix(), tt.phase, got.Unix(), tt.want.Unix())
		}
	}
}

// ADR 0020: the start time of a check, which becomes the heartbeat timestamp,
// comes from the clock of the node; the schedule follows the same clock.
func TestClockStampsOutcomes(t *testing.T) {
	const skew = 90 * time.Second
	reg := checker.NewRegistry()
	reg.Register(&fakeChecker{})
	rec := &recorder{}
	s := New(Options{Registry: reg, Env: &checker.Env{}, Sink: rec, Unit: unit,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Now().Add(skew) }})
	s.Sync([]store.Monitor{monitor("m1", 5, 4, `{"key":"m1"}`, 1)})
	stop := start(t, s)
	eventually(t, func() bool { return rec.count("m1") >= 2 }, "monitor did not run twice")
	stop()

	o, _ := rec.last("m1")
	if d := o.Started.Sub(time.Now().Add(skew)); d < -time.Second || d > time.Second {
		t.Errorf("Started = %v, want the node clock (off by %v)", o.Started, d)
	}
}

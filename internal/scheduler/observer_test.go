// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package scheduler

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// fakeObserver records the observer calls and checks that every start has a finish.
type fakeObserver struct {
	mu       sync.Mutex
	started  []string
	finished []finishCall
	running  int
	maxRun   int
}

type finishCall struct {
	typ, result string
	d           time.Duration
}

func (f *fakeObserver) CheckStarted(typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, typ)
	f.running++
	f.maxRun = max(f.maxRun, f.running)
}

func (f *fakeObserver) CheckFinished(typ, result string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, finishCall{typ, result, d})
	f.running--
}

func (f *fakeObserver) snapshot() (started int, finished []finishCall, running int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), append([]finishCall(nil), f.finished...), f.running
}

func TestObserverSeesStartAndFinish(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&fakeChecker{})
	rec := &recorder{}
	obs := &fakeObserver{}
	s := New(Options{Registry: reg, Env: &checker.Env{}, Sink: rec, Unit: unit, Observer: obs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	s.Sync([]store.Monitor{
		monitor("ok", 5, 4, `{"key":"ok"}`, 1),
		{ID: "unknown", Type: "nope", IntervalSeconds: 5, TimeoutSeconds: 4, Config: []byte(`{}`), Revision: 1},
	})
	stop := start(t, s)
	eventually(t, func() bool {
		n, f, _ := obs.snapshot()
		return n >= 3 && len(f) >= 3
	}, "observer did not see three checks")
	stop()

	started, finished, running := obs.snapshot()
	if running != 0 {
		t.Errorf("running = %d after stop, want 0 (every start needs a finish)", running)
	}
	if started != len(finished) {
		t.Errorf("started = %d, finished = %d", started, len(finished))
	}
	var sawUp, sawDown bool
	for _, c := range finished {
		if c.d < 0 {
			t.Errorf("duration %v is negative", c.d)
		}
		switch {
		case c.typ == "fake" && c.result == "up":
			sawUp = true
		case c.typ == "unknown" && c.result == "down":
			sawDown = true
		default:
			t.Errorf("unexpected finish %+v", c)
		}
	}
	if !sawUp || !sawDown {
		t.Errorf("sawUp = %v, sawDown = %v; want both", sawUp, sawDown)
	}
}

func TestNilObserverIsFine(t *testing.T) {
	s, _, rec := newScheduler(t, 0) // Observer unset
	s.Sync([]store.Monitor{monitor("m1", 5, 4, `{"key":"m1"}`, 1)})
	stop := start(t, s)
	eventually(t, func() bool { return rec.count("m1") >= 1 }, "monitor did not run")
	stop()
}

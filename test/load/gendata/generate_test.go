// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"math/rand/v2"
	"slices"
	"sort"
	"testing"

	"github.com/joinsider/pulsequorum/internal/store"
)

func TestLatencyIsDeterministicAndPlausible(t *testing.T) {
	draw := func(seed uint64) []int {
		rng := rand.New(rand.NewPCG(seed, seedStream))
		out := make([]int, 20000)
		for i := range out {
			out[i] = latencyMs(rng, 10_000)
		}
		return out
	}
	a, b := draw(1), draw(1)
	if !slices.Equal(a, b) {
		t.Fatal("same seed gave different latencies")
	}
	if slices.Equal(a, draw(2)) {
		t.Fatal("different seeds gave the same latencies")
	}
	sorted := slices.Clone(a)
	sort.Ints(sorted)
	p50, p95 := sorted[len(sorted)/2], sorted[len(sorted)*95/100]
	if p50 < 35 || p50 > 55 {
		t.Errorf("median latency %d ms, want about 45 ms", p50)
	}
	if p95 < 60 || p95 > 100 {
		t.Errorf("p95 latency %d ms, want about 80 ms", p95)
	}
	if sorted[0] < 1 {
		t.Errorf("latency %d ms below 1 ms", sorted[0])
	}
}

func TestLatencyIsClampedToTimeout(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, seedStream))
	for range 10000 {
		if v := latencyMs(rng, 30); v < 1 || v > 29 {
			t.Fatalf("latency %d ms outside [1, 29] for a 30 ms timeout", v)
		}
	}
}

// outcomes runs n checks of one monitor and returns them.
func outcomes(seed uint64, n int, downRate float64) []outcome {
	rng := rand.New(rand.NewPCG(seed, seedStream))
	g := &monitorGen{monitorID: "m", intervalMs: 60_000, timeoutMs: 10_000}
	out := make([]outcome, n)
	for i := range out {
		out[i] = g.outcome(rng, downRate)
	}
	return out
}

func TestOutcomeDownStreaks(t *testing.T) {
	const n = 200_000
	for _, tc := range []struct {
		name     string
		downRate float64
		minFrac  float64
		maxFrac  float64
	}{
		{"no failures", 0, 0, 0},
		{"0.2 percent", 0.002, 0.001, 0.02},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := outcomes(3, n, tc.downRate)
			down := 0
			for _, o := range res {
				switch o.status {
				case store.CheckDown:
					down++
					if o.errorClass != errorClassConnect && o.errorClass != errorClassTimeout {
						t.Fatalf("DOWN check with error class %q", o.errorClass)
					}
					if o.message == "" {
						t.Fatal("DOWN check without message")
					}
					if o.errorClass == errorClassTimeout && o.latencyMs != 10_000 {
						t.Fatalf("timeout latency %d, want the timeout", o.latencyMs)
					}
				case store.CheckUp:
					if o.errorClass != "" || o.message != "" || o.latencyMs < 1 {
						t.Fatalf("UP check with error %q / message %q / latency %d", o.errorClass, o.message, o.latencyMs)
					}
				default:
					t.Fatalf("unexpected status %d", o.status)
				}
			}
			frac := float64(down) / n
			if frac < tc.minFrac || frac > tc.maxFrac {
				t.Errorf("DOWN fraction %.4f outside [%g, %g]", frac, tc.minFrac, tc.maxFrac)
			}
		})
	}
	if !slices.Equal(outcomes(5, 1000, 0.01), outcomes(5, 1000, 0.01)) {
		t.Error("outcomes are not deterministic for a seed")
	}
}

func TestWindowBoundsAreContiguous(t *testing.T) {
	const start = int64(1_700_000_000_000)
	const days = 30
	prevTo := start
	for d := range days * 24 {
		from, to := windowBounds(start, d, store.BucketHour)
		if from != prevTo {
			t.Fatalf("hour %d starts at %d, want %d", d, from, prevTo)
		}
		if to-from != store.BucketHour {
			t.Fatalf("hour %d has length %d", d, to-from)
		}
		prevTo = to
	}
	if prevTo-start != days*store.BucketDay {
		t.Fatalf("history spans %d ms", prevTo-start)
	}
}

// TestGenerateWindowIsChronologicalAndComplete checks two monitors with different
// intervals over two days, generated day by day.
func TestGenerateWindowIsChronologicalAndComplete(t *testing.T) {
	const start = int64(1_700_000_000_000)
	gens := []*monitorGen{
		{tenantID: "t", monitorID: "b", intervalMs: 90_000, timeoutMs: 10_000, next: start + 5_000},
		{tenantID: "t", monitorID: "a", intervalMs: 60_000, timeoutMs: 10_000, next: start},
	}
	rng := rand.New(rand.NewPCG(1, seedStream))
	perMonitor := map[string]int{}
	var last int64 = -1
	count := 0
	for day := range 2 {
		from, to := windowBounds(start, day, store.BucketDay)
		err := generateWindow(gens, from, to, rng, 0, func(g *monitorGen, ts int64, _ outcome) error {
			if ts < last {
				t.Fatalf("check at %d after %d: not chronological", ts, last)
			}
			if ts < from || ts >= to {
				t.Fatalf("check at %d outside day [%d, %d)", ts, from, to)
			}
			last = ts
			perMonitor[g.monitorID]++
			count++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	const span = 2 * store.BucketDay
	// Checks at start+offset+k*interval within the two days.
	if want := (span-5_000-1)/90_000 + 1; int64(perMonitor["b"]) != want {
		t.Errorf("monitor b: %d checks, want %d", perMonitor["b"], want)
	}
	if want := (span-1)/60_000 + 1; int64(perMonitor["a"]) != want {
		t.Errorf("monitor a: %d checks, want %d", perMonitor["a"], want)
	}
	if count != perMonitor["a"]+perMonitor["b"] {
		t.Errorf("count %d != sum of monitors", count)
	}
}

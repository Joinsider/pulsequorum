// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package ratelimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for tests. It is not safe for
// concurrent modification; the concurrency test uses a fixed clock.
type fakeClock struct {
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// tolerance covers float rounding in the retry-after computation.
const tolerance = time.Microsecond

func TestAllowBurstThenDeny(t *testing.T) {
	tests := []struct {
		name      string
		rate      float64
		burst     int
		wantRetry time.Duration
	}{
		{name: "rate 1 burst 3", rate: 1, burst: 3, wantRetry: time.Second},
		{name: "rate 2 burst 1", rate: 2, burst: 1, wantRetry: 500 * time.Millisecond},
		{name: "rate 0.5 burst 2", rate: 0.5, burst: 2, wantRetry: 2 * time.Second},
		{name: "rate 10 burst 5", rate: 10, burst: 5, wantRetry: 100 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			l := &Limiter{Rate: tt.rate, Burst: tt.burst, Limit: 10, Now: clk.Now}

			for i := 1; i <= tt.burst; i++ {
				ok, ra := l.Allow("k")
				if !ok || ra != 0 {
					t.Fatalf("request %d: got ok=%v retryAfter=%v, want ok=true retryAfter=0", i, ok, ra)
				}
			}

			ok, ra := l.Allow("k")
			if ok {
				t.Fatalf("request %d: got ok=true, want denied", tt.burst+1)
			}
			if ra <= 0 {
				t.Fatalf("retryAfter = %v, want > 0", ra)
			}
			if ra < tt.wantRetry || ra > tt.wantRetry+tolerance {
				t.Fatalf("retryAfter = %v, want about %v", ra, tt.wantRetry)
			}

			// The hint must be sufficient: waiting for it grants one request.
			clk.Advance(ra)
			if ok, _ := l.Allow("k"); !ok {
				t.Fatalf("request after waiting retryAfter=%v was denied", ra)
			}
		})
	}
}

func TestRefill(t *testing.T) {
	// Rate 2 tokens/s, burst 4. The bucket is drained first, then the clock
	// advances by wait and the number of allowed requests out of 10 is counted.
	tests := []struct {
		name string
		wait time.Duration
		want int
	}{
		{name: "no time passed", wait: 0, want: 0},
		{name: "half a token", wait: 250 * time.Millisecond, want: 0},
		{name: "one token", wait: 500 * time.Millisecond, want: 1},
		{name: "two tokens", wait: time.Second, want: 2},
		{name: "refill capped at burst after long pause", wait: time.Hour, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			l := &Limiter{Rate: 2, Burst: 4, Limit: 10, Now: clk.Now}
			for i := 0; i < 4; i++ {
				if ok, _ := l.Allow("k"); !ok {
					t.Fatalf("initial request %d denied", i+1)
				}
			}

			clk.Advance(tt.wait)

			allowed := 0
			for i := 0; i < 10; i++ {
				if ok, _ := l.Allow("k"); ok {
					allowed++
				}
			}
			if allowed != tt.want {
				t.Fatalf("allowed after %v = %d, want %d", tt.wait, allowed, tt.want)
			}
		})
	}
}

func TestKeysIndependent(t *testing.T) {
	clk := newFakeClock()
	l := &Limiter{Rate: 1, Burst: 2, Limit: 10, Now: clk.Now}

	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("a request %d denied", i+1)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a: third request allowed, want denied")
	}
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("b"); !ok {
			t.Fatalf("b request %d denied after exhausting a", i+1)
		}
	}
}

func TestZeroValueAndDefaults(t *testing.T) {
	tests := []struct {
		name  string
		build func() *Limiter
		check func(t *testing.T, l *Limiter)
	}{
		{
			name:  "New sets default limit",
			build: func() *Limiter { return New(1, 3) },
			check: func(t *testing.T, l *Limiter) {
				if l.Limit != DefaultLimit {
					t.Fatalf("Limit = %d, want %d", l.Limit, DefaultLimit)
				}
			},
		},
		{
			name:  "zero value is usable",
			build: func() *Limiter { return &Limiter{Rate: 1, Burst: 1} },
			check: func(t *testing.T, l *Limiter) {
				if ok, _ := l.Allow("k"); !ok {
					t.Fatal("first request on zero-value limiter denied")
				}
				if ok, _ := l.Allow("k"); ok {
					t.Fatal("second request allowed, want denied")
				}
				if got := len(l.buckets); got != 1 {
					t.Fatalf("len(buckets) = %d, want 1", got)
				}
			},
		},
		{
			name:  "burst below 1 is treated as 1",
			build: func() *Limiter { return &Limiter{Rate: 1, Burst: 0, Limit: 5} },
			check: func(t *testing.T, l *Limiter) {
				if ok, _ := l.Allow("k"); !ok {
					t.Fatal("first request denied")
				}
				if ok, _ := l.Allow("k"); ok {
					t.Fatal("second request allowed, want denied")
				}
			},
		},
		{
			name:  "non-positive limit falls back to default",
			build: func() *Limiter { return &Limiter{Rate: 1, Burst: 1, Limit: 0} },
			check: func(t *testing.T, l *Limiter) {
				if got := l.limit(); got != DefaultLimit {
					t.Fatalf("limit() = %d, want %d", got, DefaultLimit)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, tt.build())
		})
	}
}

func TestEvictionKeepsLimit(t *testing.T) {
	const limit = 10
	clk := newFakeClock()
	l := &Limiter{Rate: 1, Burst: 1, Limit: limit, Now: clk.Now}

	for i := 0; i < 1000; i++ {
		l.Allow(fmt.Sprintf("key-%d", i))
		if got := len(l.buckets); got > limit {
			t.Fatalf("after %d keys len(buckets) = %d, want <= %d", i+1, got, limit)
		}
	}
	if got := len(l.buckets); got != limit {
		t.Fatalf("len(buckets) = %d, want %d", got, limit)
	}
}

func TestEviction(t *testing.T) {
	tests := []struct {
		name     string
		rate     float64
		burst    int
		limit    int
		run      func(clk *fakeClock, l *Limiter)
		wantKeys []string
	}{
		{
			// L is the least recently used but not full; F is more recent and
			// already full again. The full bucket must be dropped first.
			name:  "prefers full bucket over LRU",
			rate:  1,
			burst: 2,
			limit: 2,
			run: func(clk *fakeClock, l *Limiter) {
				l.Allow("L")
				l.Allow("L") // L drained to 0 tokens at t0
				clk.Advance(100 * time.Millisecond)
				l.Allow("F") // F at 1 token at t0+0.1s
				clk.Advance(1400 * time.Millisecond)
				// t0+1.5s: F projects to 2 (full), L to 1.5 (not full)
				l.Allow("N")
			},
			wantKeys: []string{"L", "N"},
		},
		{
			// No bucket is full, so the least recently used one is dropped.
			name:  "falls back to LRU when none are full",
			rate:  0.001,
			burst: 2,
			limit: 2,
			run: func(clk *fakeClock, l *Limiter) {
				l.Allow("a") // t0
				clk.Advance(time.Second)
				l.Allow("b") // t0+1s
				clk.Advance(time.Second)
				l.Allow("a") // t0+2s, refreshes a
				l.Allow("c") // at limit: b is LRU
			},
			wantKeys: []string{"a", "c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := newFakeClock()
			l := &Limiter{Rate: tt.rate, Burst: tt.burst, Limit: tt.limit, Now: clk.Now}
			tt.run(clk, l)

			if got := len(l.buckets); got != len(tt.wantKeys) {
				t.Fatalf("len(buckets) = %d, want %d (keys %v)", got, len(tt.wantKeys), keysOf(l))
			}
			for _, k := range tt.wantKeys {
				if _, ok := l.buckets[k]; !ok {
					t.Fatalf("key %q evicted, want kept; keys %v", k, keysOf(l))
				}
			}
		})
	}
}

func keysOf(l *Limiter) []string {
	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	return keys
}

func TestConcurrentAllow(t *testing.T) {
	const (
		goroutines = 64
		calls      = 10
		burst      = 50
	)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("shared key admits exactly burst", func(t *testing.T) {
		// Rate is negligible, so no refill happens during the test and the
		// shared key must admit exactly burst requests.
		l := &Limiter{Rate: 1e-9, Burst: burst, Limit: 1000, Now: func() time.Time { return fixed }}

		var allowed atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < calls; i++ {
					if ok, _ := l.Allow("shared"); ok {
						allowed.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		if got := allowed.Load(); got != burst {
			t.Fatalf("allowed = %d, want %d", got, burst)
		}
	})

	t.Run("distinct keys under eviction stay bounded", func(t *testing.T) {
		const limit = 16
		l := &Limiter{Rate: 1, Burst: 1, Limit: limit, Now: func() time.Time { return fixed }}

		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < calls; i++ {
					l.Allow(fmt.Sprintf("g%d-k%d", g, i))
					l.Allow("hot")
				}
			}(g)
		}
		wg.Wait()

		l.mu.Lock()
		defer l.mu.Unlock()
		if got := len(l.buckets); got > limit {
			t.Fatalf("len(buckets) = %d, want <= %d", got, limit)
		}
	})
}

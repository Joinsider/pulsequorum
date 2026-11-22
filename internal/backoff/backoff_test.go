// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package backoff_test

import (
	"context"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/backoff"
)

func TestHalfJitterBoundsGrowthAndReset(t *testing.T) {
	tests := []struct {
		name string
		minD time.Duration
		maxD time.Duration
		want []time.Duration // expected cur at each step
	}{
		{
			name: "doubles_until_capped",
			minD: 100 * time.Millisecond,
			maxD: 500 * time.Millisecond,
			want: []time.Duration{
				100 * time.Millisecond,
				200 * time.Millisecond,
				400 * time.Millisecond,
				500 * time.Millisecond,
				500 * time.Millisecond,
			},
		},
		{
			name: "equal_min_and_max",
			minD: 50 * time.Millisecond,
			maxD: 50 * time.Millisecond,
			want: []time.Duration{
				50 * time.Millisecond,
				50 * time.Millisecond,
			},
		},
		{
			name: "tiny_duration",
			minD: 1 * time.Nanosecond,
			maxD: 4 * time.Nanosecond,
			want: []time.Duration{
				1 * time.Nanosecond,
				2 * time.Nanosecond,
				4 * time.Nanosecond,
				4 * time.Nanosecond,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := backoff.HalfJitter(tc.minD, tc.maxD)
			for round := range 2 {
				for step, cur := range tc.want {
					d := b.Wait()
					lo := cur / 2
					if d < lo || d >= cur {
						t.Fatalf("round %d step %d: Wait() = %v, want in [%v, %v)", round, step, d, lo, cur)
					}
				}
				b.Reset()
			}

			// Verify distribution stays within [cur/2, cur) across many samples.
			for range 100 {
				b.Reset()
				for step, cur := range tc.want {
					d := b.Wait()
					lo := cur / 2
					if d < lo || d >= cur {
						t.Fatalf("sample step %d: Wait() = %v, want in [%v, %v)", step, d, lo, cur)
					}
				}
			}
		})
	}
}

func TestFullJitterDeterministicBoundsAndReset(t *testing.T) {
	const (
		base = 1 * time.Second
		maxD = 30 * time.Second
	)
	expectedCaps := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	t.Run("upper_bound_minus_one", func(t *testing.T) {
		var seenN []int64
		b := backoff.FullJitter(base, maxD, func(n int64) int64 {
			seenN = append(seenN, n)
			return n - 1
		})

		for round := range 2 {
			seenN = seenN[:0]
			for step, capD := range expectedCaps {
				got := b.Wait()
				if got != capD-1 {
					t.Fatalf("round %d step %d: Wait() = %v, want %v", round, step, got, capD-1)
				}
				if seenN[step] != int64(capD) {
					t.Fatalf("round %d step %d: randIntN arg = %d, want %d", round, step, seenN[step], int64(capD))
				}
			}
			b.Reset()
		}
	})

	t.Run("lower_bound_zero", func(t *testing.T) {
		b := backoff.FullJitter(base, maxD, func(int64) int64 { return 0 })
		for step := range expectedCaps {
			if got := b.Wait(); got != 0 {
				t.Fatalf("step %d: Wait() = %v, want 0", step, got)
			}
		}
	})

	t.Run("non_positive_cur_returns_zero", func(t *testing.T) {
		called := false
		for _, minD := range []time.Duration{0, -time.Second} {
			b := backoff.FullJitter(minD, maxD, func(int64) int64 {
				called = true
				return 0
			})
			if got := b.Wait(); got != 0 {
				t.Fatalf("minD=%v: Wait() = %v, want 0", minD, got)
			}
			if called {
				t.Fatalf("minD=%v: randIntN should not be called when cur <= 0", minD)
			}
		}
	})

	t.Run("nil_randIntN_uses_default", func(t *testing.T) {
		b := backoff.FullJitter(10*time.Millisecond, 40*time.Millisecond, nil)
		for _, capD := range []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond} {
			d := b.Wait()
			if d < 0 || d >= capD {
				t.Fatalf("Wait() = %v, want in [0, %v)", d, capD)
			}
		}
	})
}

func TestSleep(t *testing.T) {
	t.Run("completes_normally", func(t *testing.T) {
		ctx := context.Background()
		if !backoff.Sleep(ctx, 5*time.Millisecond) {
			t.Fatal("Sleep returned false for active context")
		}
	})

	t.Run("cancelled_context_returns_false_immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		if backoff.Sleep(ctx, 5*time.Second) {
			t.Fatal("Sleep returned true for cancelled context")
		}
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("Sleep took %v on cancelled context, expected immediate return", elapsed)
		}
	})

	t.Run("non_positive_duration", func(t *testing.T) {
		if !backoff.Sleep(context.Background(), 0) {
			t.Fatal("Sleep(0) returned false for active context")
		}
		if !backoff.Sleep(context.Background(), -time.Millisecond) {
			t.Fatal("Sleep(-1ms) returned false for active context")
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if backoff.Sleep(ctx, 0) {
			t.Fatal("Sleep(0) returned true for cancelled context")
		}
		if backoff.Sleep(ctx, -time.Millisecond) {
			t.Fatal("Sleep(-1ms) returned true for cancelled context")
		}
	})
}

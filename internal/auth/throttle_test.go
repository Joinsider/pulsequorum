// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package auth

import (
	"strconv"
	"testing"
	"time"
)

func TestThrottleDelays(t *testing.T) {
	// The wait starts with the second failure, here Free=2: then 1 s, 2 s, 4 s,
	// 8 s and the cap of 10 s.
	th := &Throttle{Free: 2, Base: time.Second, Max: 10 * time.Second, Limit: 100}
	now := time.Unix(1_800_000_000, 0)
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := th.Wait("acc", now); got != 0 {
			t.Fatalf("before failure %d: Wait = %s, want 0", i+1, got)
		}
		th.Fail("acc", now)
		if got := th.Wait("acc", now); got != w {
			t.Errorf("after failure %d: Wait = %s, want %s", i+1, got, w)
		}
		now = now.Add(w)
	}
}

func TestThrottleFreeFailuresAndFirstDelay(t *testing.T) {
	// ADR 0008: the Free-th failure already imposes the first delay.
	th := &Throttle{Free: 5, Base: time.Second, Max: time.Minute, Limit: 10}
	now := time.Unix(1_800_000_000, 0)
	for i := 1; i <= 4; i++ {
		th.Fail("acc", now)
		if got := th.Wait("acc", now); got != 0 {
			t.Fatalf("after failure %d: Wait = %s, want 0", i, got)
		}
	}
	th.Fail("acc", now)
	if got := th.Wait("acc", now); got != time.Second {
		t.Errorf("after failure 5: Wait = %s, want 1s", got)
	}
}

func TestThrottleAllowsAttemptAtDeadline(t *testing.T) {
	th := &Throttle{Free: 1, Base: time.Second, Max: time.Minute, Limit: 10}
	now := time.Unix(1_800_000_000, 0)
	th.Fail("acc", now)
	if got := th.Wait("acc", now.Add(999*time.Millisecond)); got != time.Millisecond {
		t.Errorf("Wait 999 ms after failure = %s, want 1ms", got)
	}
	if got := th.Wait("acc", now.Add(time.Second)); got != 0 {
		t.Errorf("Wait at the deadline = %s, want 0", got)
	}
}

func TestThrottleSucceedResets(t *testing.T) {
	th := NewThrottle(2)
	now := time.Unix(1_800_000_000, 0)
	th.Fail("acc", now)
	th.Fail("acc", now)
	if th.Wait("acc", now) == 0 {
		t.Fatal("second failure with two free failures must wait")
	}
	th.Succeed("acc")
	if got := th.Wait("acc", now); got != 0 {
		t.Errorf("Wait after Succeed = %s, want 0", got)
	}
	th.Fail("acc", now)
	if got := th.Wait("acc", now); got != 0 {
		t.Errorf("first failure after Succeed waits %s; the count must start over", got)
	}
}

// Failures older than Max are forgotten; a recent failure still counts.
func TestThrottleForgetsOldFailures(t *testing.T) {
	th := &Throttle{Free: 2, Base: time.Second, Max: 10 * time.Second, Limit: 10}
	now := time.Unix(1_800_000_000, 0)
	th.Fail("acc", now)
	th.Fail("acc", now) // second failure: waits 1 s
	if got := th.Wait("acc", now); got != time.Second {
		t.Fatalf("Wait = %s, want 1s", got)
	}

	// Within Max the count continues: this is the third failure, 2 s.
	now = now.Add(5 * time.Second)
	th.Fail("acc", now)
	if got := th.Wait("acc", now); got != 2*time.Second {
		t.Errorf("failure within Max: Wait = %s, want 2s", got)
	}

	// After more than Max without a failure the count starts over.
	now = now.Add(2*time.Second + 11*time.Second)
	th.Fail("acc", now)
	if got := th.Wait("acc", now); got != 0 {
		t.Errorf("failure after Max of quiet: Wait = %s, want 0 (count reset)", got)
	}
}

func TestThrottleKeysAreIndependent(t *testing.T) {
	th := NewThrottle(0)
	now := time.Unix(1_800_000_000, 0)
	th.Fail("u:alice", now)
	if th.Wait("u:alice", now) == 0 {
		t.Fatal("alice must wait after a failure")
	}
	if got := th.Wait("u:bob", now); got != 0 {
		t.Errorf("bob waits %s because of alice", got)
	}
	th.Succeed("u:alice")
	if got := th.Wait("u:bob", now); got != 0 {
		t.Errorf("bob waits after alice succeeded: %s", got)
	}
}

func TestThrottleEvictionKeepsLimit(t *testing.T) {
	th := &Throttle{Free: 0, Base: time.Second, Max: time.Minute, Limit: 3}
	now := time.Unix(1_800_000_000, 0)
	for i := range 10 {
		th.Fail("k"+strconv.Itoa(i), now)
		if n := len(th.entries); n > th.Limit {
			t.Fatalf("after %d keys: %d entries, limit %d", i+1, n, th.Limit)
		}
		now = now.Add(time.Second)
	}
	if len(th.entries) != th.Limit {
		t.Errorf("entries = %d, want %d", len(th.entries), th.Limit)
	}
	// The newest key must still be tracked, the oldest ones dropped.
	if _, ok := th.entries["k9"]; !ok {
		t.Error("most recent key was evicted")
	}
	if _, ok := th.entries["k0"]; ok {
		t.Error("oldest key was kept")
	}
}

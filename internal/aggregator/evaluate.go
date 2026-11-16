// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import "github.com/joinsider/uptimecluster/internal/store"

// streak counts consecutive evaluation rounds of the same kind.
type streak struct {
	n       int
	sinceMs int64 // start of the first round of the streak
}

// tracker is the evaluation state of one monitor on the leader (ADR 0021).
// Only status changes are replicated; the streaks are rebuilt from heartbeats
// after a leader change. Its status mirrors the replicated status.
type tracker struct {
	status store.MonitorStatus
	fail   streak // consecutive negative rounds (DOWN consensus)
	ok     streak // consecutive positive rounds (UP, DEGRADED, PARTIAL)
}

// observe adds one evaluation round. target is the consensus of the round
// (see consensus); "" means no report, which counts for neither streak.
func (t *tracker) observe(target store.MonitorStatus, startMs int64) {
	switch target {
	case "":
		return
	case store.StatusDown:
		if t.fail.n == 0 {
			t.fail.sinceMs = startMs
		}
		t.fail.n++
		t.ok = streak{}
	default:
		if t.ok.n == 0 {
			t.ok.sinceMs = startMs
		}
		t.ok.n++
		t.fail = streak{}
	}
}

// resetStreaks forgets the streaks, e.g. after a maintenance round, so that
// rounds before it do not count (FA-MNT-02).
func (t *tracker) resetStreaks() {
	t.fail, t.ok = streak{}, streak{}
}

// next decides whether the round with target (observed at startMs) changes the
// status (FA-CON-03, FA-CON-05). It returns the new status, its start and
// whether it changes:
//
//   - retries+1 consecutive DOWN rounds lead to DOWN, the rounds before to
//     PENDING; DOWN dates back to the first round of the streak.
//   - From DOWN, recovery consecutive positive rounds lead to the positive
//     target, dating back to the first of them.
//   - Otherwise a positive target is set at once, dated to this round.
//   - No report sets UNKNOWN, except from DOWN and PENDING, which stay as they
//     are (the incident stays open, no recovery without a measurement).
//   - PAUSED and MAINTENANCE are set elsewhere and never left here.
func (t *tracker) next(target store.MonitorStatus, startMs int64, retries, recovery int) (store.MonitorStatus, int64, bool) {
	switch t.status {
	case store.StatusPaused, store.StatusMaintenance:
		return "", 0, false
	}

	switch target {
	case "":
		switch t.status {
		case store.StatusDown, store.StatusPending, store.StatusUnknown:
			return "", 0, false
		}
		return store.StatusUnknown, startMs, true
	case store.StatusDown:
		switch {
		case t.status == store.StatusDown:
			return "", 0, false
		case t.fail.n >= retries+1:
			return store.StatusDown, t.fail.sinceMs, true
		case t.status != store.StatusPending:
			return store.StatusPending, t.fail.sinceMs, true
		}
		return "", 0, false
	}

	switch {
	case t.status == store.StatusDown && t.ok.n >= max(recovery, 1):
		return target, t.ok.sinceMs, true
	case t.status == store.StatusDown, t.status == target:
		return "", 0, false
	}
	return target, startMs, true
}

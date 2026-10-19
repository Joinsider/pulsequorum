// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package aggregator

import (
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/store"
)

// streak counts consecutive evaluations of the same kind.
type streak struct {
	n       int
	sinceMs int64 // time of the first evaluation of the streak
}

// tracker is the evaluation state of one monitor. Only status changes are
// replicated; the streaks are rebuilt from heartbeats after a restart.
type tracker struct {
	revision uint64
	status   store.MonitorStatus
	fail, ok streak
	// lastFailure is the most recent negative result, the cause of an incident.
	lastFailure checker.Result
}

// observe adds one evaluation.
func (t *tracker) observe(r checker.Result, atMs int64) {
	if r.Status == checker.StatusDown {
		if t.fail.n == 0 {
			t.fail.sinceMs = atMs
		}
		t.fail.n++
		t.ok = streak{}
		t.lastFailure = r
		return
	}
	if t.ok.n == 0 {
		t.ok.sinceMs = atMs
	}
	t.ok.n++
	t.fail = streak{}
}

// next decides whether the latest evaluation (result at atMs, already
// observed) changes the status (FA-CON-03):
//
//   - retries+1 consecutive failures lead to DOWN, the failures before that to
//     PENDING; DOWN dates back to the first failure of the streak.
//   - From DOWN, recovery consecutive successes lead back to UP (or DEGRADED),
//     dating back to the first success of the streak.
//   - Otherwise a success sets UP or DEGRADED immediately.
//   - PAUSED and MAINTENANCE are set elsewhere and never left here.
func (t *tracker) next(result checker.Status, atMs int64, retries, recovery int) (store.MonitorStatus, int64, bool) {
	switch t.status {
	case store.StatusPaused, store.StatusMaintenance:
		return "", 0, false
	}

	if result == checker.StatusDown {
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

	to := store.StatusUp
	if result == checker.StatusDegraded {
		to = store.StatusDegraded
	}
	switch {
	case t.status == store.StatusDown && t.ok.n >= max(recovery, 1):
		return to, t.ok.sinceMs, true
	case t.status == store.StatusDown, t.status == to:
		return "", 0, false
	}
	return to, atMs, true
}

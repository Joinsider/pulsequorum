// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package aggregator

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/store"
)

// reevaluateNamespace seeds the incident IDs of Reevaluate (see incidentID).
var reevaluateNamespace = uuid.MustParse("0b8f3c52-9d1e-4f6a-8c27-5e4d1a9b3f60")

// Change is one status change found by Reevaluate (FA-UPT-07, ADR 0024).
type Change struct {
	SinceMs int64
	Status  store.MonitorStatus
	// Incident is set for a change to DOWN.
	Incident *store.Incident
	// Reporters are the sorted, unique IDs of the nodes that reported in the
	// deciding slot.
	Reporters []string
}

// Reevaluate replays the slots of m whose start lies in [fromMs, toMs) with the
// rules of the live evaluation (consensus policy, retries, recovery threshold,
// slot window k-1 and k, responsible nodes), starting from the status from with
// empty streaks. hbs are the stored heartbeats of m in the window read by
// ReevaluateGap; heartbeats of other monitors or tenants are ignored.
//
// Slots inside a maintenance window (maintenance may be nil) reset the streaks
// and are skipped, as in the live evaluation; the first slot after such a window
// starts from PENDING, as the maintenance controller sets it when the window
// ends. PAUSED and MAINTENANCE are never produced. The result holds the changes
// in slot order. A non-positive interval yields none.
//
// Incident IDs are derived from the monitor and the start of the DOWN episode,
// so the same input gives the same result (see incidentID).
func Reevaluate(m store.Monitor, from store.MonitorStatus, fromMs, toMs int64, hbs []store.Heartbeat, responsible []string, maintenance func(at time.Time) bool, unit time.Duration) []Change {
	w := &watch{}
	w.configure(m, unit, defaultReportDelay)
	if w.intervalMs <= 0 || toMs <= fromMs {
		return nil
	}

	ids := slices.Clone(responsible)
	slices.Sort(ids)
	ids = slices.Compact(ids)

	w.hbs = make(map[hbKey]store.Heartbeat, len(hbs))
	for _, hb := range hbs {
		if hb.TenantID != m.TenantID || hb.MonitorID != m.ID {
			continue
		}
		w.hbs[hbKey{hb.NodeID, hb.Incarnation, hb.Seq}] = hb
	}

	t := &tracker{status: from}
	k := w.slotOf(fromMs)
	if w.start(k) < fromMs {
		k++
	}
	var out []Change
	afterMaintenance := false
	for ; w.start(k) < toMs; k++ {
		startMs := w.start(k)
		if maintenance != nil && maintenance(time.UnixMilli(startMs)) {
			t.resetStreaks()
			afterMaintenance = true
			continue
		}
		if afterMaintenance {
			// The controller moved the monitor from MAINTENANCE to PENDING.
			t.status = store.StatusPending
			afterMaintenance = false
		}
		reports := w.reporters(k, ids)
		to, since, change := decide(t, m, startMs, reports)
		if !change {
			continue
		}
		t.status = to
		c := Change{SinceMs: since, Status: to, Reporters: nodeIDs(reports)}
		if to == store.StatusDown {
			c.Incident = incidentOf(incidentID(m, since), reports)
		}
		out = append(out, c)
	}
	return out
}

// incidentID returns the ID of the incident that starts at sinceMs for m. It
// is a version 5 UUID over tenant, monitor and start, so a replay that finds
// the same episode again proposes the same ID.
func incidentID(m store.Monitor, sinceMs int64) string {
	return uuid.NewSHA1(reevaluateNamespace, fmt.Appendf(nil, "%s\x00%s\x00%d", m.TenantID, m.ID, sinceMs)).String()
}

// ReevaluateGap loads the heartbeats that the slots of m in [fromMs, toMs) need
// and replays them with Reevaluate. It is for the leader job that corrects the
// history after a quorum returned (FA-UPT-07). It reads the registry and the
// heartbeats but changes no aggregator state, so it may run next to Run.
func (a *Aggregator) ReevaluateGap(ctx context.Context, m store.Monitor, from store.MonitorStatus, fromMs, toMs int64) ([]Change, error) {
	var registry []store.Node
	if a.opts.Registry != nil {
		nodes, err := a.opts.Registry(ctx)
		if err != nil {
			return nil, fmt.Errorf("read node registry: %w", err)
		}
		registry = nodes
	}
	w := &watch{}
	w.configure(m, a.opts.Unit, a.opts.ReportDelay)
	if w.intervalMs <= 0 || toMs <= fromMs {
		return nil, nil
	}
	// The slots need one interval before fromMs and one after the last slot
	// start, and the rules look back lookback slots.
	lo := fromMs - w.lookback()*w.intervalMs
	hbs, err := a.loadHeartbeats(ctx, m.TenantID, m.ID, lo, toMs+w.intervalMs)
	if err != nil {
		return nil, fmt.Errorf("load heartbeats: %w", err)
	}
	var maintenance func(at time.Time) bool
	if a.opts.Maintenance != nil {
		maintenance = func(at time.Time) bool { return a.opts.Maintenance(m, at) }
	}
	return Reevaluate(m, from, fromMs, toMs, hbs, a.responsible(m, registry), maintenance, a.opts.Unit), nil
}

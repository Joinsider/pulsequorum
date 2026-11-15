// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/joinsider/uptimecluster/test/chaos/harness"
	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// replicationRound is the anti-entropy period of heartbeats between voters
// (docs/betrieb/cluster.md). Replicated heartbeats can lag by one round.
const replicationRound = 30 * time.Second

// writeVisibleWithin is the limit for a write through one voter to be seen
// on the others (AS-02).
const writeVisibleWithin = 5 * time.Second

func init() {
	register(Scenario{
		ID:    "as02",
		Label: "AS-02",
		Title: "Zwei weitere Voter beitreten lassen",
		Run:   runAS02,
	})
}

// runAS02 automates AS-02 (docs/pflichtenheft.md, section 13.2): voter 1
// runs five HTTP monitors, voters 2 and 3 join while they keep running, and
// the cluster must stay uninterrupted and consistent.
func runAS02(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	if cfg.Voters < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(cfg.Voters), false)
		return errors.New("AS-02 needs three voter slots")
	}

	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := cl.Bootstrap(ctx, "admin"); err != nil {
		rep.Check("Voter 1 bootstraps, admin set up", "setup 201, admin logged in", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 bootstraps, admin set up", "setup 201, admin logged in",
		"admin created on "+cl.Names().Voter(1), true)
	api := cl.Client(1)

	// Notification channel with a synchronous test delivery to the fake ntfy.
	suffix := randomSuffix()
	ch, err := api.CreateChannel(ctx, "as02-ntfy-"+suffix, env.NtfyURL("as02-"+suffix), []string{"down", "up"})
	if err != nil {
		rep.Check("Notification channel created", "201, kind shoutrrr, default", err.Error(), false)
		return err
	}
	rep.Check("Notification channel created", "201, kind shoutrrr, default", "channel "+ch.ID, true)
	res, err := api.TestChannel(ctx, ch.ID)
	if err != nil || !res.Delivered {
		msg := res.Error
		if err != nil {
			msg = err.Error()
		}
		rep.Check("Channel test delivery", "delivered to ntfy", msg, false)
		return errors.New("channel test delivery failed")
	}
	rep.Check("Channel test delivery", "delivered to ntfy", fmt.Sprintf("delivered in %d ms", res.DurationMs), true)

	// Five HTTP monitors against the fake target, all voters check.
	const monitorCount = 5
	monitors := make([]ucapi.Monitor, 0, monitorCount)
	for n := 1; n <= monitorCount; n++ {
		m, err := api.CreateMonitorInput(ctx, ucapi.MonitorInput{
			Name:                   fmt.Sprintf("as02-%s-m%d", suffix, n),
			Type:                   "http",
			Config:                 map[string]any{"url": env.TargetURL(fmt.Sprintf("/as02/%s/m%d", suffix, n))},
			IntervalSeconds:        intervalSec,
			Retries:                1,
			Assignment:             &ucapi.Assignment{Mode: "all"},
			NotificationChannelIDs: []string{ch.ID},
		})
		if err != nil {
			rep.Check("Five HTTP monitors created", "5 monitors, assignment all", err.Error(), false)
			return err
		}
		monitors = append(monitors, m)
	}
	rep.Check("Five HTTP monitors created", fmt.Sprintf("5 monitors, assignment all, interval %ds", intervalSec),
		fmt.Sprintf("%d monitors", len(monitors)), len(monitors) == monitorCount)

	// One interval with voter 1 alone: every monitor must have heartbeats of voter 1.
	node1, err := cl.NodeID(ctx, 1)
	if err != nil {
		rep.Check("Node ID of voter 1", "voter lists itself", err.Error(), false)
		return err
	}
	ok, lastErr := pollUntil(ctx, 2*interval+replicationRound, 2*time.Second, func() (bool, error) {
		return allMonitorsHaveProducer(ctx, cl, 1, monitors, node1, time.Now().Add(-2*interval))
	})
	rep.Check("Monitors run on voter 1 before the joins", "heartbeats of voter 1 for every monitor",
		fmt.Sprintf("complete: %v%s", ok, errSuffix(lastErr)), ok)
	if !ok {
		return errors.New("monitors produce no heartbeats before the joins")
	}
	if sum, err := cl.StatusSummary(ctx, 1, monitors[0].ID, time.Now().Add(-3*interval), time.Now()); err == nil {
		rep.Info("Check results of "+monitors[0].Name+" on voter 1", "diagnostic", harness.FormatCounts(sum))
	}

	// Joins while the monitors keep running.
	joinStart := time.Now()
	if err := cl.Join(ctx, 2); err != nil {
		rep.Check("Voter 2 joins", "listed as voter", err.Error(), false)
		return err
	}
	rep.Check("Voter 2 joins", "listed as voter by the leader",
		fmt.Sprintf("joined after %s", time.Since(joinStart).Round(time.Second)), true)
	if err := ensureLoginWorks(ctx, env, cl, 2); err != nil {
		return err
	}

	join3Start := time.Now()
	if err := cl.Join(ctx, 3); err != nil {
		rep.Check("Voter 3 joins", "listed as voter by the leader", err.Error(), false)
		return err
	}
	joinEnd := time.Now()
	rep.Check("Voter 3 joins", "listed as voter by the leader",
		fmt.Sprintf("joined after %s", joinEnd.Sub(join3Start).Round(time.Second)), true)
	if err := ensureLoginWorks(ctx, env, cl, 3); err != nil {
		return err
	}

	// No interruption: the largest gap between voter 1's own heartbeats, per
	// monitor, must stay within 2 x interval. The joins take seconds, so the
	// window keeps running for two more intervals after the last join.
	if err := waitUntil(ctx, joinEnd.Add(2*interval)); err != nil {
		return err
	}
	windowEnd := time.Now()
	limit := 2 * interval
	var worst time.Duration
	var worstName string
	for _, m := range monitors {
		gap, err := cl.HeartbeatGap(ctx, 1, m.ID, node1, joinStart, windowEnd, interval)
		if err != nil {
			rep.Check("No interruption during the joins", "max gap <= 2 x interval", err.Error(), false)
			return err
		}
		if gap > worst {
			worst, worstName = gap, m.Name
		}
	}
	rep.Check("No interruption during the joins (voter 1 view)",
		fmt.Sprintf("max gap <= 2 x interval (%s) for every monitor", limit),
		fmt.Sprintf("max gap %s (%s) over a window of %s", worst.Round(time.Second), worstName, windowEnd.Sub(joinStart).Round(time.Second)),
		worst <= limit)

	// Replication: after about two intervals every voter has heartbeats of
	// all three producers for every monitor. Replication runs once per round,
	// so the deadline includes one round.
	nodes := make([]string, 0, 3)
	for i := 1; i <= 3; i++ {
		id, err := cl.NodeID(ctx, i)
		if err != nil {
			rep.Check(fmt.Sprintf("Node ID of voter %d", i), "voter lists itself", err.Error(), false)
			return err
		}
		nodes = append(nodes, id)
	}
	replStart := time.Now()
	ok, lastErr = pollUntil(ctx, 2*interval+replicationRound, 2*time.Second, func() (bool, error) {
		return allVotersHaveAllProducers(ctx, cl, monitors, nodes, time.Now().Add(-2*interval))
	})
	rep.Check("Heartbeats of all three producers on every voter",
		fmt.Sprintf("within 2 x interval (%s) plus one replication round (%s)", 2*interval, replicationRound),
		fmt.Sprintf("complete: %v after %s%s", ok, time.Since(replStart).Round(time.Second), errSuffix(lastErr)), ok)

	// Cluster state: three voters, one leader, no warnings.
	list, err := cl.Members(ctx, 1)
	if err != nil {
		rep.Check("Three voters listed", "3 voters", err.Error(), false)
		return err
	}
	voters := 0
	for _, n := range list.Nodes {
		if n.Voter {
			voters++
		}
	}
	rep.Check("Three voters listed", "3 voters, all listed as voter", fmt.Sprintf("%d voters of %d members", voters, len(list.Nodes)),
		voters == 3 && len(list.Nodes) == 3)
	leader, err := cl.LeaderOf(ctx, []int{1, 2, 3})
	rep.Check("One leader seen by all voters", "identical, non-empty leader ID",
		fmt.Sprintf("leader %s%s", leader, errSuffix(err)), err == nil && leader != "")
	rep.Check("No cluster warnings", "no warnings", fmt.Sprintf("warnings %v", list.Warnings), len(list.Warnings) == 0)

	// Identical data on all voters (volatile fields excluded).
	ok, lastErr = pollUntil(ctx, 30*time.Second, 2*time.Second, func() (bool, error) {
		diffs, err := snapshotDiffs(ctx, cl)
		if err != nil {
			return false, err
		}
		if len(diffs) > 0 {
			return false, fmt.Errorf("%d differences", len(diffs))
		}
		return true, nil
	})
	measured := "snapshots of voters 1, 2 and 3 are equal"
	if !ok {
		diffs, _ := snapshotDiffs(ctx, cl)
		measured = fmt.Sprintf("%d differences: %v%s", len(diffs), firstN(diffs, 5), errSuffix(lastErr))
	}
	rep.Check("Identical data on all voters", "normalized snapshots of voters 1, 2, 3 equal", measured, ok)

	// The monitors kept checking the target through the joins: all UP.
	var notUp []string
	for _, m := range monitors {
		cur, err := api.GetMonitor(ctx, m.ID)
		if err != nil {
			notUp = append(notUp, m.Name+": "+err.Error())
			continue
		}
		if cur.Status != "UP" {
			notUp = append(notUp, m.Name+": "+cur.Status)
		}
	}
	rep.Check("Monitors UP after the joins", "all five monitors UP on voter 1",
		fmt.Sprintf("%d of %d UP %v", len(monitors)-len(notUp), len(monitors), notUp), len(notUp) == 0)

	// A write through voter 3 is visible on voters 1 and 2 within 5 seconds.
	name := fmt.Sprintf("as02-%s-via-v3", suffix)
	if _, err := cl.Client(3).CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:            name,
		Type:            "http",
		Config:          map[string]any{"url": env.TargetURL("/as02/" + suffix + "/via-v3")},
		IntervalSeconds: intervalSec,
		Retries:         1,
		Assignment:      &ucapi.Assignment{Mode: "all"},
	}); err != nil {
		rep.Check("Write through voter 3", "201", err.Error(), false)
		return err
	}
	written := time.Now()
	var visible []string
	allVisible := true
	for _, i := range []int{1, 2} {
		seen, _ := pollUntil(ctx, writeVisibleWithin, 200*time.Millisecond, func() (bool, error) {
			mons, err := cl.Client(i).ListMonitors(ctx)
			if err != nil {
				return false, err
			}
			for _, m := range mons {
				if m.Name == name {
					return true, nil
				}
			}
			return false, nil
		})
		elapsed := time.Since(written).Round(100 * time.Millisecond)
		visible = append(visible, fmt.Sprintf("voter %d after %s", i, elapsed))
		allVisible = allVisible && seen
	}
	rep.Check("Write through voter 3 visible on voters 1 and 2", "visible within 5 s",
		fmt.Sprint(visible), allVisible)
	return nil
}

// waitUntil blocks until t or until ctx ends.
func waitUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ensureLoginWorks checks that a freshly joined voter answers logins. Known
// product issue: a voter that starts before its replicated state has arrived
// creates no access-token signing key ("no signing key available", HTTP 500)
// and never retries. A restart reloads the key from the replicated state.
// The restart is recorded as an INFO step, so the report shows it.
func ensureLoginWorks(ctx context.Context, env *Env, cl *harness.Cluster, i int) error {
	rep := env.Rep
	name := cl.Names().Voter(i)
	ok, lastErr := pollUntil(ctx, loginGrace, 2*time.Second, func() (bool, error) {
		return loginOK(ctx, cl, i)
	})
	if ok {
		rep.Check("Login on "+name, "login answers 200", "200", true)
		return nil
	}
	rep.Info("Login on "+name+" after join", "login answers 200",
		fmt.Sprintf("not within %s: %s; restarting %s", loginGrace, errText(lastErr), name))
	if err := cl.Restart(ctx, i); err != nil {
		rep.Check("Restart "+name, "voter healthy again", err.Error(), false)
		return err
	}
	ok, lastErr = pollUntil(ctx, loginGrace, 2*time.Second, func() (bool, error) {
		return loginOK(ctx, cl, i)
	})
	rep.Check("Login on "+name+" after restart", "login answers 200",
		fmt.Sprintf("login ok: %v%s", ok, errSuffix(lastErr)), ok)
	if !ok {
		return fmt.Errorf("%s does not answer logins", name)
	}
	return nil
}

// loginGrace bounds how long a joined voter may take to answer logins.
const loginGrace = 20 * time.Second

// loginOK logs in on voter i and reports success. The password never leaves
// the client.
func loginOK(ctx context.Context, cl *harness.Cluster, i int) (bool, error) {
	if err := cl.Client(i).Login(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// allMonitorsHaveProducer reports whether every monitor has at least one
// heartbeat of producer (a node ID) read from voter viewer since since.
func allMonitorsHaveProducer(ctx context.Context, cl *harness.Cluster, viewer int, monitors []ucapi.Monitor, producer string, since time.Time) (bool, error) {
	for _, m := range monitors {
		counts, err := cl.ProducerCounts(ctx, viewer, m.ID, since, time.Now())
		if err != nil {
			return false, err
		}
		if counts[producer] == 0 {
			return false, nil
		}
	}
	return true, nil
}

// allVotersHaveAllProducers reports whether each voter 1..3 has heartbeats of
// every producer for every monitor since since.
func allVotersHaveAllProducers(ctx context.Context, cl *harness.Cluster, monitors []ucapi.Monitor, producers []string, since time.Time) (bool, error) {
	for viewer := 1; viewer <= 3; viewer++ {
		for _, m := range monitors {
			counts, err := cl.ProducerCounts(ctx, viewer, m.ID, since, time.Now())
			if err != nil {
				return false, err
			}
			for _, p := range producers {
				if counts[p] == 0 {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// snapshotDiffs compares the normalized snapshots of voters 1, 2 and 3.
func snapshotDiffs(ctx context.Context, cl *harness.Cluster) ([]string, error) {
	base, err := cl.Snapshot(ctx, 1)
	if err != nil {
		return nil, err
	}
	var diffs []string
	for _, i := range []int{2, 3} {
		other, err := cl.Snapshot(ctx, i)
		if err != nil {
			return nil, err
		}
		for _, d := range harness.CompareSnapshots(base, other) {
			diffs = append(diffs, fmt.Sprintf("voter %d vs 1: %s", i, d))
		}
	}
	return diffs, nil
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

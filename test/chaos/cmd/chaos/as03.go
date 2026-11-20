// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
)

// as03Monitors is the number of HTTP monitors that fail together.
const as03Monitors = 2

// as03Retries is the retry count of the monitors (the DOWN alert is due
// after retries+1 failed checks).
const as03Retries = 1

func init() {
	register(Scenario{
		ID:    "as03",
		Label: "AS-03",
		Title: "Leader beenden: Failover, genau ein DOWN und ein UP je Monitor",
		Run:   runAS03,
	})
}

// runAS03 automates AS-03 (docs/pflichtenheft.md): the target of two monitors
// fails, the leader is killed around the time the DOWN alert is due, the
// survivors elect a new leader within 10 s and every monitor sends exactly
// one DOWN and, after recovery, exactly one UP. The killed voter rejoins with
// its data. The second failover kills the new leader while the target is
// healthy and must produce no alert at all.
func runAS03(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(n), false)
		return errors.New("AS-03 needs three voter slots")
	}

	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := bringUp(ctx, env, cl, n); err != nil {
		return err
	}

	// Notification channel with a synchronous test delivery to the fake ntfy.
	api := cl.Client(1)
	suffix := randomSuffix()
	ch, err := api.CreateChannel(ctx, "as03-ntfy-"+suffix, env.NtfyURL("as03-"+suffix), []string{"down", "up"})
	if err != nil {
		rep.Check("Notification channel created", "201, kind shoutrrr", err.Error(), false)
		return err
	}
	rep.Check("Notification channel created", "201, kind shoutrrr", "channel "+ch.ID, true)
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

	// Monitors against the fake target (healthy until the failure is injected).
	env.Target.SetFailing(false)
	names := make([]string, 0, as03Monitors)
	ids := make([]string, 0, as03Monitors)
	created := time.Now()
	for k := 1; k <= as03Monitors; k++ {
		name := fmt.Sprintf("as03-%s-m%d", suffix, k)
		m, err := createHTTPMonitor(ctx, api, name, env.TargetURL(fmt.Sprintf("/as03/%s/m%d", suffix, k)), intervalSec, []string{ch.ID})
		if err != nil {
			rep.Check("HTTP monitors created", "201, assignment all", err.Error(), false)
			return err
		}
		names = append(names, m.Name)
		ids = append(ids, m.ID)
	}
	rep.Check("HTTP monitors created", fmt.Sprintf("%d monitors, assignment all, interval %ds, retries %d", as03Monitors, intervalSec, as03Retries),
		fmt.Sprintf("%d monitors", len(ids)), len(ids) == as03Monitors)

	up, _ := waitMonitorStatus(ctx, api, ids, "UP", 2*interval+replicationRound)
	rep.Check("Monitors UP before the failure", "all monitors UP", fmt.Sprintf("all UP: %v", up), up)
	if !up {
		return errors.New("monitors do not come up")
	}
	nodes, err := nodeIDs(ctx, cl, slots)
	if err != nil {
		rep.Check("Node IDs of all voters", "every voter lists itself", err.Error(), false)
		return err
	}
	producers := make([]string, 0, n)
	for _, i := range slots {
		producers = append(producers, nodes[i])
	}
	allSeen, _ := waitHeartbeatsOfAll(ctx, cl, 1, ids, producers, created, 2*interval+replicationRound)
	rep.Check("Every voter checks every monitor before the failure", "heartbeats of all voters for all monitors",
		fmt.Sprintf("complete: %v", allSeen), allSeen)

	oldSlot, oldLeader, err := leaderSlot(ctx, cl)
	rep.Check("One leader before the failure", "identical leader ID on all voters",
		fmt.Sprintf("voter %d, node %s%s", oldSlot, oldLeader, errSuffix(err)), err == nil)
	if err != nil {
		return err
	}

	// Failure: the target answers 503 from now on. The leader is killed when
	// the DOWN alert is due: retries+1 intervals after the failure.
	t0 := time.Now()
	env.Target.SetFailing(true)
	dueAt := t0.Add(time.Duration(as03Retries+1) * interval)
	if err := waitUntil(ctx, dueAt); err != nil {
		return err
	}
	survivors := without(slots, oldSlot)
	killOffset := time.Since(t0)
	elect, newLeader, err := killLeader(ctx, cl, oldSlot, oldLeader, survivors)
	rep.Check("SIGKILL of the leader when the DOWN alert is due",
		fmt.Sprintf("kill about %s after the failure", (as03Retries+1)*interval),
		fmt.Sprintf("voter %d (node %s) killed at +%s", oldSlot, oldLeader, killOffset.Round(100*time.Millisecond)), true)
	rep.Check("New leader agreed by the survivors", fmt.Sprintf("< %s (NFA-REL-02)", failoverLimit),
		fmt.Sprintf("%s, leader %s%s", electMs(elect), newLeader, errSuffix(err)), err == nil && elect < failoverLimit)
	if err != nil {
		return err
	}

	// One DOWN per monitor, due within (retries+1) intervals + 30 s + election time.
	downBy := t0.Add(time.Duration(as03Retries+1)*interval + 30*time.Second + elect)
	alive := survivors[0]
	if _, err := pollUntil(ctx, time.Until(downBy), time.Second, func() (bool, error) {
		for _, name := range names {
			if c, _ := notifyCount(env.Ntfy, t0, "DOWN: "+name); c == 0 {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	// Give duplicates one more interval to show up.
	if err := waitUntil(ctx, time.Now().Add(interval)); err != nil {
		return err
	}
	for _, name := range names {
		c, at := notifyCount(env.Ntfy, t0, "DOWN: "+name)
		first := "none"
		if len(at) > 0 {
			first = "+" + at[0].Sub(t0).Round(time.Second).String()
		}
		rep.Check("DOWN notification for "+name, fmt.Sprintf("exactly 1, due by +%s", downBy.Sub(t0).Round(time.Second)),
			fmt.Sprintf("%d DOWN, first %s", c, first), c == 1 && len(at) > 0 && !at[0].After(downBy))
	}

	// Recovery: the target answers 200 again; exactly one UP per monitor.
	t1 := time.Now()
	env.Target.SetFailing(false)
	recovered, _ := waitMonitorStatus(ctx, cl.Client(alive), ids, "UP", 2*interval+30*time.Second)
	rep.Check("Monitors UP again after recovery", "all monitors UP on a survivor", fmt.Sprintf("all UP: %v", recovered), recovered)
	if err := waitUntil(ctx, time.Now().Add(interval)); err != nil {
		return err
	}
	for _, name := range names {
		c, at := notifyCount(env.Ntfy, t1, "UP: "+name)
		rep.Check("UP notification for "+name, "exactly 1 after recovery",
			fmt.Sprintf("%d UP, first +%s", c, firstSince(at, t1)), c == 1)
	}
	for _, name := range names {
		c, _ := notifyCount(env.Ntfy, t1, "DOWN: "+name)
		rep.Check("No further DOWN for "+name+" after recovery", "0 DOWN after recovery", fmt.Sprintf("%d", c), c == 0)
	}

	// Restart the killed voter: it rejoins with its data and agrees on the leader.
	restartAt := time.Now()
	if err := cl.Start(ctx, oldSlot, harness.StartOptions{}); err != nil {
		rep.Check("Restart "+cl.Names().Voter(oldSlot), "healthy again", err.Error(), false)
		return err
	}
	rep.Check("Restart "+cl.Names().Voter(oldSlot), "healthy again", fmt.Sprintf("healthy after %s", time.Since(restartAt).Round(time.Second)), true)
	if err := requireLogin(ctx, env, cl, oldSlot); err != nil {
		return err
	}
	agreeAt, _ := pollUntil(ctx, 60*time.Second, 500*time.Millisecond, func() (bool, error) {
		id, err := cl.LeaderOf(ctx, slots)
		return err == nil && id == newLeader, nil
	})
	rep.Check("Restarted voter agrees on the new leader", "all voters name the same leader",
		fmt.Sprintf("agreed: %v after %s", agreeAt, time.Since(restartAt).Round(time.Second)), agreeAt)
	list, err := cl.Members(ctx, alive)
	voters := 0
	for _, m := range list.Nodes {
		if m.Voter {
			voters++
		}
	}
	rep.Check("Membership after the failover", fmt.Sprintf("%d voters", n),
		fmt.Sprintf("%d voters of %d members%s", voters, len(list.Nodes), errSuffix(err)), err == nil && voters == n)
	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 90*time.Second)
	rep.Check("Snapshots of all voters equal after the restart", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)

	// Second failover while the target is healthy: the new leader is killed.
	// Neither DOWN nor UP may be sent.
	secondSlot, secondLeader, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("Leader before the second failover", "one leader agreed", err.Error(), false)
		return err
	}
	survivors2 := without(slots, secondSlot)
	killAt2 := time.Now()
	elect2, leader2, err := killLeader(ctx, cl, secondSlot, secondLeader, survivors2)
	rep.Check("Second failover: SIGKILL of the new leader", fmt.Sprintf("new leader < %s (NFA-REL-02)", failoverLimit),
		fmt.Sprintf("voter %d killed, %s, leader %s%s", secondSlot, electMs(elect2), leader2, errSuffix(err)),
		err == nil && elect2 < failoverLimit)
	if err != nil {
		return err
	}
	if err := waitUntil(ctx, killAt2.Add(2*interval)); err != nil {
		return err
	}
	alerts := 0
	for _, name := range names {
		c1, _ := notifyCount(env.Ntfy, killAt2, "DOWN: "+name)
		c2, _ := notifyCount(env.Ntfy, killAt2, "UP: "+name)
		alerts += c1 + c2
	}
	rep.Check("No alert during the second failover", "0 DOWN and 0 UP", fmt.Sprintf("%d", alerts), alerts == 0)
	stillUp, _ := waitMonitorStatus(ctx, cl.Client(survivors2[0]), ids, "UP", 30*time.Second)
	rep.Check("Monitors stay UP during the second failover", "all monitors UP", fmt.Sprintf("all UP: %v", stillUp), stillUp)
	if err := cl.Start(ctx, secondSlot, harness.StartOptions{}); err != nil {
		rep.Check("Restart "+cl.Names().Voter(secondSlot), "healthy again", err.Error(), false)
		return err
	}
	if err := requireLogin(ctx, env, cl, secondSlot); err != nil {
		return err
	}
	equal, diffs, _ = awaitSnapshots(ctx, cl, slots, 90*time.Second)
	rep.Check("Snapshots of all voters equal after the second restart", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)
	return nil
}

// without returns slots without the slots in xs.
func without(slots []int, xs ...int) []int {
	out := make([]int, 0, len(slots))
	for _, s := range slots {
		drop := false
		for _, x := range xs {
			drop = drop || s == x
		}
		if !drop {
			out = append(out, s)
		}
	}
	return out
}

// electMs formats an election time in milliseconds.
func electMs(d time.Duration) string {
	return fmt.Sprintf("election %d ms", d.Milliseconds())
}

// firstSince formats the offset of the first time in at from since, or "none".
func firstSince(at []time.Time, since time.Time) string {
	if len(at) == 0 {
		return "none"
	}
	return at[0].Sub(since).Round(time.Second).String()
}

// snapshotSummary renders the outcome of awaitSnapshots for a report row.
func snapshotSummary(equal bool, diffs []string) string {
	if equal {
		return "snapshots equal"
	}
	return fmt.Sprintf("%d differences: %v", len(diffs), firstN(diffs, 5))
}

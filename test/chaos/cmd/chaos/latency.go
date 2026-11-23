// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

// latencyDelay and latencyJitter are the egress shaping of every voter.
const (
	latencyDelay  = 100 * time.Millisecond
	latencyJitter = 10 * time.Millisecond
)

// latencyWriteLimit bounds one write under latency. It is the expectation of
// this scenario; the Pflichtenheft only asks for the failover time (NFA-REL-02).
const latencyWriteLimit = 5 * time.Second

func init() {
	register(Scenario{
		ID:    "latency",
		Label: "LATENCY",
		Title: "100 ms Latenz zwischen den Voter: Failover < 10 s, Schreiben funktioniert (NFA-REL-02)",
		Run:   runLatency,
	})
}

// runLatency adds 100 ms (plus or minus 10 ms) of egress delay to every voter
// and then kills the leader twice, each time with a new leader. Each failover
// must be agreed within 10 s, and writes must work under the latency.
func runLatency(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(n), false)
		return errors.New("latency needs three voter slots")
	}

	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := bringUp(ctx, env, cl, n); err != nil {
		return err
	}
	api := cl.Client(1)
	suffix := randomSuffix()
	if _, err := createHTTPMonitor(ctx, api, "latency-"+suffix, env.TargetURL("/latency/"+suffix), intervalSec, nil); err != nil {
		rep.Check("Monitor created", "201", err.Error(), false)
		return err
	}

	// Baseline and shaping check: voter 1 to voter 2 through its sidecar.
	before, err := medianTimeHTTP(ctx, cl, 1, 2)
	if err != nil {
		rep.Check("Baseline request from voter 1 to voter 2", "GET answers", err.Error(), false)
		return err
	}
	if err := applyLatency(ctx, cl, slots); err != nil {
		rep.Check("Latency on all voters", expectNetemQdiscInstalled, err.Error(), false)
		return err
	}
	var active []int
	for _, i := range slots {
		if ok, _ := cl.LatencyActive(ctx, i); ok {
			active = append(active, i)
		}
	}
	rep.Check("Latency on all voters", fmt.Sprintf("%d ms +-%d ms egress delay on every voter", latencyDelay.Milliseconds(), latencyJitter.Milliseconds()),
		fmt.Sprintf("qdisc active on voters %v", active), len(active) == n)
	delayed, err := medianTimeHTTP(ctx, cl, 1, 2)
	if err != nil {
		rep.Check("Request under latency", "GET answers", err.Error(), false)
		return err
	}
	rep.Check("Latency is effective between voters", "request at least 150 ms slower than the baseline",
		fmt.Sprintf("baseline %s, delayed %s", before.Round(time.Millisecond), delayed.Round(time.Millisecond)),
		delayed-before >= 150*time.Millisecond)

	// Writes under latency through a follower.
	leader, leaderID, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("One leader under latency", "identical leader ID", err.Error(), false)
		return err
	}
	writer := without(slots, leader)[0]
	var writeTimes []time.Duration
	writesOK := true
	for k := 1; k <= 3; k++ {
		t := time.Now()
		_, err := createHTTPMonitor(ctx, cl.Client(writer), fmt.Sprintf("latency-%s-w%d", suffix, k), env.TargetURL(fmt.Sprintf("/latency/%s/w%d", suffix, k)), intervalSec, nil)
		d := time.Since(t)
		writeTimes = append(writeTimes, d)
		if err != nil || d >= latencyWriteLimit {
			writesOK = false
		}
		if err != nil {
			rep.Info(fmt.Sprintf("Write %d under latency", k), "201", err.Error())
		}
	}
	rep.Check("Writes under latency via follower voter "+fmt.Sprint(writer), fmt.Sprintf("3 writes, each 201 within %s", latencyWriteLimit),
		fmt.Sprintf("durations %v", writeTimes), writesOK)

	// Failover 1: SIGKILL of the leader.
	survivors := without(slots, leader)
	elect1, leader1, err := killLeader(ctx, cl, leader, leaderID, survivors)
	rep.Check("Failover 1 under latency: new leader agreed", fmt.Sprintf("< %s (NFA-REL-02)", failoverLimit),
		fmt.Sprintf("voter %d killed, %s, leader %s%s", leader, electMs(elect1), leader1, errSuffix(err)),
		err == nil && elect1 < failoverLimit)
	if err != nil {
		return err
	}
	if err := writeAfterFailover(ctx, env, cl, survivors, suffix, 1, intervalSec); err != nil {
		return err
	}

	// Restart the killed voter. Its sidecar is new, so the latency is set again.
	if err := cl.Start(ctx, leader, harness.StartOptions{}); err != nil {
		rep.Check(prefixRestart+cl.Names().Voter(leader), expectHealthyAgain, err.Error(), false)
		return err
	}
	if err := setLatency(ctx, cl, leader); err != nil {
		rep.Check("Latency on restarted voter "+fmt.Sprint(leader), expectNetemQdiscInstalled, err.Error(), false)
		return err
	}
	if err := requireLogin(ctx, env, cl, leader); err != nil {
		return err
	}

	// Failover 2: SIGKILL of the new leader.
	second, _, err := leaderSlot(ctx, cl)
	secondID := leader1
	if err != nil {
		rep.Check("Leader before failover 2", "identical leader ID", err.Error(), false)
		return err
	}
	survivors2 := without(slots, second)
	elect2, leader2, err := killLeader(ctx, cl, second, secondID, survivors2)
	rep.Check("Failover 2 under latency: new leader agreed", fmt.Sprintf("< %s (NFA-REL-02)", failoverLimit),
		fmt.Sprintf("voter %d killed, %s, leader %s%s", second, electMs(elect2), leader2, errSuffix(err)),
		err == nil && elect2 < failoverLimit)
	if err != nil {
		return err
	}
	if err := writeAfterFailover(ctx, env, cl, survivors2, suffix, 2, intervalSec); err != nil {
		return err
	}
	if err := cl.Start(ctx, second, harness.StartOptions{}); err != nil {
		rep.Check(prefixRestart+cl.Names().Voter(second), expectHealthyAgain, err.Error(), false)
		return err
	}
	if err := setLatency(ctx, cl, second); err != nil {
		rep.Check("Latency on restarted voter "+fmt.Sprint(second), expectNetemQdiscInstalled, err.Error(), false)
		return err
	}
	if err := requireLogin(ctx, env, cl, second); err != nil {
		return err
	}
	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 120*time.Second)
	rep.Check("Snapshots of all voters equal after both restarts", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)

	// Remove the shaping again.
	for _, i := range slots {
		if err := cl.ClearLatency(ctx, i); err != nil {
			rep.Check("Clear latency on "+cl.Names().Voter(i), "netem removed", err.Error(), false)
			return err
		}
	}
	var still []int
	for _, i := range slots {
		if ok, _ := cl.LatencyActive(ctx, i); ok {
			still = append(still, i)
		}
	}
	rep.Check("Latency removed from all voters", "no netem qdisc left", fmt.Sprintf("still active on %v", still), len(still) == 0)
	return nil
}

// applyLatency sets the shaping on every given slot.
func applyLatency(ctx context.Context, cl *harness.Cluster, slots []int) error {
	for _, i := range slots {
		if err := setLatency(ctx, cl, i); err != nil {
			return err
		}
	}
	return nil
}

// setLatency sets the shaping on one voter.
func setLatency(ctx context.Context, cl *harness.Cluster, i int) error {
	return cl.Latency(ctx, i, harness.LatencySpec{Delay: latencyDelay, Jitter: latencyJitter})
}

// writeAfterFailover creates a monitor through the first survivor and checks
// that it is listed on every survivor.
func writeAfterFailover(ctx context.Context, env *Env, cl *harness.Cluster, survivors []int, suffix string, round, intervalSec int) error {
	rep := env.Rep
	s := survivors[0]
	name := fmt.Sprintf("latency-%s-after-%d", suffix, round)
	t := time.Now()
	m, err := createHTTPMonitor(ctx, cl.Client(s), name, env.TargetURL("/latency/"+suffix+"/after"+fmt.Sprint(round)), intervalSec, nil)
	d := time.Since(t)
	rep.Check(fmt.Sprintf("Write after failover %d via voter %d under latency", round, s),
		fmt.Sprintf("201 within %s", latencyWriteLimit), fmt.Sprintf("%s%s", elapsedMs(d), errSuffix(err)),
		err == nil && d < latencyWriteLimit)
	if err != nil {
		return err
	}
	var missing []int
	ok, _ := pollUntil(ctx, 30*time.Second, time.Second, func() (bool, error) {
		var err error
		missing, err = survivorsMissingMonitor(ctx, cl, survivors, m.ID)
		return err == nil && len(missing) == 0, err
	})
	rep.Check(fmt.Sprintf("Write after failover %d visible on all survivors", round), "listed on every survivor",
		fmt.Sprintf("complete: %v, missing on %v", ok, missing), ok)
	return nil
}

func survivorsMissingMonitor(ctx context.Context, cl *harness.Cluster, survivors []int, monitorID string) ([]int, error) {
	var missing []int
	for _, i := range survivors {
		mons, err := cl.Client(i).ListMonitors(ctx)
		if err != nil {
			return nil, err
		}
		if !containsMonitorID(mons, monitorID) {
			missing = append(missing, i)
		}
	}
	return missing, nil
}

func containsMonitorID(mons []ucapi.Monitor, monitorID string) bool {
	for _, mm := range mons {
		if mm.ID == monitorID {
			return true
		}
	}
	return false
}

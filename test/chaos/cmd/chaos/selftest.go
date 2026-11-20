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

// selftestPort is the cluster port of voters, used for reachability checks.
const selftestPort = 9443

// replicationLagLimit is the lag from which voter 1 counts a peer as cut off
// from replication. A healthy peer is replicated within one round.
const replicationLagLimit = 60 * time.Second

func init() {
	register(Scenario{
		ID:    "selftest",
		Label: "SELFTEST",
		Title: "Netzfehler-Werkzeuge pruefen (Isolate, Heal, Latency)",
		Run:   runSelftest,
	})
}

// runSelftest checks the fault injection itself, not the product: a voter
// isolated by iptables must be unreachable and fall behind in replication;
// after Heal it must catch up; a netem delay must show up in the RTT of the
// API and disappear when it is cleared.
func runSelftest(ctx context.Context, env *Env) error {
	rep := env.Rep
	if env.Cfg.Voters < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(env.Cfg.Voters), false)
		return errors.New("selftest needs three voter slots")
	}
	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := cl.Bootstrap(ctx, "admin"); err != nil {
		rep.Check("Voter 1 bootstraps", "admin set up", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 bootstraps", "admin set up", "admin created", true)
	for _, i := range []int{2, 3} {
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", "joined", true)
	}

	// A monitor gives every voter something to replicate.
	suffix := randomSuffix()
	interval := env.Cfg.Interval
	if _, err := cl.Client(1).CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:            "selftest-" + suffix,
		Type:            "http",
		Config:          map[string]any{"url": env.TargetURL("/selftest/" + suffix)},
		IntervalSeconds: int(interval / time.Second),
		Retries:         1,
		Assignment:      &ucapi.Assignment{Mode: "all"},
	}); err != nil {
		rep.Check("Monitor created", "201", err.Error(), false)
		return err
	}
	node3, err := cl.NodeID(ctx, 3)
	if err != nil {
		return err
	}
	mon, err := cl.Client(1).ListMonitors(ctx)
	if err != nil {
		return fmt.Errorf("list monitors: %w", err)
	}
	if len(mon) == 0 {
		return errors.New("list monitors: no monitor")
	}
	seen, _ := pollUntil(ctx, 2*interval+replicationRound, 2*time.Second, func() (bool, error) {
		counts, err := cl.ProducerCounts(ctx, 1, mon[0].ID, time.Now().Add(-2*interval), time.Now())
		if err != nil {
			return false, err
		}
		return counts[node3] > 0, nil
	})
	rep.Check("Voter 3 produces heartbeats seen by voter 1", "heartbeats of voter 3 on voter 1",
		fmt.Sprintf("seen: %v", seen), seen)
	if !seen {
		return errors.New("voter 3 produces no heartbeats")
	}

	// Isolate voter 3: the cluster port of voter 1 must be unreachable.
	if err := cl.Isolate(ctx, 3); err != nil {
		rep.Check("Isolate voter 3", "partition rules installed", err.Error(), false)
		return err
	}
	rep.Check("Isolate voter 3", "partition rules installed", "iptables DROP to voters 1 and 2", true)
	reach, err := cl.Reachable(ctx, 1, 3, selftestPort)
	if err != nil {
		rep.Check("Voter 1 cannot reach voter 3", "TCP connect fails", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 cannot reach voter 3", "TCP connect to 9443 fails", fmt.Sprintf("reachable: %v", reach), !reach)

	// Replication from voter 3 breaks: voter 1 reports lag or an error for it.
	isoStart := time.Now()
	var lagInfo string
	cut, _ := pollUntil(ctx, replicationLagLimit+2*replicationRound, 2*time.Second, func() (bool, error) {
		member, err := memberOf(ctx, cl, 1, node3)
		if err != nil {
			return false, err
		}
		lagInfo = describeLag(member)
		return member.ReplicationError != "" || (member.ReplicationLagMs != nil && time.Duration(*member.ReplicationLagMs)*time.Millisecond > replicationLagLimit), nil
	})
	rep.Check("Voter 1 sees replication cut off from voter 3",
		fmt.Sprintf("replication error or lag > %s", replicationLagLimit),
		fmt.Sprintf("%s after %s", lagInfo, time.Since(isoStart).Round(time.Second)), cut)

	// Heal: connectivity and replication recover.
	if err := cl.Heal(ctx, 3); err != nil {
		rep.Check("Heal voter 3", "partition rules removed", err.Error(), false)
		return err
	}
	healed, _ := pollUntil(ctx, 60*time.Second, time.Second, func() (bool, error) {
		return cl.Reachable(ctx, 1, 3, selftestPort)
	})
	rep.Check("Voter 1 reaches voter 3 after Heal", "TCP connect to 9443 succeeds", fmt.Sprintf("reachable: %v", healed), healed)

	caught, _ := pollUntil(ctx, replicationLagLimit+2*replicationRound, 2*time.Second, func() (bool, error) {
		member, err := memberOf(ctx, cl, 1, node3)
		if err != nil {
			return false, err
		}
		return member.ReplicationError == "" && member.ReplicationLagMs != nil &&
			time.Duration(*member.ReplicationLagMs)*time.Millisecond <= replicationLagLimit, nil
	})
	member, _ := memberOf(ctx, cl, 1, node3)
	rep.Check("Replication from voter 3 recovers", fmt.Sprintf("lag <= %s, no error", replicationLagLimit),
		describeLag(member), caught)

	// Latency: a 200 ms egress delay on voter 2 raises the time of requests
	// from voter 1 to voter 2 (container-to-container traffic, which netem
	// shapes). The host's round trip to the published port is not shaped in
	// this rootless setup; it is reported as information.
	before, err := medianTimeHTTP(ctx, cl, 1, 2)
	if err != nil {
		rep.Check("Baseline request from voter 1 to voter 2", "GET answers", err.Error(), false)
		return err
	}
	if err := cl.Latency(ctx, 2, harness.LatencySpec{Delay: 200 * time.Millisecond, Jitter: 10 * time.Millisecond}); err != nil {
		rep.Check("Latency on voter 2", "netem installed", err.Error(), false)
		return err
	}
	active, _ := cl.LatencyActive(ctx, 2)
	rep.Check("Latency on voter 2", "netem qdisc installed", fmt.Sprintf("qdisc active: %v", active), active)
	delayed, err := medianTimeHTTP(ctx, cl, 1, 2)
	if err != nil {
		rep.Check("Request with latency", "GET answers", err.Error(), false)
		return err
	}
	added := delayed - before
	rep.Check("Request from voter 1 to voter 2 with 200 ms netem delay",
		"at least 150 ms slower than the baseline",
		fmt.Sprintf("baseline %s, delayed %s, added %s",
			before.Round(time.Millisecond), delayed.Round(time.Millisecond), added.Round(time.Millisecond)),
		added >= 150*time.Millisecond)
	if host, err := medianHostHealth(ctx, cl, 2); err == nil {
		rep.Info("Host round trip to voter 2 with netem (published port)", "information only",
			fmt.Sprintf("%.2f ms; host traffic is not shaped by eth0 netem here", float64(host.Microseconds())/1000))
	}

	if err := cl.ClearLatency(ctx, 2); err != nil {
		rep.Check("Clear latency on voter 2", "netem removed", err.Error(), false)
		return err
	}
	active, _ = cl.LatencyActive(ctx, 2)
	cleared, err := medianTimeHTTP(ctx, cl, 1, 2)
	if err != nil {
		rep.Check("Request after clearing latency", "GET answers", err.Error(), false)
		return err
	}
	rep.Check("Clear latency on voter 2", "netem removed, request back near the baseline (within 100 ms)",
		fmt.Sprintf("qdisc active: %v, request %s, baseline %s", active, cleared.Round(time.Millisecond), before.Round(time.Millisecond)),
		!active && cleared-before < 100*time.Millisecond)
	return nil
}

// medianTimeHTTP returns the median of five timed requests from voter from
// to voter to.
func medianTimeHTTP(ctx context.Context, cl *harness.Cluster, from, to int) (time.Duration, error) {
	var samples []time.Duration
	for n := 0; n < 5; n++ {
		d, err := cl.TimeHTTP(ctx, from, to, "/api/v1/health")
		if err != nil {
			return 0, err
		}
		samples = append(samples, d)
	}
	return medianDuration(samples), nil
}

// medianHostHealth returns the median of five host round trips to voter i.
func medianHostHealth(ctx context.Context, cl *harness.Cluster, i int) (time.Duration, error) {
	var samples []time.Duration
	for n := 0; n < 5; n++ {
		start := time.Now()
		if _, err := cl.Client(i).Health(ctx); err != nil {
			return 0, err
		}
		samples = append(samples, time.Since(start))
	}
	return medianDuration(samples), nil
}

// memberOf returns the member with ID id as seen by voter viewer.
func memberOf(ctx context.Context, cl *harness.Cluster, viewer int, id string) (ucapi.ClusterNode, error) {
	list, err := cl.Members(ctx, viewer)
	if err != nil {
		return ucapi.ClusterNode{}, err
	}
	for _, n := range list.Nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return ucapi.ClusterNode{}, fmt.Errorf("node %s is not a member", id)
}

func describeLag(n ucapi.ClusterNode) string {
	lag := "no lag value"
	if n.ReplicationLagMs != nil {
		lag = fmt.Sprintf("lag %s", (time.Duration(*n.ReplicationLagMs) * time.Millisecond).Round(time.Second))
	}
	if n.ReplicationError != "" {
		return lag + ", error: " + n.ReplicationError
	}
	return lag + ", no error"
}

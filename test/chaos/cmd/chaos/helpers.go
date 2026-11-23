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

// failoverLimit is the time from the loss of the leader to a leader that all
// survivors agree on (NFA-REL-02, AS-03).
const (
	failoverLimit               = 10 * time.Second
	expectListedAsVoterByLeader = "listed as voter by the leader"
	prefixDown                  = "DOWN: "
	prefixRestart               = "Restart "
	expectHealthyAgain          = "healthy again"
	expectDeliveredToNtfy       = "delivered to ntfy"
	expectNetemQdiscInstalled   = "netem qdisc installed"
)

// bringUp starts an n-voter cluster: voter 1 bootstraps, voters 2..n join one
// after another, and each joined voter must answer logins.
func bringUp(ctx context.Context, env *Env, cl *harness.Cluster, n int) error {
	rep := env.Rep
	if err := cl.Bootstrap(ctx, "admin"); err != nil {
		rep.Check("Voter 1 bootstraps, admin set up", "setup 201, admin logged in", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 bootstraps, admin set up", "setup 201, admin logged in",
		"admin created on "+cl.Names().Voter(1), true)
	for i := 2; i <= n; i++ {
		started := time.Now()
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d joins", i), expectListedAsVoterByLeader, err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Voter %d joins", i), expectListedAsVoterByLeader,
			fmt.Sprintf("joined after %s", time.Since(started).Round(time.Second)), true)
		if err := requireLogin(ctx, env, cl, i); err != nil {
			return err
		}
	}
	return nil
}

// voterRange returns the slots 1..n.
func voterRange(n int) []int {
	out := make([]int, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, i)
	}
	return out
}

// leaderSlot returns the slot and the node ID of the leader that the running
// voters agree on. The slot is the running voter whose node ID is the leader.
func leaderSlot(ctx context.Context, cl *harness.Cluster) (int, string, error) {
	running, err := cl.RunningVoters(ctx)
	if err != nil {
		return 0, "", err
	}
	id, err := cl.LeaderOf(ctx, running)
	if err != nil {
		return 0, "", err
	}
	for _, i := range running {
		nid, err := cl.NodeID(ctx, i)
		if err == nil && nid == id {
			return i, id, nil
		}
	}
	return 0, id, fmt.Errorf("leader %s is not a running voter", id)
}

// nodeIDs returns the node IDs of the given slots, read from each voter.
func nodeIDs(ctx context.Context, cl *harness.Cluster, slots []int) (map[int]string, error) {
	out := make(map[int]string, len(slots))
	for _, i := range slots {
		id, err := cl.NodeID(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("node ID of %s: %w", cl.Names().Voter(i), err)
		}
		out[i] = id
	}
	return out, nil
}

// awaitLeaderChange polls until the given voters agree on a leader other than
// oldLeader. It returns the new leader ID.
func awaitLeaderChange(ctx context.Context, cl *harness.Cluster, voters []int, oldLeader string, timeout time.Duration) (string, error) {
	var leader string
	var last error
	ok, _ := pollUntil(ctx, timeout, 100*time.Millisecond, func() (bool, error) {
		id, err := cl.LeaderOf(ctx, voters)
		if err != nil {
			last = err
			return false, nil
		}
		if id == "" || id == oldLeader {
			last = fmt.Errorf("leader still %q", id)
			return false, nil
		}
		leader = id
		return true, nil
	})
	if !ok {
		if last == nil {
			last = errors.New("no change")
		}
		return "", fmt.Errorf("no new leader within %s: %w", timeout, last)
	}
	return leader, nil
}

// killLeader SIGKILLs voter slot and measures the time until the survivors
// agree on a new leader. The time starts before the kill.
func killLeader(ctx context.Context, cl *harness.Cluster, slot int, oldLeader string, survivors []int) (time.Duration, string, error) {
	killAt := time.Now()
	if err := cl.Kill(ctx, slot); err != nil {
		return 0, "", fmt.Errorf("kill %s: %w", cl.Names().Voter(slot), err)
	}
	leader, err := awaitLeaderChange(ctx, cl, survivors, oldLeader, 30*time.Second)
	return time.Since(killAt), leader, err
}

// snapshotDiffsOf compares the normalized snapshots of the given voters with
// the first one. An empty result means all voters hold the same data.
func snapshotDiffsOf(ctx context.Context, cl *harness.Cluster, voters []int) ([]string, error) {
	if len(voters) < 2 {
		return nil, nil
	}
	base, err := cl.Snapshot(ctx, voters[0])
	if err != nil {
		return nil, err
	}
	var diffs []string
	for _, i := range voters[1:] {
		other, err := cl.Snapshot(ctx, i)
		if err != nil {
			return nil, err
		}
		for _, d := range harness.CompareSnapshots(base, other) {
			diffs = append(diffs, fmt.Sprintf("voter %d vs %d: %s", i, voters[0], d))
		}
	}
	return diffs, nil
}

// awaitSnapshots polls until the snapshots of voters are equal.
func awaitSnapshots(ctx context.Context, cl *harness.Cluster, voters []int, timeout time.Duration) (bool, []string, error) {
	var diffs []string
	var last error
	ok, _ := pollUntil(ctx, timeout, 2*time.Second, func() (bool, error) {
		d, err := snapshotDiffsOf(ctx, cl, voters)
		if err != nil {
			last = err
			return false, nil
		}
		diffs = d
		return len(d) == 0, nil
	})
	if !ok && len(diffs) == 0 && last != nil {
		return false, []string{last.Error()}, nil
	}
	return ok, diffs, nil
}

// heartbeatKeys returns the heartbeats of one producer for one monitor, as
// seen by voter viewer in [from, to], keyed by timestamp and producer.
func heartbeatKeys(ctx context.Context, cl *harness.Cluster, viewer int, monitorID, producer string, from, to time.Time) (map[string]bool, error) {
	hbs, err := cl.Client(viewer).ListHeartbeats(ctx, monitorID, ucapi.HeartbeatQuery{From: from, To: to, Node: producer})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(hbs))
	for _, h := range hbs {
		out[h.TS.UTC().Format(time.RFC3339Nano)+"|"+h.NodeID] = true
	}
	return out, nil
}

// missingKeys returns the keys of want that are not in have, sorted by the
// number only (for reporting).
func missingKeys(want, have map[string]bool) int {
	n := 0
	for k := range want {
		if !have[k] {
			n++
		}
	}
	return n
}

// createHTTPMonitor creates an HTTP monitor against the fake target with
// assignment all, retries 1 and the given channels.
func createHTTPMonitor(ctx context.Context, c *ucapi.Client, name, url string, intervalSec int, channels []string) (ucapi.Monitor, error) {
	if channels == nil {
		channels = []string{}
	}
	return c.CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:                   name,
		Type:                   "http",
		Config:                 map[string]any{"url": url},
		IntervalSeconds:        intervalSec,
		Retries:                1,
		Assignment:             &ucapi.Assignment{Mode: "all"},
		NotificationChannelIDs: channels,
	})
}

// waitMonitorStatus polls until every monitor reports status.
func waitMonitorStatus(ctx context.Context, c *ucapi.Client, ids []string, status string, timeout time.Duration) (bool, error) {
	return pollUntil(ctx, timeout, 2*time.Second, func() (bool, error) {
		for _, id := range ids {
			m, err := c.GetMonitor(ctx, id)
			if err != nil {
				return false, err
			}
			if m.Status != status {
				return false, nil
			}
		}
		return true, nil
	})
}

// waitHeartbeatsOfAll polls until every monitor has heartbeats of every slot
// (as seen by voter viewer) since since.
func waitHeartbeatsOfAll(ctx context.Context, cl *harness.Cluster, viewer int, monitorIDs, producers []string, since time.Time, timeout time.Duration) (bool, error) {
	return pollUntil(ctx, timeout, 2*time.Second, func() (bool, error) {
		for _, id := range monitorIDs {
			counts, err := cl.ProducerCounts(ctx, viewer, id, since, time.Now())
			if err != nil {
				return false, err
			}
			for _, p := range producers {
				if counts[p] == 0 {
					return false, nil
				}
			}
		}
		return true, nil
	})
}

// elapsedMs formats a duration in whole milliseconds.
func elapsedMs(d time.Duration) string {
	return fmt.Sprintf("%d ms", d.Milliseconds())
}

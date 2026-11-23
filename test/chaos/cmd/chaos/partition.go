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

// partitionPort is the cluster port used for the reachability check.
const (
	partitionPort   = 9443
	prefixPartition = "partition-"
	pathPartition   = "/partition/"
)

func init() {
	register(Scenario{
		ID:    "partition",
		Label: "PARTITION",
		Title: "Leader isoliert (2|1): Mehrheit wählt neu, Minderheit schreibt nicht, Heilung ohne Duplikate",
		Run:   runPartition,
	})
}

// runPartition isolates the leader of a three-voter cluster in both
// directions. The majority must elect a new leader within 10 s and keep
// writing. The isolated voter must not commit writes and answers reads, maybe
// stale. After the heal it steps down, the cluster agrees on one leader and
// the data is equal on all voters. The target fails during the partition: the
// DOWN alert must be sent, and the heal must not repeat it. Alerts sent by the
// minority cannot be told apart from the majority's, so the count during the
// partition is recorded as information.
func runPartition(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(n), false)
		return errors.New("partition needs three voter slots")
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
	ch, err := api.CreateChannel(ctx, "partition-ntfy-"+suffix, env.NtfyURL(prefixPartition+suffix), []string{"down", "up"})
	if err != nil {
		rep.Check("Notification channel created", "201", err.Error(), false)
		return err
	}
	env.Target.SetFailing(false)
	mon, err := createHTTPMonitor(ctx, api, prefixPartition+suffix, env.TargetURL(pathPartition+suffix), intervalSec, []string{ch.ID})
	if err != nil {
		rep.Check("Monitor created", "201", err.Error(), false)
		return err
	}
	up, _ := waitMonitorStatus(ctx, api, []string{mon.ID}, "UP", 2*interval+replicationRound)
	rep.Check("Monitor UP before the partition", "UP", fmt.Sprintf("UP: %v", up), up)
	if !up {
		return errors.New("monitor does not come up")
	}

	leader, leaderID, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("One leader before the partition", "identical leader ID", err.Error(), false)
		return err
	}
	majority := without(slots, leader)
	mw := majority[0]
	rep.Check("One leader before the partition", "identical leader ID on all voters",
		fmt.Sprintf("leader voter %d (node %s), majority %v", leader, leaderID, majority), true)

	t0, newLeader, err := partitionIsolateAndElect(ctx, env, cl, leader, mw, leaderID, majority)
	if err != nil {
		return err
	}

	minName, err := partitionVerifyReadsAndWrites(ctx, env, cl, leader, mw, suffix, intervalSec)
	if err != nil {
		return err
	}

	dueAt := t0.Add(time.Duration(as03Retries+1)*interval + 20*time.Second)
	if err := waitUntil(ctx, dueAt); err != nil {
		return err
	}
	downBeforeHeal, _ := notifyCount(env.Ntfy, t0, prefixDown+mon.Name)
	rep.Info("DOWN notifications during the partition", "at least 1 (majority); exactly 1 expected",
		fmt.Sprintf("%d DOWN before the heal", downBeforeHeal))

	return partitionHealAndVerify(ctx, env, cl, slots, leader, newLeader, minName, mon, t0, interval)
}

func partitionIsolateAndElect(ctx context.Context, env *Env, cl *harness.Cluster, leader, mw int, leaderID string, majority []int) (time.Time, string, error) {
	rep := env.Rep
	t0 := time.Now()
	env.Target.SetFailing(true)
	isoAt := time.Now()
	if err := cl.Isolate(ctx, leader); err != nil {
		rep.Check("Isolate the leader "+cl.Names().Voter(leader), "partition rules installed", err.Error(), false)
		return time.Time{}, "", err
	}
	rep.Check("Isolate the leader "+cl.Names().Voter(leader), "DROP rules to the other voters, both directions",
		fmt.Sprintf("rules installed at +%s", isoAt.Sub(t0).Round(time.Millisecond)), true)
	reach, err := cl.Reachable(ctx, mw, leader, partitionPort)
	rep.Check("Majority cannot reach the isolated leader", "TCP connect fails",
		fmt.Sprintf("reachable: %v%s", reach, errSuffix(err)), err == nil && !reach)

	newLeader, err := awaitLeaderChange(ctx, cl, majority, leaderID, 30*time.Second)
	elect := time.Since(isoAt)
	rep.Check("Majority elects a new leader", fmt.Sprintf("< %s (NFA-REL-02)", failoverLimit),
		fmt.Sprintf("%s, leader %s%s", electMs(elect), newLeader, errSuffix(err)), err == nil && elect < failoverLimit)
	if err != nil {
		return time.Time{}, "", err
	}
	return t0, newLeader, nil
}

func partitionVerifyReadsAndWrites(ctx context.Context, env *Env, cl *harness.Cluster, leader, mw int, suffix string, intervalSec int) (string, error) {
	rep := env.Rep
	mStart := time.Now()
	majMon, err := createHTTPMonitor(ctx, cl.Client(mw), prefixPartition+suffix+"-majority", env.TargetURL(pathPartition+suffix+"/majority"), intervalSec, nil)
	dMaj := time.Since(mStart)
	rep.Check("Write through the majority (voter "+fmt.Sprint(mw)+")", "201",
		fmt.Sprintf("%s%s", elapsedMs(dMaj), errSuffix(err)), err == nil)
	if err != nil {
		return "", err
	}

	lc := cl.Client(leader)
	minName := prefixPartition + suffix + "-minority"
	wStart := time.Now()
	minMonRes, werr := createHTTPMonitor(ctx, lc, minName, env.TargetURL(pathPartition+suffix+"/minority"), intervalSec, nil)
	dMin := time.Since(wStart)
	outcome := formatWriteOutcome(werr)
	rep.Check("Write through the isolated old leader", "rejected (503 or timeout), not committed",
		fmt.Sprintf("%s after %s (created id %q)", outcome, dMin.Round(100*time.Millisecond), minMonRes.ID),
		werr != nil)

	minorityMons, rerr := lc.ListMonitors(ctx)
	seesMajority := containsMonitorID(minorityMons, majMon.ID)
	rep.Check("Read on the isolated old leader answers", "list answers (stale data allowed)",
		fmt.Sprintf("%d monitors, majority write visible: %v%s", len(minorityMons), seesMajority, errSuffix(rerr)), rerr == nil)
	if list, err := lc.ListClusterNodes(ctx); err == nil {
		rep.Info("Isolated old leader's view of the leader", "information only",
			fmt.Sprintf("leader ID %s, its own leader flag: %v", list.LeaderID, selfIsLeader(list)))
	}
	return minName, nil
}

func formatWriteOutcome(err error) string {
	if err == nil {
		return "no error"
	}
	if code := ucapi.StatusCode(err); code != 0 {
		return fmt.Sprintf("HTTP %d", code)
	}
	return "error: " + err.Error()
}

func partitionHealAndVerify(ctx context.Context, env *Env, cl *harness.Cluster, slots []int, leader int, newLeader, minName string, mon ucapi.Monitor, t0 time.Time, interval time.Duration) error {
	rep := env.Rep
	lc := cl.Client(leader)
	healAt := time.Now()
	if err := cl.Heal(ctx, leader); err != nil {
		rep.Check("Heal the isolated leader", "partition rules removed", err.Error(), false)
		return err
	}
	agreed, _ := pollUntil(ctx, 60*time.Second, 250*time.Millisecond, func() (bool, error) {
		id, err := cl.LeaderOf(ctx, slots)
		return err == nil && id == newLeader, nil
	})
	rep.Check("Heal: all voters agree on the new leader", "one leader on all voters, old leader stepped down",
		fmt.Sprintf("agreed: %v after %s", agreed, time.Since(healAt).Round(time.Millisecond)), agreed)
	if list, err := lc.ListClusterNodes(ctx); err == nil {
		rep.Check("Old leader after the heal", "not the leader any more",
			fmt.Sprintf("leader ID %s, own leader flag: %v%s", list.LeaderID, selfIsLeader(list), errSuffix(err)),
			list.LeaderID == newLeader && !selfIsLeader(list))
	}

	present := votersHavingMonitorName(ctx, cl, slots, minName)
	rep.Check("Rejected write absent on all voters", "monitor of the rejected write not listed",
		fmt.Sprintf("listed on %v", present), len(present) == 0)

	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 120*time.Second)
	rep.Check("Snapshots of all voters equal after the heal", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)

	if err := waitUntil(ctx, time.Now().Add(interval)); err != nil {
		return err
	}
	downAfterHeal, _ := notifyCount(env.Ntfy, healAt, prefixDown+mon.Name)
	rep.Check("No DOWN repeated after the heal", "0 DOWN after the heal",
		fmt.Sprintf("%d", downAfterHeal), downAfterHeal == 0)
	downTotal, downAt := notifyCount(env.Ntfy, t0, prefixDown+mon.Name)
	rep.Check("DOWN notification delivered during the partition", "at least 1",
		fmt.Sprintf("%d DOWN, first +%s", downTotal, firstSince(downAt, t0)), downTotal >= 1)
	rep.Info("DOWN notifications in total", "exactly 1 expected; more would be a duplicate (minority or majority, not attributable)",
		fmt.Sprintf("%d", downTotal))

	env.Target.SetFailing(false)
	recovered, _ := waitMonitorStatus(ctx, cl.Client(1), []string{mon.ID}, "UP", 2*interval+30*time.Second)
	rep.Check("Monitor UP again after the heal", "UP", fmt.Sprintf("UP: %v", recovered), recovered)
	if err := waitUntil(ctx, time.Now().Add(interval)); err != nil {
		return err
	}
	upTotal, upAt := notifyCount(env.Ntfy, t0, "UP: "+mon.Name)
	rep.Info("UP notifications after the target recovers", "exactly 1 expected",
		fmt.Sprintf("%d UP, first +%s", upTotal, firstSince(upAt, t0)))
	return nil
}

func votersHavingMonitorName(ctx context.Context, cl *harness.Cluster, slots []int, name string) []string {
	var present []string
	for _, i := range slots {
		mons, err := cl.Client(i).ListMonitors(ctx)
		if err != nil {
			present = append(present, fmt.Sprintf("voter %d unreadable", i))
			continue
		}
		for _, m := range mons {
			if m.Name == name {
				present = append(present, fmt.Sprintf("voter %d", i))
			}
		}
	}
	return present
}

// selfIsLeader reports whether the answering node lists itself as leader.
func selfIsLeader(list ucapi.ClusterNodeList) bool {
	for _, n := range list.Nodes {
		if n.Self && n.Leader {
			return true
		}
	}
	return false
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// replaceSlot is the voter whose node is replaced.
const replaceSlot = 3

// replaceHTTP409 matches a standalone 409 (not part of a UUID or a number).
var replaceHTTP409 = regexp.MustCompile(`\b409\b`)

func init() {
	register(Scenario{
		ID:    "replace",
		Label: "REPLACE",
		Title: "Knoten ersetzen und Datenverzeichnis löschen (FA-CLU-08)",
		Run:   runReplace,
	})
}

// runReplace checks the replacement of a node (FA-CLU-08, docs/betrieb/cluster.md
// "Node ersetzen"). Voter 3 is stopped and its data is wiped. A join under the
// same address must be refused while the old member is still listed. After
// the old member is removed through a follower, a join with a new token gives
// a new node ID. A copy of the old data directory must not be able to rejoin.
func runReplace(ctx context.Context, env *Env) error {
	rep := env.Rep
	interval := env.Cfg.Interval
	if env.Cfg.Voters < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(env.Cfg.Voters), false)
		return errors.New("replace needs three voter slots")
	}
	all := []int{1, 2, 3}

	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := cl.Bootstrap(ctx, "admin"); err != nil {
		rep.Check("Voter 1 bootstraps", "admin set up", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 bootstraps", "admin set up", "admin created on "+cl.Names().Voter(1), true)

	suffix := randomSuffix()
	ch, err := opsCreateChannel(ctx, env, cl.Client(1), "replace-ntfy-"+suffix, "replace-"+suffix)
	if err != nil {
		rep.Check("Notification channel with test delivery", "delivered to ntfy", err.Error(), false)
		return err
	}
	rep.Check("Notification channel with test delivery", "delivered to ntfy", "channel "+ch.ID, true)
	m, err := opsHTTPMonitor(ctx, env, cl.Client(1), "replace-"+suffix, "/replace/"+suffix, []string{ch.ID})
	if err != nil {
		rep.Check("HTTP monitor created", "201, assignment all", err.Error(), false)
		return err
	}
	monIDs := []string{m.ID}

	for _, i := range []int{2, 3} {
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", err.Error(), false)
			return err
		}
		if err := opsJoinLogin(ctx, env, cl, i); err != nil {
			rep.Check(fmt.Sprintf("Login on voter %d after join", i), "login answers", err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", "joined", true)
	}

	nodes, err := opsNodeIDs(ctx, cl, all)
	if err != nil {
		rep.Check("Node IDs of the voters", "each voter lists itself", err.Error(), false)
		return err
	}
	producers := []string{nodes[1], nodes[2], nodes[3]}
	ok, err := pollUntil(ctx, 2*interval+2*opsReplicationRound, 2*time.Second, func() (bool, error) {
		return opsAllProducers(ctx, cl, all, monIDs, producers, time.Now().Add(-2*interval))
	})
	rep.Check("Heartbeats of all voters on every voter before the replacement", "all producers on all voters",
		fmt.Sprintf("complete: %v%s", ok, as09ErrSuffix(err)), ok)
	if !ok {
		return errors.New("baseline heartbeats incomplete")
	}
	snapOK, diffs, err := opsAwaitSnapshots(ctx, cl, all, 60*time.Second)
	rep.Check("Identical data before the replacement", "normalized snapshots equal",
		fmt.Sprintf("equal: %v, %d differences%s", snapOK, len(diffs), as09ErrSuffix(err)), snapOK)

	oldID := nodes[replaceSlot]
	leaderID, err := cl.LeaderOf(ctx, all)
	if err != nil {
		rep.Check("One leader before the replacement", "identical leader ID", err.Error(), false)
		return err
	}
	remover := 0
	for _, i := range []int{1, 2} {
		if nodes[i] != leaderID {
			remover = i
			break
		}
	}
	if remover == 0 {
		return errors.New("no follower among voters 1 and 2")
	}

	// Stop, copy the data for the stale-start check, then wipe.
	volume := cl.Names().Volume(replaceSlot)
	backupVol := env.Cfg.Prefix + "-bak-v3-data"
	staleName := env.Cfg.Prefix + "-stale-v3"
	defer func() {
		if err := opsCleanup(ctx, cl, []string{staleName}, []string{backupVol}); err != nil {
			env.Log.Error("cleanup replace copies", "err", err)
		}
	}()
	if err := cl.Stop(ctx, replaceSlot); err != nil {
		rep.Check(fmt.Sprintf("Stop voter %d", replaceSlot), "stopped", err.Error(), false)
		return err
	}
	if err := cl.CopyVolume(ctx, volume, backupVol); err != nil {
		rep.Check("Copy the data of the old node (for the stale-start check)", "copy created", err.Error(), false)
		return err
	}
	rep.Info("Data of the old node copied before the wipe", "copy for the stale-start check",
		fmt.Sprintf("old node %s, copy volume %s", oldID, backupVol))
	if err := cl.WipeData(ctx, replaceSlot); err != nil {
		rep.Check(fmt.Sprintf("Wipe data of voter %d", replaceSlot), "container and data removed, empty volume", err.Error(), false)
		return err
	}
	rep.Check(fmt.Sprintf("Wipe data of voter %d", replaceSlot), "container and data removed, empty volume", "wiped", true)

	// A join under the same address while the old member is still listed.
	attempt, err := cl.TryJoin(ctx, replaceSlot, 90*time.Second)
	if err != nil {
		rep.Check("Join with the same address while the old member is listed", "refused", err.Error(), false)
		return err
	}
	reason := replaceReason(attempt.Logs)
	rep.Check("Join with the same address while the old member is listed",
		fmt.Sprintf("refused, the error names the old node ID %s (the join runs over the cluster port: no HTTP 409 is expected there)", oldID),
		fmt.Sprintf("exited: %v; old ID in log: %v; HTTP 409 token in log: %v; message: %s",
			attempt.Exited, strings.Contains(attempt.Logs, oldID), replaceHTTP409.MatchString(attempt.Logs), reason),
		attempt.Exited && strings.Contains(attempt.Logs, oldID))
	if !attempt.Exited {
		return errors.New("the join under the same address did not stop")
	}

	// The failed attempt may have written a node ID; start over with an empty directory.
	if err := cl.WipeData(ctx, replaceSlot); err != nil {
		rep.Check("Empty data directory for the second join", "empty volume", err.Error(), false)
		return err
	}

	// Remove the old member through a follower (exercises the forwarding).
	if err := opsRefreshLogins(ctx, cl, []int{remover}); err != nil {
		rep.Check("Login on the follower", "login answers", err.Error(), false)
		return err
	}
	removal, err := cl.Client(remover).RemoveClusterNode(ctx, oldID)
	rep.Check(fmt.Sprintf("Remove the old member %s through follower voter %d", oldID, remover),
		"200, old member removed, voters left reported",
		fmt.Sprintf("votersLeft %d, warnings %v%s", removal.VotersLeft, removal.Warnings, as09ErrSuffix(err)), err == nil)
	if err != nil {
		return err
	}

	// A copy of the old data must not rejoin: its certificate is revoked.
	if err := cl.StartStale(ctx, staleName, backupVol, replaceSlot); err != nil {
		rep.Check("Start the copy of the old data", "container starts", err.Error(), false)
		return err
	}
	if err := opsWaitUntil(ctx, time.Now().Add(45*time.Second)); err != nil {
		return err
	}
	staleRunning, _ := cl.ContainerRunning(ctx, staleName)
	staleLogs, err := cl.ContainerLogs(ctx, staleName)
	if err != nil {
		rep.Check("Logs of the copy of the old node", "readable", err.Error(), false)
		return err
	}
	listed, err := cl.Members(ctx, 1)
	if err != nil {
		rep.Check("Member list after the stale start", "answers", err.Error(), false)
		return err
	}
	_, oldListed := as09Find(listed, oldID)
	rep.Info("Copy of the old node started", "rejected at the cluster port (revoked certificate)",
		fmt.Sprintf("still running after 45 s: %v; old ID listed: %v; voters %d; log: %s",
			staleRunning, oldListed, opsVoterCount(listed), replaceReason(staleLogs)))
	rep.Check("Copy of the old node does not rejoin", "not listed, cluster unchanged",
		fmt.Sprintf("old ID listed: %v, voters %d", oldListed, opsVoterCount(listed)),
		!oldListed && opsVoterCount(listed) == 2)
	if err := opsCleanup(ctx, cl, []string{staleName}, nil); err != nil {
		rep.Info("Remove the stale container", "removed", err.Error())
	}

	// Join again with a new token and an empty directory.
	joinStart := time.Now()
	if err := cl.Join(ctx, replaceSlot); err != nil {
		rep.Check("Second join with a new token", "listed as voter", err.Error(), false)
		return err
	}
	if err := opsJoinLogin(ctx, env, cl, replaceSlot); err != nil {
		rep.Check("Login on voter 3 after the second join", "login answers", err.Error(), false)
		return err
	}
	newID, err := cl.NodeID(ctx, replaceSlot)
	if err != nil {
		rep.Check("Node ID of the new voter 3", "lists itself", err.Error(), false)
		return err
	}
	rep.Check("Second join with a new token", "listed as voter, new node ID",
		fmt.Sprintf("joined after %s, new ID %s (old %s)", time.Since(joinStart).Round(time.Second), newID, oldID),
		newID != oldID)

	list, err := cl.Members(ctx, 1)
	if err != nil {
		rep.Check("Member list after the replacement", "answers", err.Error(), false)
		return err
	}
	_, oldStill := as09Find(list, oldID)
	_, newListed := as09Find(list, newID)
	rep.Check("Member list after the replacement", "3 voters, new ID listed, old ID gone",
		fmt.Sprintf("%d voters, new listed: %v, old listed: %v, warnings %v", opsVoterCount(list), newListed, oldStill, list.Warnings),
		opsVoterCount(list) == 3 && newListed && !oldStill)

	// Heartbeats of the new node on the others, and identical data.
	newNodes := map[int]string{1: nodes[1], 2: nodes[2], 3: newID}
	newProducers := []string{newNodes[1], newNodes[2], newNodes[3]}
	ok, err = pollUntil(ctx, 2*interval+2*opsReplicationRound, 2*time.Second, func() (bool, error) {
		return opsAllProducers(ctx, cl, all, monIDs, newProducers, time.Now().Add(-2*interval))
	})
	rep.Check("Heartbeats of the new voter 3 on every voter", "all producers on all voters",
		fmt.Sprintf("complete: %v%s", ok, as09ErrSuffix(err)), ok)
	snapOK, diffs, err = opsAwaitSnapshots(ctx, cl, all, 60*time.Second)
	measured := "snapshots of voters 1, 2, 3 equal"
	if !snapOK {
		measured = fmt.Sprintf("%d differences: %v%s", len(diffs), as09First(diffs, 5), as09ErrSuffix(err))
	}
	rep.Check("Identical data after the replacement", "normalized snapshots equal", measured, snapOK)

	desc, visible, err := opsWriteVisible(ctx, cl, env, 3, []int{1, 2}, "replace-"+suffix+"-via-v3", "/replace/"+suffix+"/via-v3")
	if err != nil {
		rep.Check("Write through the new voter 3", "201, visible within 5 s", err.Error(), false)
		return err
	}
	rep.Check("Write through the new voter 3", "visible on voters 1 and 2 within 5 s", desc, visible)
	return nil
}

// replaceReason returns the first log line that names an error or the
// refusal, shortened for the report. Logs are JSON lines or text.
func replaceReason(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		l := strings.ToLower(line)
		if strings.Contains(l, "already used") || strings.Contains(l, "error") || strings.Contains(l, "refus") || strings.Contains(l, "revoked") || strings.Contains(l, "not valid") {
			line = strings.TrimSpace(line)
			if len(line) > 300 {
				line = line[:300] + "..."
			}
			return line
		}
	}
	return "no error line"
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

// recFreshWait bounds how long the write probes wait for the 503 of a cluster
// without quorum.
const recFreshWait = 60 * time.Second

func init() {
	register(Scenario{
		ID:    "recovery",
		Label: "RECOVERY",
		Title: "Runbook Quorumverlust, admin recover und Admin-Passwort (ADR 0019)",
		Run:   runRecovery,
	})
}

// recRaftInfo is the JSON of "admin raft-info --json" (internal/cluster RaftInfo).
type recRaftInfo struct {
	NodeID       string `json:"nodeId"`
	LastIndex    uint64 `json:"lastIndex"`
	LastTerm     uint64 `json:"lastTerm"`
	AppliedIndex uint64 `json:"appliedIndex"`
	CommandLevel uint32 `json:"commandLevel"`
	Servers      []struct {
		ID      string `json:"id"`
		Address string `json:"address"`
		Voter   bool   `json:"voter"`
	} `json:"servers"`
}

// runRecovery follows docs/betrieb/recovery.md with three voters that hold
// data: two voters die (quorum lost), the survivors are inspected with
// "admin raft-info", the most up-to-date one runs "admin recover", the
// other two rejoin as new nodes, and a copy of an old data directory must not
// disturb the cluster. Finally the admin password is reset in a running voter.
func runRecovery(ctx context.Context, env *Env) error {
	rep := env.Rep
	interval := env.Cfg.Interval
	if env.Cfg.Voters < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(env.Cfg.Voters), false)
		return errors.New("recovery needs three voter slots")
	}
	all := []int{1, 2, 3}
	image := env.Cfg.Image
	prefix := env.Cfg.Prefix

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
	ch, err := opsCreateChannel(ctx, env, cl.Client(1), "recovery-ntfy-"+suffix, "recovery-"+suffix)
	if err != nil {
		rep.Check("Notification channel with test delivery", "delivered to ntfy", err.Error(), false)
		return err
	}
	rep.Check("Notification channel with test delivery", "delivered to ntfy", "channel "+ch.ID, true)
	var monIDs, monNames []string
	for n := 1; n <= 2; n++ {
		name := fmt.Sprintf("recovery-%s-m%d", suffix, n)
		m, err := opsHTTPMonitor(ctx, env, cl.Client(1), name, fmt.Sprintf("/recovery/%s/m%d", suffix, n), []string{ch.ID})
		if err != nil {
			rep.Check("HTTP monitors created", "2 monitors", err.Error(), false)
			return err
		}
		monIDs = append(monIDs, m.ID)
		monNames = append(monNames, m.Name)
	}
	rep.Check("HTTP monitors created", "2 monitors, assignment all", fmt.Sprintf("%d monitors", len(monIDs)), len(monIDs) == 2)

	for _, i := range []int{2, 3} {
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", err.Error(), false)
			return err
		}
		if err := opsJoinLogin(ctx, env, cl, i); err != nil {
			rep.Check(fmt.Sprintf("Login on voter %d after join", i), "login answers", err.Error(), false)
			return err
		}
	}
	rep.Check("Voters 2 and 3 join", "3 voters, logins work", "joined", true)

	nodes, err := opsNodeIDs(ctx, cl, all)
	if err != nil {
		rep.Check("Node IDs of the voters", "each voter lists itself", err.Error(), false)
		return err
	}
	producers := []string{nodes[1], nodes[2], nodes[3]}
	ok, err := pollUntil(ctx, 2*interval+2*opsReplicationRound, 2*time.Second, func() (bool, error) {
		return opsAllProducers(ctx, cl, all, monIDs, producers, time.Now().Add(-2*interval))
	})
	rep.Check("Data replicated to all voters before the failure", "heartbeats of all producers on all voters",
		fmt.Sprintf("complete: %v%s", ok, as09ErrSuffix(err)), ok)
	if !ok {
		return errors.New("baseline heartbeats incomplete")
	}
	snapOK, _, err := opsAwaitSnapshots(ctx, cl, all, 60*time.Second)
	rep.Check("Identical data before the failure", "normalized snapshots equal", fmt.Sprintf("equal: %v%s", snapOK, as09ErrSuffix(err)), snapOK)

	// Log in on voter 1 while the quorum still exists.
	if err := opsRefreshLogins(ctx, cl, []int{1}); err != nil {
		rep.Check("Login on voter 1", "login answers", err.Error(), false)
		return err
	}

	// Quorum lost: voters 2 and 3 are killed (SIGKILL).
	killAt := time.Now()
	if err := cl.Kill(ctx, 2); err != nil {
		rep.Check("Kill voter 2", "killed", err.Error(), false)
		return err
	}
	if err := cl.Kill(ctx, 3); err != nil {
		rep.Check("Kill voter 3", "killed", err.Error(), false)
		return err
	}
	rep.Check("Kill voters 2 and 3 (SIGKILL), quorum lost", "both containers stopped", "killed", true)

	// Writes through voter 1 end with 503, reads still work. The token was
	// taken before the kill: a login needs the leader and answers 503 without quorum.
	var s503, sOK, sOther int
	var firstStatus string
	probeUntil := time.Now().Add(recFreshWait)
	for n := 0; time.Now().Before(probeUntil) && s503 < 3; n++ {
		_, err := cl.Client(1).CreateMonitorInput(ctx, ucapi.MonitorInput{
			Name:            fmt.Sprintf("recovery-probe-%s-%d", suffix, n),
			Type:            "http",
			Config:          map[string]any{"url": env.TargetURL("/recovery/probe")},
			IntervalSeconds: int(interval / time.Second),
			Retries:         1,
			Assignment:      &ucapi.Assignment{Mode: "all"},
		})
		switch code := ucapi.StatusCode(err); {
		case err == nil:
			sOK++
		case code == 503:
			s503++
			if firstStatus == "" {
				firstStatus = fmt.Sprintf("%d after %s", code, time.Since(killAt).Round(time.Second))
			}
		default:
			sOther++
		}
		if err := opsWaitUntil(ctx, time.Now().Add(3*time.Second)); err != nil {
			return err
		}
	}
	rep.Check("Writes through voter 1 without quorum", "503 each time, none accepted",
		fmt.Sprintf("503: %d, accepted: %d, other: %d (first 503 %s)", s503, sOK, sOther, firstStatus),
		s503 > 0 && sOK == 0 && sOther == 0)
	readMons, readErr := cl.Client(1).ListMonitors(ctx)
	rep.Check("Reads through voter 1 without quorum", "200, monitors listed",
		fmt.Sprintf("%d monitors%s", len(readMons), as09ErrSuffix(readErr)), readErr == nil && len(readMons) >= 2)

	// Voter 1 stops as well: all three are now stopped.
	if err := cl.Stop(ctx, 1); err != nil {
		rep.Check("Stop voter 1", "stopped", err.Error(), false)
		return err
	}
	rep.Check("Stop voter 1 (SIGTERM)", "all voters stopped", "stopped", true)

	// Inspect every volume with a one-off container, same environment as the node.
	type survivor struct {
		slot int
		info recRaftInfo
	}
	var infos []survivor
	for _, i := range all {
		out, err := cl.OneShot(ctx, image, i, "raft-info", "--json")
		if err != nil {
			rep.Check(fmt.Sprintf("admin raft-info on voter %d", i), "JSON answer", err.Error(), false)
			return err
		}
		var info recRaftInfo
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			rep.Check(fmt.Sprintf("admin raft-info on voter %d", i), "JSON answer", err.Error(), false)
			return err
		}
		infos = append(infos, survivor{slot: i, info: info})
		rep.Info(fmt.Sprintf("raft-info voter %d", i), "last term and index of the volume",
			fmt.Sprintf("term %d, index %d, applied %d, command level %d, node %s",
				info.LastTerm, info.LastIndex, info.AppliedIndex, info.CommandLevel, info.NodeID))
	}
	rep.Check("admin raft-info on all three volumes", "JSON with node ID and log position",
		fmt.Sprintf("%d volumes read", len(infos)), len(infos) == 3)
	best := infos[0]
	for _, s := range infos[1:] {
		if s.info.LastTerm > best.info.LastTerm || (s.info.LastTerm == best.info.LastTerm && s.info.LastIndex > best.info.LastIndex) {
			best = s
		}
	}
	rep.Info("Survivor chosen for recover", "highest last term, then highest last index",
		fmt.Sprintf("voter %d (term %d, index %d)", best.slot, best.info.LastTerm, best.info.LastIndex))

	// Dry run first: it lists the plan and must change nothing.
	plan, err := cl.OneShot(ctx, image, best.slot, "recover")
	if err != nil {
		rep.Check("admin recover (plan only)", "plan printed", err.Error(), false)
		return err
	}
	var others []string
	for _, i := range all {
		if i != best.slot {
			others = append(others, nodes[i])
		}
	}
	sort.Strings(others)
	planOK := strings.Contains(plan, "Recovery plan for node") && strings.Contains(plan, "Nothing has been changed")
	for _, id := range others {
		planOK = planOK && strings.Contains(plan, id)
	}
	rep.Check("admin recover without --yes shows the plan", "plan lists the other two nodes as revoked, nothing changed",
		fmt.Sprintf("plan %v, lists %d of 2 old nodes", planOK, countContained(plan, others)), planOK)

	applied, err := cl.OneShot(ctx, image, best.slot, "recover", "--yes")
	if err != nil {
		rep.Check("admin recover --yes", "recovered", err.Error(), false)
		return err
	}
	backupLine := ""
	for _, line := range strings.Split(applied, "\n") {
		if strings.HasPrefix(line, "Backup of raft/ and state.db:") {
			backupLine = strings.TrimSpace(line)
		}
	}
	rep.Check("admin recover --yes on the chosen survivor", "node is the only voter, backup written",
		fmt.Sprintf("%s; %s", firstLine(applied), backupLine), strings.Contains(applied, "Recovered: node") && backupLine != "")

	// The other survivors keep their old volumes as copies; the volumes are
	// then removed so that they can rejoin with empty directories.
	staleVol := make(map[int]string)
	for _, i := range all {
		if i == best.slot {
			continue
		}
		staleVol[i] = prefix + fmt.Sprintf("-stale-v%d-data", i)
		staleCont := fmt.Sprintf("%s-stale-v%d", prefix, i)
		defer func(vol, cont string) {
			if err := opsCleanup(ctx, cl, []string{cont}, []string{vol}); err != nil {
				env.Log.Error("cleanup recovery copies", "err", err)
			}
		}(staleVol[i], staleCont)
		if err := cl.CopyVolume(ctx, cl.Names().Volume(i), staleVol[i]); err != nil {
			rep.Check(fmt.Sprintf("Copy the old data of voter %d", i), "copy created", err.Error(), false)
			return err
		}
	}
	rep.Info("Old volumes of the other survivors copied", "copies kept for the stale-start check",
		fmt.Sprintf("%s and %s", staleVol[others2(all, best.slot)[0]], staleVol[others2(all, best.slot)[1]]))

	// Start the recovered node: it becomes leader alone and revokes the others.
	if err := cl.Start(ctx, best.slot, harness.StartOptions{}); err != nil {
		rep.Check(fmt.Sprintf("Start voter %d after recover", best.slot), "healthy", err.Error(), false)
		return err
	}
	if err := opsLogin(ctx, cl, best.slot); err != nil {
		rep.Check(fmt.Sprintf("Login on voter %d after recover", best.slot), "login answers", err.Error(), false)
		return err
	}
	revokedAt := time.Now()
	revoked := false
	_, _ = pollUntil(ctx, 90*time.Second, 2*time.Second, func() (bool, error) {
		logs, err := cl.ContainerLogs(ctx, cl.Names().Voter(best.slot))
		if err != nil {
			return false, err
		}
		revoked = strings.Contains(logs, "old members revoked after recovery")
		return revoked, nil
	})
	rep.Check("Old members revoked after recovery", "log line \"old members revoked after recovery\"",
		fmt.Sprintf("found: %v after %s", revoked, time.Since(revokedAt).Round(time.Second)), revoked)

	after, err := cl.Members(ctx, best.slot)
	if err != nil {
		rep.Check("Node list after recover", "answers", err.Error(), false)
		return err
	}
	selfLeader := len(after.Nodes) == 1 && after.Nodes[0].ID == nodes[best.slot] && after.Nodes[0].Leader
	rep.Check("Node list after recover shows only the recovered node as leader", "1 member, leader",
		fmt.Sprintf("%d members, leader %v", len(after.Nodes), selfLeader), selfLeader)

	mons, err := cl.Client(best.slot).ListMonitors(ctx)
	missing := 0
	for _, name := range monNames {
		found := false
		for _, m := range mons {
			if m.Name == name {
				found = true
			}
		}
		if !found {
			missing++
		}
	}
	rep.Check("Data from before the failure present", "all baseline monitors listed",
		fmt.Sprintf("%d of %d monitors present%s", len(monNames)-missing, len(monNames), as09ErrSuffix(err)), err == nil && missing == 0)

	probe := "recovery-after-" + suffix
	if _, err := opsHTTPMonitor(ctx, env, cl.Client(best.slot), probe, "/recovery/"+suffix+"/after", nil); err != nil {
		rep.Check("Write after recover", "201", err.Error(), false)
		return err
	}
	rep.Check("Write after recover", "201 on the recovered node", "monitor created", true)

	// Join two fresh voters into the emptied slots.
	for _, i := range others2(all, best.slot) {
		if err := cl.Remove(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Move voter %d aside (container and volume)", i), "removed; copy kept", err.Error(), false)
			return err
		}
		joinStart := time.Now()
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d rejoins as a new node", i), "listed as voter", err.Error(), false)
			return err
		}
		if err := opsJoinLogin(ctx, env, cl, i); err != nil {
			rep.Check(fmt.Sprintf("Login on voter %d after rejoin", i), "login answers", err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Voter %d rejoins as a new node", i), "listed as voter with a new node ID",
			fmt.Sprintf("joined after %s", time.Since(joinStart).Round(time.Second)), true)
	}

	list, err := cl.Members(ctx, best.slot)
	if err != nil {
		rep.Check("Member list after the rejoins", "answers", err.Error(), false)
		return err
	}
	oldLeft := 0
	for _, id := range others {
		if _, ok := as09Find(list, id); ok {
			oldLeft++
		}
	}
	rep.Check("Three voters after the rejoins, old IDs gone", "3 voters, old IDs not listed",
		fmt.Sprintf("%d voters, old IDs listed: %d", opsVoterCount(list), oldLeft), opsVoterCount(list) == 3 && oldLeft == 0)

	newNodes, err := opsNodeIDs(ctx, cl, all)
	if err != nil {
		rep.Check("Node IDs after the rejoins", "each voter lists itself", err.Error(), false)
		return err
	}
	newProducers := []string{newNodes[1], newNodes[2], newNodes[3]}
	ok, err = pollUntil(ctx, 2*interval+2*opsReplicationRound, 2*time.Second, func() (bool, error) {
		return opsAllProducers(ctx, cl, all, monIDs, newProducers, time.Now().Add(-2*interval))
	})
	rep.Check("Heartbeats of all three voters on every voter after the rejoins", "all producers on all voters",
		fmt.Sprintf("complete: %v%s", ok, as09ErrSuffix(err)), ok)
	snapOK, diffs, err := opsAwaitSnapshots(ctx, cl, all, 90*time.Second)
	measured := "snapshots of voters 1, 2, 3 equal"
	if !snapOK {
		measured = fmt.Sprintf("%d differences: %v%s", len(diffs), as09First(diffs, 5), as09ErrSuffix(err))
	}
	rep.Check("Identical data after the rejoins", "normalized snapshots equal", measured, snapOK)

	// A stale copy of an old data directory is started on the cluster network.
	leaderBefore, _ := cl.LeaderOf(ctx, all)
	stale := others2(all, best.slot)[0]
	staleCont := fmt.Sprintf("%s-stale-v%d", prefix, stale)
	if err := cl.StartStale(ctx, staleCont, staleVol[stale], stale); err != nil {
		rep.Check("Start the stale copy of the old voter data", "container starts", err.Error(), false)
		return err
	}
	if err := opsWaitUntil(ctx, time.Now().Add(60*time.Second)); err != nil {
		return err
	}
	staleRunning, _ := cl.ContainerRunning(ctx, staleCont)
	staleLogs, _ := cl.ContainerLogs(ctx, staleCont)
	leaderAfter, leaderErr := cl.LeaderOf(ctx, all)
	listStale, err := cl.Members(ctx, best.slot)
	if err != nil {
		rep.Check("Member list with the stale copy running", "answers", err.Error(), false)
		return err
	}
	staleIDs := 0
	for _, id := range others {
		if _, ok := as09Find(listStale, id); ok {
			staleIDs++
		}
	}
	rep.Info("Stale copy of voter "+fmt.Sprint(stale)+" started", "rejected, no disturbance",
		fmt.Sprintf("still running after 60 s: %v; leader before %s, after %s%s; old IDs listed %d; log: %s",
			staleRunning, leaderBefore, leaderAfter, as09ErrSuffix(leaderErr), staleIDs, replaceReason(staleLogs)))
	stableOK := leaderErr == nil && leaderAfter == leaderBefore && staleIDs == 0 && opsVoterCount(listStale) == 3
	rep.Check("Stale copy does not disturb the cluster", "leader unchanged, 3 voters, no old ID listed",
		fmt.Sprintf("leader unchanged %v, voters %d, old IDs listed %d", leaderErr == nil && leaderAfter == leaderBefore, opsVoterCount(listStale), staleIDs), stableOK)
	if err := opsCleanup(ctx, cl, []string{staleCont}, nil); err != nil {
		rep.Info("Remove the stale container", "removed", err.Error())
	}

	// Admin password reset in a running voter, read from stdin.
	pw, err := opsRandomSecret(24)
	if err != nil {
		return err
	}
	out, err := cl.ExecStdin(ctx, best.slot, pw+"\n", "/pulsequorum", "admin", "reset-password", "--username", "admin")
	rep.Check("admin reset-password in the running voter (password on stdin)", "password reset, sessions ended",
		fmt.Sprintf("%s%s", firstLine(out), as09ErrSuffix(err)), err == nil && strings.Contains(out, "Password of"))
	if err != nil {
		return err
	}
	newClient := ucapi.New(env.Cfg.BaseURL(best.slot), "admin", pw)
	loginErr := newClient.Login(ctx)
	rep.Check("Login with the new password", "login answers 200",
		fmt.Sprintf("login ok: %v%s", loginErr == nil, as09ErrSuffix(loginErr)), loginErr == nil)
	oldErr := cl.Client(best.slot).Login(ctx)
	rep.Check("Login with the old password", "rejected (401)",
		fmt.Sprintf("status %d", ucapi.StatusCode(oldErr)), ucapi.StatusCode(oldErr) == 401)
	return nil
}

// others2 returns the voters of all except slot.
func others2(all []int, slot int) []int {
	var out []int
	for _, i := range all {
		if i != slot {
			out = append(out, i)
		}
	}
	return out
}

// countContained counts how many of ids occur in s.
func countContained(s string, ids []string) int {
	n := 0
	for _, id := range ids {
		if strings.Contains(s, id) {
			n++
		}
	}
	return n
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

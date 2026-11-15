// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/chaos/harness"
	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// Images and versions of the rolling update (AS-09). The old image is built
// from a git ref in a temporary worktree, the new one from the working tree.
const (
	as09ImageOld   = "uptimecluster:chaos-n"
	as09ImageNew   = "uptimecluster:chaos-n1"
	as09VersionOld = "0.0.0-chaos-n"
	as09VersionNew = "0.0.1-chaos-n1"
	// as09Monitors is the number of HTTP monitors that run during the update.
	as09Monitors = 3
	// as09FailIntervals is how long the target answers 503 during the first
	// step, in monitor intervals. Two failures in a row make a DOWN.
	as09FailIntervals = 3
	// as09ReadyTimeout bounds the wait for a restarted voter to be back.
	as09ReadyTimeout = 3 * time.Minute
)

func init() {
	register(Scenario{
		ID:    "as09",
		Label: "AS-09",
		Title: "Rolling Update aller Voter auf neue Minor-Version",
		Run:   runAS09,
	})
}

// runAS09 automates AS-09 (docs/pflichtenheft.md, section 13.2, FA-CLU-12,
// ADR 0018, docs/betrieb/cluster.md "Cluster aktualisieren"). Three voters on
// the old image run monitors; each voter is stopped and restarted from its
// volume on the new image, followers first, the leader last.
func runAS09(ctx context.Context, env *Env) error {
	rep := env.Rep
	interval := env.Cfg.Interval
	if env.Cfg.Voters < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(env.Cfg.Voters), false)
		return errors.New("AS-09 needs three voter slots")
	}
	root, err := findRepoRoot()
	if err != nil {
		return err
	}

	buildStart := time.Now()
	if err := as09BuildImages(ctx, env, root); err != nil {
		rep.Check("Old and new image built", "chaos-n from the ref, chaos-n1 from the working tree", err.Error(), false)
		return err
	}
	rep.Info("Old and new image built", "both images present",
		fmt.Sprintf("%s from %s, %s from working tree, %s",
			as09ImageOld, env.FromRef, as09ImageNew, time.Since(buildStart).Round(time.Second)))

	env.Cfg.Image = as09ImageOld
	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := cl.Bootstrap(ctx, "admin"); err != nil {
		rep.Check("Voter 1 bootstraps on the old image", "admin set up", err.Error(), false)
		return err
	}
	rep.Check("Voter 1 bootstraps on the old image", "admin set up", "admin created on "+cl.Names().Voter(1), true)
	api := cl.Client(1)

	// Notifications and monitors exist before the joins, so the update runs
	// on replicated data.
	suffix := randomSuffix()
	topicPath := "/as09-" + suffix
	since := time.Now()
	ch, err := opsCreateChannel(ctx, env, api, "as09-ntfy-"+suffix, "as09-"+suffix)
	if err != nil {
		rep.Check("Notification channel with test delivery", "delivered to ntfy", err.Error(), false)
		return err
	}
	rep.Check("Notification channel with test delivery", "delivered to ntfy", "channel "+ch.ID, true)

	monIDs := make([]string, 0, as09Monitors)
	monNames := make([]string, 0, as09Monitors)
	for n := 1; n <= as09Monitors; n++ {
		name := fmt.Sprintf("as09-%s-m%d", suffix, n)
		m, err := opsHTTPMonitor(ctx, env, api, name, fmt.Sprintf("/as09/%s/m%d", suffix, n), []string{ch.ID})
		if err != nil {
			rep.Check("HTTP monitors created", fmt.Sprintf("%d monitors, assignment all, interval %s", as09Monitors, interval), err.Error(), false)
			return err
		}
		monIDs = append(monIDs, m.ID)
		monNames = append(monNames, m.Name)
	}
	rep.Check("HTTP monitors created", fmt.Sprintf("%d monitors, assignment all, interval %s", as09Monitors, interval),
		fmt.Sprintf("%d monitors", len(monIDs)), len(monIDs) == as09Monitors)

	for _, i := range []int{2, 3} {
		joinStart := time.Now()
		if err := cl.Join(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Voter %d joins", i), "listed as voter", err.Error(), false)
			return err
		}
		if err := opsJoinLogin(ctx, env, cl, i); err != nil {
			rep.Check(fmt.Sprintf("Login on voter %d after join", i), "login answers", err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Voter %d joins on the old image", i), "listed as voter, login works",
			fmt.Sprintf("joined after %s", time.Since(joinStart).Round(time.Second)), true)
	}

	all := []int{1, 2, 3}
	nodes, err := opsNodeIDs(ctx, cl, all)
	if err != nil {
		rep.Check("Node IDs of the voters", "each voter lists itself", err.Error(), false)
		return err
	}
	producers := []string{nodes[1], nodes[2], nodes[3]}
	ok, err := pollUntil(ctx, 2*interval+2*opsReplicationRound, 2*time.Second, func() (bool, error) {
		return opsAllProducers(ctx, cl, all, monIDs, producers, time.Now().Add(-2*interval))
	})
	rep.Check("Heartbeats of all voters on every voter before the update", "all producers on all voters",
		fmt.Sprintf("complete: %v%s", ok, as09ErrSuffix(err)), ok)
	if !ok {
		return errors.New("baseline heartbeats incomplete")
	}

	before, err := cl.Members(ctx, 1)
	if err != nil {
		rep.Check("Node list before the update", "answers", err.Error(), false)
		return err
	}
	oldCount := as09CountVersion(before, as09VersionOld)
	rep.Check("All voters on the old version before the update", "3 voters on "+as09VersionOld,
		fmt.Sprintf("%d of %d nodes on %s, %d voters", oldCount, len(before.Nodes), as09VersionOld, opsVoterCount(before)),
		oldCount == 3 && opsVoterCount(before) == 3)
	cmdBefore := before.CommandLevel
	rep.Info("Command level before the update", "commandLevel from the node list",
		fmt.Sprintf("commandLevel %d, upgradePending %v", before.CommandLevel, before.UpgradePending))

	// Followers first, in slot order; the leader last.
	leaderID, err := cl.LeaderOf(ctx, all)
	if err != nil {
		rep.Check("One leader before the update", "identical leader ID", err.Error(), false)
		return err
	}
	var order []int
	leaderSlot := 0
	for _, i := range all {
		if nodes[i] == leaderID {
			leaderSlot = i
			continue
		}
		order = append(order, i)
	}
	if leaderSlot == 0 {
		return errors.New("no leader among the voters")
	}
	order = append(order, leaderSlot)
	rep.Info("Update order", "followers first, leader last", fmt.Sprintf("%v (leader was voter %d)", order, leaderSlot))

	// Rolling update: one voter at a time.
	var failStart time.Time
	for step, i := range order {
		others := make([]int, 0, 2)
		for _, v := range all {
			if v != i {
				others = append(others, v)
			}
		}
		if err := opsRefreshLogins(ctx, cl, others); err != nil {
			rep.Check(fmt.Sprintf("Logins before step %d", step+1), "logins answer", err.Error(), false)
			return err
		}
		stepStart := time.Now()
		if step == 0 {
			failStart = stepStart
			env.Target.SetFailing(true)
		}

		t0 := time.Now()
		if err := cl.Stop(ctx, i); err != nil {
			rep.Check(fmt.Sprintf("Stop voter %d (SIGTERM)", i), "stopped within grace period", err.Error(), false)
			return err
		}
		stopDur := time.Since(t0)

		t1 := time.Now()
		if err := cl.Start(ctx, i, harness.StartOptions{Image: as09ImageNew}); err != nil {
			rep.Check(fmt.Sprintf("Start voter %d on the new image", i), "healthy on "+as09VersionNew, err.Error(), false)
			return err
		}
		startDur := time.Since(t1)

		if err := opsLogin(ctx, cl, i); err != nil {
			rep.Check(fmt.Sprintf("Login on voter %d after restart", i), "login answers", err.Error(), false)
			return err
		}
		readyOK, lagInfo, lastErr := as09WaitReady(ctx, cl, others, nodes[i])
		readyDur := time.Since(t0)
		rep.Check(fmt.Sprintf("Voter %d is back as voter on %s, healthy, replicated", i, as09VersionNew),
			"node list of the other voters: voter, version, healthy, replication lag present",
			fmt.Sprintf("%v after %s (%s)%s", readyOK, readyDur.Round(time.Second), lagInfo, as09ErrSuffix(lastErr)), readyOK)
		if !readyOK {
			return fmt.Errorf("voter %d did not come back on the new version", i)
		}
		rep.Info(fmt.Sprintf("Voter %d restart timing", i), "recorded",
			fmt.Sprintf("stop %s, start %s, back as voter %s after the stop began", stopDur.Round(time.Millisecond), startDur.Round(time.Millisecond), readyDur.Round(time.Millisecond)))

		if err := opsWaitUntil(ctx, stepStart.Add(2*interval)); err != nil {
			return err
		}
		stepEnd := time.Now()
		worst, worstAt, own, err := as09Gaps(ctx, cl, others, i, nodes, monIDs, stepStart, stepEnd, interval)
		if err != nil {
			rep.Check(fmt.Sprintf("Heartbeat gaps during step %d", step+1), "measurable", err.Error(), false)
			return err
		}
		limit := 2 * interval
		rep.Check(fmt.Sprintf("No interruption while voter %d is updated", i),
			fmt.Sprintf("max heartbeat gap <= 2 x interval (%s) for every monitor and producer on the voters that stay up", limit),
			fmt.Sprintf("max gap %s (%s) over %s", as09Dur(worst), worstAt, as09Dur(stepEnd.Sub(stepStart))),
			worst <= limit)
		rep.Info(fmt.Sprintf("Own heartbeat gap of voter %d during its restart", i), "recorded, not limited",
			fmt.Sprintf("seen by the other voters %s, seen by itself %s", own.bySelf, own.byOthers))

		if step == 0 {
			if err := opsWaitUntil(ctx, failStart.Add(as09FailIntervals*interval)); err != nil {
				return err
			}
			env.Target.SetFailing(false)
			rep.Info("Target failure window", "503 from the first step on, then healthy",
				fmt.Sprintf("503 from %s for %s", failStart.Format("15:04:05"), time.Since(failStart).Round(time.Second)))
		}
		upgradePending, pendErr := as09UpgradePending(ctx, cl, others)
		if pendErr != nil {
			return pendErr
		}
		rep.Info(fmt.Sprintf("upgradePending after step %d", step+1),
			"recorded; ADR 0018 section 7 derives it from the command level, which stays 2 here",
			fmt.Sprint(upgradePending))
	}
	updateDur := time.Since(failStart)
	rep.Info("Update duration", "recorded", fmt.Sprintf("from the first stop to the last voter back: %s", updateDur.Round(time.Second)))

	// Final state.
	pending := true
	_, _ = pollUntil(ctx, 30*time.Second, 2*time.Second, func() (bool, error) {
		list, err := cl.Members(ctx, 1)
		if err != nil {
			return false, err
		}
		pending = list.UpgradePending
		return !list.UpgradePending, nil
	})
	after, err := cl.Members(ctx, 1)
	if err != nil {
		rep.Check("Node list after the update", "answers", err.Error(), false)
		return err
	}
	newCount := as09CountVersion(after, as09VersionNew)
	rep.Check("All voters on the new version", "3 voters on "+as09VersionNew,
		fmt.Sprintf("%d of %d nodes on %s, %d voters", newCount, len(after.Nodes), as09VersionNew, opsVoterCount(after)),
		newCount == 3 && opsVoterCount(after) == 3)
	rep.Check("upgradePending after the update", "false", fmt.Sprintf("upgradePending %v", pending), !pending)
	rep.Info("Command level after the update", "commandLevel from the node list",
		fmt.Sprintf("before %d, after %d (upgradePending %v)", cmdBefore, after.CommandLevel, after.UpgradePending))

	// One DOWN and one UP per monitor: the target failed once.
	var counts map[string]int
	alertsOK, alertErr := pollUntil(ctx, 90*time.Second, 2*time.Second, func() (bool, error) {
		counts = opsNotifyCounts(env.Ntfy, topicPath, since)
		for _, name := range monNames {
			if counts["DOWN: "+name] > 1 || counts["UP: "+name] > 1 {
				return false, fmt.Errorf("duplicate notification for %s", name)
			}
			if counts["DOWN: "+name] != 1 || counts["UP: "+name] != 1 {
				return false, nil
			}
		}
		return true, nil
	})
	var perMonitor []string
	for _, name := range monNames {
		perMonitor = append(perMonitor, fmt.Sprintf("%s: DOWN %d, UP %d", name, counts["DOWN: "+name], counts["UP: "+name]))
	}
	rep.Check("Exactly one DOWN and one UP per monitor", "1 DOWN and 1 UP each, no duplicates",
		strings.Join(perMonitor, "; ")+as09ErrSuffix(alertErr), alertsOK)

	// Monitors still run and report UP.
	var notUp []string
	for n, id := range monIDs {
		cur, err := cl.Client(1).GetMonitor(ctx, id)
		if err != nil {
			notUp = append(notUp, monNames[n]+": "+err.Error())
			continue
		}
		if cur.Status != "UP" {
			notUp = append(notUp, monNames[n]+": "+cur.Status)
		}
	}
	rep.Check("Monitors UP after the update", "all monitors UP on voter 1",
		fmt.Sprintf("%d of %d UP %v", len(monIDs)-len(notUp), len(monIDs), notUp), len(notUp) == 0)

	snapOK, diffs, err := opsAwaitSnapshots(ctx, cl, all, 60*time.Second)
	measured := "snapshots of voters 1, 2, 3 equal"
	if !snapOK {
		measured = fmt.Sprintf("%d differences: %v%s", len(diffs), as09First(diffs, 5), as09ErrSuffix(err))
	}
	rep.Check("Identical data on all voters after the update", "normalized snapshots equal", measured, snapOK)

	// Writes work through every voter and are seen by the other two.
	for _, w := range all {
		var seen []int
		for _, v := range all {
			if v != w {
				seen = append(seen, v)
			}
		}
		desc, visible, err := opsWriteVisible(ctx, cl, env, w, seen, fmt.Sprintf("as09-%s-after-w%d", suffix, w), fmt.Sprintf("/as09/%s/after-w%d", suffix, w))
		if err != nil {
			rep.Check(fmt.Sprintf("Write through voter %d after the update", w), "201, visible within 5 s", err.Error(), false)
			return err
		}
		rep.Check(fmt.Sprintf("Write through voter %d after the update", w), "201, visible on the other voters within 5 s", desc, visible)
	}
	return nil
}

// as09BuildImages builds the old image from the git ref in a temporary
// worktree and the new image from the working tree. The worktree is removed
// afterwards.
func as09BuildImages(ctx context.Context, env *Env, root string) error {
	parent, err := os.MkdirTemp("", "ucchaos-as09-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(parent) }()
	wt := filepath.Join(parent, "tree")
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "worktree", "add", "--detach", wt, env.FromRef).CombinedOutput(); err != nil { // #nosec G204 -- fixed binary; the ref is a flag of the driver
		return fmt.Errorf("git worktree add %s: %w: %s", env.FromRef, err, strings.TrimSpace(string(out)))
	}
	defer as09RemoveWorktree(env, root, wt)

	if _, err := harness.EnsureImage(ctx, env.Runtime, harness.BuildSpec{
		Tag:       as09ImageOld,
		File:      filepath.Join(wt, "Dockerfile"),
		Context:   wt,
		BuildArgs: map[string]string{"VERSION": as09VersionOld},
	}, harness.BuildAlways, env.Log); err != nil {
		return err
	}
	if _, err := harness.EnsureImage(ctx, env.Runtime, harness.BuildSpec{
		Tag:       as09ImageNew,
		File:      filepath.Join(root, "Dockerfile"),
		Context:   root,
		BuildArgs: map[string]string{"VERSION": as09VersionNew},
	}, harness.BuildAlways, env.Log); err != nil {
		return err
	}
	return nil
}

// as09RemoveWorktree removes the temporary worktree of the old image.
func as09RemoveWorktree(env *Env, root, wt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "worktree", "remove", "--force", wt).CombinedOutput(); err != nil { // #nosec G204 -- fixed binary
		env.Log.Error("remove worktree", "err", err, "out", strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", root, "worktree", "prune").CombinedOutput(); err != nil { // #nosec G204 -- fixed binary
		env.Log.Error("prune worktrees", "err", err, "out", strings.TrimSpace(string(out)))
	}
}

// as09WaitReady waits until voter i is listed as a voter with the new
// version, healthy and with a replication lag, in the node lists of all the
// other voters. It returns the lags seen for the report.
func as09WaitReady(ctx context.Context, cl *harness.Cluster, others []int, id string) (bool, string, error) {
	var lags []string
	var last error
	ok, err := pollUntil(ctx, as09ReadyTimeout, 2*time.Second, func() (bool, error) {
		lags = lags[:0]
		for _, v := range others {
			list, err := cl.Client(v).ListClusterNodes(ctx)
			if err != nil {
				return false, err
			}
			n, found := as09Find(list, id)
			switch {
			case !found:
				last = fmt.Errorf("not listed by voter %d", v)
				return false, nil
			case !n.Voter:
				last = fmt.Errorf("not a voter in the view of voter %d", v)
				return false, nil
			case n.Version != as09VersionNew:
				last = fmt.Errorf("version %q in the view of voter %d", n.Version, v)
				return false, nil
			case n.Healthy == nil || !*n.Healthy:
				last = fmt.Errorf("not healthy in the view of voter %d", v)
				return false, nil
			case n.ReplicationLagMs == nil:
				last = fmt.Errorf("no replication lag in the view of voter %d", v)
				return false, nil
			}
			lags = append(lags, fmt.Sprintf("lag %d ms as seen by voter %d", *n.ReplicationLagMs, v))
		}
		last = nil
		return true, nil
	})
	if err != nil {
		last = err
	}
	return ok, strings.Join(lags, ", "), last
}

// as09Find returns the member with the given ID.
func as09Find(list ucapi.ClusterNodeList, id string) (ucapi.ClusterNode, bool) {
	for _, n := range list.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return ucapi.ClusterNode{}, false
}

// as09CountVersion counts the members that run the given version.
func as09CountVersion(list ucapi.ClusterNodeList, version string) int {
	n := 0
	for _, m := range list.Nodes {
		if m.Version == version {
			n++
		}
	}
	return n
}

// as09UpgradePending reads upgradePending from the node list of the first
// voter that answers.
func as09UpgradePending(ctx context.Context, cl *harness.Cluster, viewers []int) (bool, error) {
	var lastErr error
	for _, v := range viewers {
		list, err := cl.Client(v).ListClusterNodes(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		return list.UpgradePending, nil
	}
	return false, fmt.Errorf("no voter answered the node list: %w", lastErr)
}

// as09GapsResult holds the restarted voter's own gaps.
type as09GapsResult struct {
	bySelf   string
	byOthers string
}

// as09Gaps measures the largest heartbeat gap per monitor and producer, for
// every voter that stays up (viewers) and every producer except the one being
// restarted. Gaps of the restarted voter are returned separately.
func as09Gaps(ctx context.Context, cl *harness.Cluster, viewers []int, restarted int, nodes map[int]string, monIDs []string, from, to time.Time, interval time.Duration) (time.Duration, string, as09GapsResult, error) {
	var worst time.Duration
	var worstAt string
	var ownOthers time.Duration
	var ownSelf time.Duration
	for _, m := range monIDs {
		for _, v := range viewers {
			for p := 1; p <= 3; p++ {
				gap, err := cl.HeartbeatGap(ctx, v, m, nodes[p], from, to, interval)
				if err != nil {
					return 0, "", as09GapsResult{}, err
				}
				if p == restarted {
					ownOthers = max(ownOthers, gap)
					continue
				}
				if gap > worst {
					worst = gap
					worstAt = fmt.Sprintf("viewer %d, producer %d, monitor %s", v, p, m[:min(8, len(m))])
				}
			}
		}
		gap, err := cl.HeartbeatGap(ctx, restarted, m, nodes[restarted], from, to, interval)
		if err != nil {
			return 0, "", as09GapsResult{}, err
		}
		ownSelf = max(ownSelf, gap)
	}
	return worst, worstAt, as09GapsResult{
		bySelf:   as09Dur(ownSelf),
		byOthers: as09Dur(ownOthers),
	}, nil
}

// as09Dur renders a duration with 0.1 s precision for the report.
func as09Dur(d time.Duration) string {
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// as09ErrSuffix renders an error for a report cell.
func as09ErrSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

// as09First returns at most n entries of in.
func as09First(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/chaos/harness"
	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// minorityWriteLimit bounds one write through a survivor with all others up
// or with a minority down. It is the expectation of this scenario, not a
// requirement of the Pflichtenheft.
const minorityWriteLimit = 5 * time.Second

func init() {
	register(Scenario{
		ID:    "minority",
		Label: "MINORITY",
		Title: "Minderheit fällt aus: Schreib-/Lesezugriff, Heartbeats, Konsistenz (NFA-REL-01, -04, -05)",
		Run:   runMinority,
	})
}

// runMinority kills a minority of followers, that is floor((n-1)/2) voters,
// after confirmed writes through a follower. The survivors must keep serving
// writes and reads and keep producing heartbeats. After the restart every
// confirmed write must be on every voter, and the heartbeats the killed
// voters produced before the kill must still be there.
func runMinority(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(n), false)
		return errors.New("minority needs three voter slots")
	}
	killCount := (n - 1) / 2

	cl, err := env.NewCluster()
	if err != nil {
		rep.Check("Cluster prepared", "config valid", err.Error(), false)
		return err
	}
	if err := bringUp(ctx, env, cl, n); err != nil {
		return err
	}
	leader, leaderID, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("One leader before the kill", "identical leader ID", err.Error(), false)
		return err
	}
	followers := without(slots, leader)
	victims := followers[len(followers)-killCount:]
	survivors := without(slots, victims...)
	writer := leader
	for _, s := range survivors {
		if s != leader {
			writer = s
			break
		}
	}
	rep.Check("Layout before the kill",
		fmt.Sprintf("%d follower(s) killed, leader and %d voter(s) survive", killCount, len(survivors)-1),
		fmt.Sprintf("leader voter %d, killed %v, survivors %v, writes via voter %d", leader, victims, survivors, writer),
		true)

	nodes, err := nodeIDs(ctx, cl, slots)
	if err != nil {
		rep.Check("Node IDs of all voters", "every voter lists itself", err.Error(), false)
		return err
	}
	producers := make([]string, 0, n)
	for _, i := range slots {
		producers = append(producers, nodes[i])
	}

	// Confirmed writes through a follower: two monitors, one update, one
	// maintenance window.
	suffix := randomSuffix()
	wc := cl.Client(writer)
	created := time.Now()
	monA, err := createHTTPMonitor(ctx, wc, "minority-"+suffix+"-a", env.TargetURL("/minority/"+suffix+"/a"), intervalSec, nil)
	if err != nil {
		rep.Check("Monitor created via follower", "201", err.Error(), false)
		return err
	}
	monB, err := createHTTPMonitor(ctx, wc, "minority-"+suffix+"-b", env.TargetURL("/minority/"+suffix+"/b"), intervalSec, nil)
	if err != nil {
		rep.Check("Monitor created via follower", "201", err.Error(), false)
		return err
	}
	rep.Check("Monitors created via follower", "201 for both", fmt.Sprintf("%s, %s", monA.ID, monB.ID), true)

	updName := monA.Name + "-upd"
	if _, err := wc.UpdateMonitor(ctx, monA.ID, ucapi.MonitorInput{
		Name:                   updName,
		Description:            "updated before the kill",
		Type:                   "http",
		Config:                 map[string]any{"url": env.TargetURL("/minority/" + suffix + "/a")},
		IntervalSeconds:        intervalSec,
		Retries:                1,
		Assignment:             &ucapi.Assignment{Mode: "all"},
		NotificationChannelIDs: []string{},
	}); err != nil {
		rep.Check("Monitor updated via follower", "200", err.Error(), false)
		return err
	}
	rep.Check("Monitor updated via follower", "200, new name visible", "renamed to "+updName, true)

	winName := "minority-" + suffix + "-window"
	win, err := wc.CreateMaintenance(ctx, ucapi.MaintenanceWindowInput{
		Name:       winName,
		MonitorIDs: []string{monA.ID},
		Schedule: map[string]any{
			"kind":     "once",
			"startsAt": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
			"endsAt":   time.Now().Add(7 * time.Hour).UTC().Format(time.RFC3339),
		},
		Active: true,
	})
	if err != nil {
		rep.Check("Maintenance window created via follower", "201", err.Error(), false)
		return err
	}
	rep.Check("Maintenance window created via follower", "201", "window "+win.ID, true)

	// Baseline: heartbeats of all voters for both monitors, seen by every
	// survivor. The monitors run for three intervals before the kill, so the
	// victims have several heartbeats each to lose or keep.
	ids := []string{monA.ID, monB.ID}
	seen, _ := waitHeartbeatsOfAll(ctx, cl, survivors[0], ids, producers, created, 2*interval+replicationRound)
	rep.Check("Every voter produces heartbeats before the kill", "heartbeats of all voters for both monitors",
		fmt.Sprintf("complete: %v", seen), seen)
	if err := waitUntil(ctx, created.Add(3*interval)); err != nil {
		return err
	}

	// NFA-REL-05 baseline: heartbeats of the victims as each survivor has them.
	type hbKey struct{ survivor, monitor, victim int }
	baseline := make(map[hbKey]map[string]bool)
	union := make(map[string]map[int]map[string]bool) // monitor -> victim -> keys
	baseTotal := 0
	baseMin := -1
	nowBefore := time.Now()
	for _, s := range survivors {
		for _, m := range ids {
			for _, v := range victims {
				keys, err := heartbeatKeys(ctx, cl, s, m, nodes[v], created, nowBefore)
				if err != nil {
					rep.Check("Heartbeats of killed voters read before the kill", "readable", err.Error(), false)
					return err
				}
				baseline[hbKey{s, indexOf(ids, m), v}] = keys
				baseTotal += len(keys)
				if baseMin < 0 || len(keys) < baseMin {
					baseMin = len(keys)
				}
				if union[m] == nil {
					union[m] = make(map[int]map[string]bool)
				}
				if union[m][v] == nil {
					union[m][v] = make(map[string]bool)
				}
				for k := range keys {
					union[m][v][k] = true
				}
			}
		}
	}
	rep.Check("Killed voters have heartbeats before the kill", "at least 2 per survivor, monitor and killed voter",
		fmt.Sprintf("%d heartbeats in total, fewest %d", baseTotal, baseMin), baseMin >= 2)

	// Kill the followers.
	killAt := time.Now()
	for _, v := range victims {
		if err := cl.Kill(ctx, v); err != nil {
			rep.Check("SIGKILL "+cl.Names().Voter(v), "killed", err.Error(), false)
			return err
		}
	}
	rep.Check("SIGKILL of the minority", fmt.Sprintf("%d voter(s) killed", killCount),
		fmt.Sprintf("voters %v killed", victims), true)
	lid, err := cl.LeaderOf(ctx, survivors)
	rep.Check("Leader unchanged after the loss of the minority", "the same leader on all survivors",
		fmt.Sprintf("leader %s%s", lid, errSuffix(err)), err == nil && lid == leaderID)

	// Writes and reads through every survivor, while the minority is down.
	for _, s := range survivors {
		c := cl.Client(s)
		name := fmt.Sprintf("minority-%s-via-%d", suffix, s)
		t := time.Now()
		m, err := createHTTPMonitor(ctx, c, name, env.TargetURL("/minority/"+suffix+"/"+name), intervalSec, nil)
		dCreate := time.Since(t)
		rep.Check(fmt.Sprintf("Create monitor via survivor voter %d", s), fmt.Sprintf("201 within %s", minorityWriteLimit),
			fmt.Sprintf("%s%s", elapsedMs(dCreate), errSuffix(err)), err == nil && dCreate < minorityWriteLimit)
		if err != nil {
			continue
		}
		t = time.Now()
		_, err = c.UpdateMonitor(ctx, m.ID, ucapi.MonitorInput{
			Name:            name + "-upd",
			Type:            "http",
			Config:          map[string]any{"url": env.TargetURL("/minority/" + suffix + "/" + name)},
			IntervalSeconds: intervalSec,
			Retries:         1,
			Assignment:      &ucapi.Assignment{Mode: "all"},
		})
		dUpdate := time.Since(t)
		rep.Check(fmt.Sprintf("Update monitor via survivor voter %d", s), fmt.Sprintf("200 within %s", minorityWriteLimit),
			fmt.Sprintf("%s%s", elapsedMs(dUpdate), errSuffix(err)), err == nil && dUpdate < minorityWriteLimit)
		t = time.Now()
		err = c.DeleteMonitor(ctx, m.ID)
		dDelete := time.Since(t)
		rep.Check(fmt.Sprintf("Delete monitor via survivor voter %d", s), fmt.Sprintf("204 within %s", minorityWriteLimit),
			fmt.Sprintf("%s%s", elapsedMs(dDelete), errSuffix(err)), err == nil && dDelete < minorityWriteLimit)
		mons, err := c.ListMonitors(ctx)
		found := false
		for _, mm := range mons {
			if mm.ID == m.ID {
				found = true
			}
		}
		rep.Check(fmt.Sprintf("List via survivor voter %d after delete", s), "deleted monitor gone, confirmed monitor visible",
			fmt.Sprintf("%d monitors, deleted still listed: %v%s", len(mons), found, errSuffix(err)), err == nil && !found)
	}

	// Reads on each survivor show the confirmed writes from before the kill.
	for _, s := range survivors {
		mons, err := cl.Client(s).ListMonitors(ctx)
		wins, werr := cl.Client(s).ListMaintenance(ctx)
		nameOK, winOK := false, false
		for _, m := range mons {
			if m.ID == monA.ID && m.Name == updName {
				nameOK = true
			}
		}
		for _, w := range wins {
			if w.ID == win.ID {
				winOK = true
			}
		}
		lerr := err
		if lerr == nil {
			lerr = werr
		}
		rep.Check(fmt.Sprintf("Confirmed writes readable on survivor voter %d", s),
			"renamed monitor and maintenance window listed",
			fmt.Sprintf("monitor renamed: %v, window: %v%s", nameOK, winOK, errSuffix(lerr)),
			err == nil && werr == nil && nameOK && winOK)
	}

	// Heartbeats of the survivors keep coming: no gap above 2 x interval.
	if err := waitUntil(ctx, killAt.Add(3*interval)); err != nil {
		return err
	}
	now := time.Now()
	var worst time.Duration
	worstWhat := ""
	for _, s := range survivors {
		for _, m := range ids {
			gap, err := cl.HeartbeatGap(ctx, s, m, nodes[s], killAt, now, interval)
			if err != nil {
				rep.Check("Heartbeat gaps on survivors", "readable", err.Error(), false)
				return err
			}
			if gap > worst {
				worst = gap
				worstWhat = fmt.Sprintf("voter %d, monitor %d", s, indexOf(ids, m)+1)
			}
		}
	}
	rep.Check("Survivors keep producing heartbeats", fmt.Sprintf("max gap <= 2 x interval (%s)", 2*interval),
		fmt.Sprintf("max gap %s (%s) over %s", worst.Round(time.Second), worstWhat, now.Sub(killAt).Round(time.Second)),
		worst <= 2*interval)

	// Restart the killed voters with their data.
	for _, v := range victims {
		restartAt := time.Now()
		if err := cl.Start(ctx, v, harness.StartOptions{}); err != nil {
			rep.Check("Restart "+cl.Names().Voter(v), "healthy again", err.Error(), false)
			return err
		}
		rep.Check("Restart "+cl.Names().Voter(v), "healthy again", fmt.Sprintf("healthy after %s", time.Since(restartAt).Round(time.Second)), true)
		if err := requireLogin(ctx, env, cl, v); err != nil {
			return err
		}
	}
	list, err := cl.Members(ctx, leader)
	voters := 0
	for _, m := range list.Nodes {
		if m.Voter {
			voters++
		}
	}
	rep.Check("Membership after the restart", fmt.Sprintf("%d voters", n),
		fmt.Sprintf("%d voters of %d members%s", voters, len(list.Nodes), errSuffix(err)), err == nil && voters == n)

	// Every confirmed write on every voter.
	var missing []string
	okWrites, _ := pollUntil(ctx, 120*time.Second, 2*time.Second, func() (bool, error) {
		missing = missing[:0]
		for _, i := range slots {
			c := cl.Client(i)
			mon, err := c.GetMonitor(ctx, monA.ID)
			if err != nil {
				missing = append(missing, fmt.Sprintf("voter %d: monitor A: %v", i, err))
				continue
			}
			if mon.Name != updName {
				missing = append(missing, fmt.Sprintf("voter %d: monitor A named %q", i, mon.Name))
			}
			wins, err := c.ListMaintenance(ctx)
			if err != nil {
				missing = append(missing, fmt.Sprintf("voter %d: maintenance: %v", i, err))
				continue
			}
			hasWin := false
			for _, w := range wins {
				if w.ID == win.ID {
					hasWin = true
				}
			}
			if !hasWin {
				missing = append(missing, fmt.Sprintf("voter %d: maintenance window", i))
			}
		}
		return len(missing) == 0, nil
	})
	rep.Check("All confirmed writes on all voters", "monitor update and maintenance window present on every voter (NFA-REL-04)",
		fmt.Sprintf("complete: %v %v", okWrites, firstN(missing, 4)), okWrites)

	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 120*time.Second)
	rep.Check("Snapshots of all voters equal after the restart", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)

	// NFA-REL-05: heartbeats the killed voters produced before the kill.
	// Each survivor must still have what it received; every voter must
	// have all of them after replication.
	for _, s := range survivors {
		lost := 0
		total := 0
		for _, m := range ids {
			for _, v := range victims {
				want := baseline[hbKey{s, indexOf(ids, m), v}]
				total += len(want)
				keys, err := heartbeatKeys(ctx, cl, s, m, nodes[v], created, time.Now())
				if err != nil {
					return err
				}
				lost += missingKeys(want, keys)
			}
		}
		rep.Check(fmt.Sprintf("Heartbeats received by survivor voter %d still present", s),
			"every heartbeat of the killed voters seen before the kill is listed (NFA-REL-05)",
			fmt.Sprintf("%d of %d missing", lost, total), lost == 0)
	}
	lostAll := make(map[int]int)
	okHB, _ := pollUntil(ctx, 120*time.Second, 2*time.Second, func() (bool, error) {
		done := true
		for _, i := range slots {
			lostAll[i] = 0
			for _, m := range ids {
				for _, v := range victims {
					keys, err := heartbeatKeys(ctx, cl, i, m, nodes[v], created, time.Now())
					if err != nil {
						return false, err
					}
					lostAll[i] += missingKeys(union[m][v], keys)
				}
			}
			if lostAll[i] > 0 {
				done = false
			}
		}
		return done, nil
	})
	var parts []string
	for _, i := range slots {
		parts = append(parts, fmt.Sprintf("voter %d: %d missing", i, lostAll[i]))
	}
	rep.Check("Heartbeats of the killed voters on every voter after replication",
		"all pre-kill heartbeats of the killed voters present on every voter",
		strings.Join(parts, ", "), okHB)
	return nil
}

// indexOf returns the index of s in list, or -1.
func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

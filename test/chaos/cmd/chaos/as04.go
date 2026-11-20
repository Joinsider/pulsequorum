// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
	"github.com/joinsider/pulsequorum/test/internal/fake"
	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

// as04Monitors is the number of HTTP monitors that fail during the partition.
const as04Monitors = 2

// as04DegradedWait bounds the time until the isolated follower reports
// degraded: DEGRADED_AFTER is 15 s (ADR 0024).
const as04DegradedWait = 90 * time.Second

// as04HealWait bounds the time until the leader has judged the unconfirmed
// alerts. The judgement needs confirmed history after each alert (ADR 0024, 6).
const as04HealWait = 6 * time.Minute

// as04UnconfirmedPrefix is the title prefix of an unconfirmed alert of a
// minority of one of three voters (ADR 0024, FA-ALR-10).
const as04UnconfirmedPrefix = "⚠ unconfirmed – cluster degraded (1/3 nodes) – "

var judgedCount = regexp.MustCompile(`"count":(\d+)`)

func init() {
	register(Scenario{
		ID:    "as04",
		Label: "AS-04",
		Title: "Partition 2|1, Ziel fällt aus: Mehrheit bestätigt, Minderheit unbestätigt, Heilung ohne Duplikate",
		Run:   runAS04,
	})
	// Variant with bundling off: the voters get ALERT_BUNDLE_WINDOW=0s, so
	// every alert leaves at once instead of waiting for the bundle window.
	register(Scenario{
		ID:    "as04nb",
		Label: "AS-04 (ALERT_BUNDLE_WINDOW=0s)",
		Title: "Wie AS-04, Alert-Bündelung aus (ALERT_BUNDLE_WINDOW=0s, ADR 0023)",
		Run: func(ctx context.Context, env *Env) error {
			env.Cfg.AlertBundleWindow = "0s"
			return runAS04(ctx, env)
		},
	})
}

// runAS04 automates AS-04 (docs/pflichtenheft.md, NFA-REL-03, NFA-REL-06). A
// follower is isolated (2|1) and the minority is degraded. Then the target of
// the monitors fails and stays down through the heal. The majority must send
// exactly one confirmed DOWN per monitor within (retries+1)×interval+30 s, the
// minority one unconfirmed DOWN per monitor. After the heal the minority is no
// longer degraded, no false alarm is sent and the snapshots are equal. Only
// after that the target recovers, and every monitor sends exactly one UP.
func runAS04(ctx context.Context, env *Env) error {
	rep := env.Rep
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n != 3 {
		rep.Check("Voter slots", "3 voters (partition 2|1)", fmt.Sprint(n), false)
		return errors.New("AS-04 needs exactly three voter slots")
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
	ch, err := api.CreateChannel(ctx, "as04-ntfy-"+suffix, env.NtfyURL("as04-"+suffix), []string{"down", "up"})
	if err != nil {
		rep.Check("Notification channel created", "201, events down and up", err.Error(), false)
		return err
	}
	rep.Check("Notification channel created", "201, events down and up", "channel "+ch.ID, true)
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

	env.Target.SetFailing(false)
	names := make([]string, 0, as04Monitors)
	ids := make([]string, 0, as04Monitors)
	created := time.Now()
	for k := 1; k <= as04Monitors; k++ {
		name := fmt.Sprintf("as04-%s-m%d", suffix, k)
		m, err := createPolicyMonitor(ctx, api, name, env.TargetURL(fmt.Sprintf("/as04/%s/m%d", suffix, k)), intervalSec, []string{ch.ID}, "majority")
		if err != nil {
			rep.Check("HTTP monitors created", "201, assignment all, policy majority", err.Error(), false)
			return err
		}
		names = append(names, m.Name)
		ids = append(ids, m.ID)
	}
	rep.Check("HTTP monitors created", fmt.Sprintf("%d monitors, assignment all, policy majority, interval %ds, retries %d", as04Monitors, intervalSec, as03Retries),
		fmt.Sprintf("%d monitors", len(ids)), len(ids) == as04Monitors)

	up, _ := waitMonitorStatus(ctx, api, ids, "UP", 2*interval+replicationRound)
	rep.Check("Monitors UP before the partition", "all monitors UP", fmt.Sprintf("all UP: %v", up), up)
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
	rep.Check("Every voter checks every monitor before the partition", "heartbeats of all voters for all monitors",
		fmt.Sprintf("complete: %v", allSeen), allSeen)

	oldSlot, oldLeader, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("One leader before the partition", "identical leader ID on all voters", err.Error(), false)
		return err
	}
	minority := without(slots, oldSlot)[0]
	majority := without(slots, minority)
	minorityID := nodes[minority]
	rep.Check("Layout before the partition", "isolated node is a follower, majority holds the leader",
		fmt.Sprintf("leader voter %d (node %s), isolated follower voter %d (node %s), majority %v",
			oldSlot, oldLeader, minority, minorityID, majority), true)

	// Partition 2|1: the isolated follower loses both directions to the others.
	t0 := time.Now()
	if err := cl.Isolate(ctx, minority); err != nil {
		rep.Check("Isolate follower "+cl.Names().Voter(minority), "DROP rules to the other voters, both directions", err.Error(), false)
		return err
	}
	rep.Check("Isolate follower "+cl.Names().Voter(minority), "DROP rules to the other voters, both directions", "rules installed", true)
	reach, err := cl.Reachable(ctx, majority[0], minority, partitionPort)
	rep.Check("Majority cannot reach the isolated follower", "TCP connect fails",
		fmt.Sprintf("reachable: %v%s", reach, errSuffix(err)), err == nil && !reach)

	var lastMinorityHealth ucapi.Health
	degraded, err := pollUntil(ctx, as04DegradedWait, 500*time.Millisecond, func() (bool, error) {
		h, err := cl.Client(minority).Health(ctx)
		if err != nil {
			return false, err
		}
		lastMinorityHealth = h
		return h.Degraded && len(h.Partition) == 1 && h.Partition[0] == minorityID, nil
	})
	rep.Check("Isolated follower is degraded", "health degraded true, partition = [itself]",
		fmt.Sprintf("degraded %v after %s, partition %v%s", degraded, time.Since(t0).Round(time.Second),
			lastMinorityHealth.Partition, errSuffixIf(!degraded)+errSuffix(err)), degraded)
	if !degraded {
		if err != nil {
			return fmt.Errorf("minority does not turn degraded: %w", err)
		}
		return errors.New("minority does not turn degraded")
	}

	lead, lerr := cl.LeaderOf(ctx, majority)
	rep.Check("Majority keeps its leader", "leader unchanged "+oldLeader,
		fmt.Sprintf("leader %s%s", lead, errSuffix(lerr)), lerr == nil && lead == oldLeader)
	mh, herr := cl.Client(majority[0]).Health(ctx)
	rep.Check("Majority is not degraded", "degraded false", fmt.Sprintf("degraded %v%s", mh.Degraded, errSuffix(herr)),
		herr == nil && !mh.Degraded)

	// Failure: the target answers 503 and stays down through the heal.
	tFail := time.Now()
	env.Target.SetFailing(true)
	dueBy := tFail.Add(time.Duration(as03Retries+1)*interval + 30*time.Second)
	allAlerted, _ := pollUntil(ctx, time.Until(dueBy)+5*time.Second, time.Second, func() (bool, error) {
		for _, name := range names {
			if c, _ := notifyCount(env.Ntfy, tFail, "DOWN: "+name); c == 0 {
				return false, nil
			}
			if c, _ := notifyCount(env.Ntfy, tFail, as04UnconfirmedPrefix+"DOWN: "+name); c == 0 {
				return false, nil
			}
		}
		return true, nil
	})
	// Give duplicates one more interval to show up.
	if err := waitUntil(ctx, time.Now().Add(interval)); err != nil {
		return err
	}
	for k, name := range names {
		c, at := notifyCount(env.Ntfy, tFail, "DOWN: "+name)
		// Two times for NFA-REL-03: the status change in the replicated state
		// (StatusSince of the majority's view) and the delivery of the alert.
		changed := "status change unknown"
		if mon, err := cl.Client(majority[0]).GetMonitor(ctx, ids[k]); err == nil {
			changed = fmt.Sprintf("status change %s (status %s)", mon.StatusSince.Sub(tFail).Round(time.Second), mon.Status)
		}
		rep.Check("Majority DOWN for "+name, fmt.Sprintf("exactly 1 confirmed, delivered by +%s (NFA-REL-03)", dueBy.Sub(tFail).Round(time.Second)),
			fmt.Sprintf("%d DOWN, delivered +%s; %s", c, firstSince(at, tFail), changed), c == 1 && len(at) > 0 && !at[0].After(dueBy))
		rep.Info("Messages mentioning "+name+" after the failure", "raw titles and arrival offsets",
			messagesMentioning(env.Ntfy, tFail, name))
	}
	if !allAlerted {
		rep.Info("Alerts of the failure within the due time", "all DOWN alerts by the due time", "not all sent in time")
	}
	for _, name := range names {
		c, at := notifyCount(env.Ntfy, tFail, as04UnconfirmedPrefix+"DOWN: "+name)
		rep.Check("Minority DOWN for "+name+" carries the unconfirmed prefix", "exactly 1, title '"+strings.TrimSuffix(as04UnconfirmedPrefix, " – ")+"', View line of the isolated follower",
			fmt.Sprintf("%d unconfirmed DOWN, first +%s, view of isolated follower: %v", c, firstSince(at, tFail), viewOf(env.Ntfy, tFail, unconfirmedDown(name), minorityID)),
			c == 1 && viewOf(env.Ntfy, tFail, unconfirmedDown(name), minorityID))
	}
	viewClash := 0
	for _, name := range names {
		k, _ := countWhere(env.Ntfy, tFail, func(m fake.Message) bool {
			return m.Title == "DOWN: "+name && strings.Contains(m.Body, "View: "+minorityID+" ·")
		})
		viewClash += k
	}
	rep.Check("Confirmed DOWN not sent by the isolated follower", "0 confirmed DOWN with the minority's View line",
		fmt.Sprint(viewClash), viewClash == 0)

	// Heal: the isolated follower rejoins; the target stays down.
	healAt := time.Now()
	if err := cl.Heal(ctx, minority); err != nil {
		rep.Check("Heal the isolated follower", "partition rules removed", err.Error(), false)
		return err
	}
	agreed, _ := pollUntil(ctx, 90*time.Second, 250*time.Millisecond, func() (bool, error) {
		id, err := cl.LeaderOf(ctx, slots)
		return err == nil && id == oldLeader, nil
	})
	rep.Check("Heal: all voters agree on the leader", "one leader on all voters",
		fmt.Sprintf("agreed: %v after %s", agreed, time.Since(healAt).Round(time.Millisecond)), agreed)
	cleared, _ := pollUntil(ctx, 90*time.Second, time.Second, func() (bool, error) {
		h, err := cl.Client(minority).Health(ctx)
		if err != nil {
			return false, err
		}
		lastMinorityHealth = h
		return !h.Degraded, nil
	})
	rep.Check("Isolated follower after the heal", "health degraded false",
		fmt.Sprintf("degraded false: %v after %s", cleared, time.Since(healAt).Round(time.Second)), cleared)

	// The leader judges the unconfirmed alerts once the history covers them.
	judged, judgedN, errLines := awaitJudgement(ctx, cl, oldSlot, as04HealWait)
	rep.Check("Leader judges the unconfirmed alerts", fmt.Sprintf("%d judged in total (one per unconfirmed alert)", as04Monitors),
		fmt.Sprintf("%d judged after %s", judgedN, time.Since(healAt).Round(time.Second)), judged && judgedN == as04Monitors)
	rep.Check("No failure while judging", "no 'resolve unconfirmed alerts' error in the leader log",
		fmt.Sprint(errLines), errLines == 0)

	falseAlarms, _ := countWhere(env.Ntfy, tFail, func(m fake.Message) bool {
		return strings.Contains(m.Title, "False alarm") || strings.Contains(m.Body, "False alarm") ||
			strings.Contains(m.Title, "Fehlalarm") || strings.Contains(m.Body, "Fehlalarm")
	})
	rep.Check("No false alarm reaches ntfy", "0 'False alarm' messages", fmt.Sprint(falseAlarms), falseAlarms == 0)
	confirmed, _ := countWhere(env.Ntfy, healAt, func(m fake.Message) bool { return strings.HasPrefix(m.Title, "Confirmed:") })
	rep.Info("Verdict messages 'Confirmed' after the heal", "0 expected: the majority already reported the change (ADR 0024, 6)",
		fmt.Sprint(confirmed))

	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 120*time.Second)
	rep.Check("Snapshots of all voters equal after the heal", "normalized snapshots equal",
		snapshotSummary(equal, diffs), equal)
	rep.Info("Snapshot scope", "unconfirmed_alerts are not part of the API snapshot",
		"the snapshot covers monitors, channels, maintenance, users and members; the verdicts are seen through the logs and ntfy")

	// Duplicates after the heal: still exactly one DOWN per monitor.
	for _, name := range names {
		c, _ := notifyCount(env.Ntfy, tFail, "DOWN: "+name)
		rep.Check("No confirmed DOWN repeated after the heal for "+name, "exactly 1 DOWN in total",
			fmt.Sprint(c), c == 1)
	}

	// Target recovers: exactly one UP per monitor, from the majority.
	tRec := time.Now()
	env.Target.SetFailing(false)
	recovered, _ := waitMonitorStatus(ctx, cl.Client(majority[0]), ids, "UP", 2*interval+replicationRound)
	rep.Check("Monitors UP again after recovery", "all monitors UP", fmt.Sprintf("all UP: %v", recovered), recovered)
	// UP alerts follow the replicated heartbeats, which lag by up to one round.
	if err := waitUntil(ctx, time.Now().Add(2*interval+replicationRound)); err != nil {
		return err
	}
	for _, name := range names {
		c, at := notifyCount(env.Ntfy, tRec, "UP: "+name)
		rep.Check("UP notification for "+name, "exactly 1 after recovery",
			fmt.Sprintf("%d UP, first +%s", c, firstSince(at, tRec)), c == 1)
		u, _ := notifyCount(env.Ntfy, tRec, as04UnconfirmedPrefix+"UP: "+name)
		d, _ := notifyCount(env.Ntfy, tRec, "DOWN: "+name)
		rep.Check("No unconfirmed UP and no DOWN after recovery for "+name, "0 and 0",
			fmt.Sprintf("%d unconfirmed UP, %d DOWN", u, d), u == 0 && d == 0)
	}
	return nil
}

// createPolicyMonitor creates an HTTP monitor against the fake target with
// assignment all, retries 1, the given channels and consensus policy.
func createPolicyMonitor(ctx context.Context, c *ucapi.Client, name, url string, intervalSec int, channels []string, policy string) (ucapi.Monitor, error) {
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
		ConsensusPolicy:        policy,
		NotificationChannelIDs: channels,
	})
}

// countWhere counts the recorded messages at or after since that match pred,
// and returns their arrival times.
func countWhere(rec *fake.Recorder, since time.Time, pred func(fake.Message) bool) (int, []time.Time) {
	var n int
	var at []time.Time
	for _, m := range rec.Messages() {
		if !m.At.Before(since) && pred(m) {
			n++
			at = append(at, m.At)
		}
	}
	return n, at
}

// unconfirmedDown matches the unconfirmed DOWN alert of one monitor sent by
// the minority (ADR 0024).
func unconfirmedDown(name string) func(fake.Message) bool {
	return func(m fake.Message) bool {
		return m.Title == as04UnconfirmedPrefix+"DOWN: "+name
	}
}

// viewOf reports whether the matching messages carry the View line of node id
// with a 1/3 partition, as the minority's alerts do.
func viewOf(rec *fake.Recorder, since time.Time, pred func(fake.Message) bool, id string) bool {
	want := "View: " + id + " · 1/3 voters"
	found := false
	for _, m := range rec.Messages() {
		if !m.At.Before(since) && pred(m) {
			if !strings.Contains(m.Body, want) {
				return false
			}
			found = true
		}
	}
	return found
}

// awaitJudgement polls the log of voter slot for the leader's judgement of the
// unconfirmed alerts. It returns whether a judgement line was seen, the sum of
// the judged counts and the number of resolve errors in the log.
func awaitJudgement(ctx context.Context, cl *harness.Cluster, slot int, timeout time.Duration) (bool, int, int) {
	var judged, total, errs int
	_, _ = pollUntil(ctx, timeout, 5*time.Second, func() (bool, error) {
		logs, err := cl.ContainerLogs(ctx, cl.Names().Voter(slot))
		if err != nil {
			return false, err
		}
		total, judged, errs = 0, 0, 0
		for _, line := range strings.Split(logs, "\n") {
			switch {
			case strings.Contains(line, "degraded: judged unconfirmed alerts"):
				judged++
				if m := judgedCount.FindStringSubmatch(line); m != nil {
					k, _ := strconv.Atoi(m[1])
					total += k
				}
			case strings.Contains(line, "degraded: resolve unconfirmed alerts"):
				errs++
			}
		}
		return total >= as04Monitors, nil
	})
	return judged > 0, total, errs
}

// errSuffixIf formats the last error text of a poll when cond holds.
func errSuffixIf(cond bool) string {
	if cond {
		return " (not reached)"
	}
	return ""
}

// messagesMentioning lists the recorded messages that mention name since
// since, with their arrival offsets and raw titles (a bundle has a count title).
func messagesMentioning(rec *fake.Recorder, since time.Time, name string) string {
	var parts []string
	for _, m := range rec.Messages() {
		if !m.At.Before(since) && (strings.Contains(m.Title, name) || strings.Contains(m.Body, name)) {
			parts = append(parts, fmt.Sprintf("+%s %q", m.At.Sub(since).Round(time.Second), m.Title))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/fake"
	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// as05SystemLimit bounds the time from the block until the system alert
// node_impaired is delivered. The impairment needs two signal rounds of about
// 15 s each plus the deviation slots (ADR 0022, "Konsequenzen": about 30 s and
// more), and the system alert follows on the next evaluation round, 15 to 30 s
// later (ADR 0025). The first draft used 60 s; that was a harness guess, not a
// spec value. Two minutes covers both rounds with margin.
const as05SystemLimit = 2 * time.Minute

// as05ImpairedLimit bounds the time until the cluster API shows the node
// impaired. The hysteresis is two evaluations of 15 s (ADR 0022).
const as05ImpairedLimit = 2 * time.Minute

// as05ClearLimit bounds the time until the impairment and its system alert
// clear: about 2 min of hysteresis plus evaluation (ADR 0022), allow 6 min.
const as05ClearLimit = 6 * time.Minute

func init() {
	register(Scenario{
		ID:    "as05",
		Label: "AS-05",
		Title: "Knoten mit gesperrtem Egress: kein DOWN für Ziele, Systemalert node_impaired, Ausschluss und Rückkehr",
		Run:   runAS05,
	})
}

// runAS05 automates AS-05 (FA-CON-04, ADR 0022). One follower loses its egress
// to the monitor target and the reference target (iptables OUTPUT, only these
// two ports); the cluster traffic stays. The majority monitors must not send a
// DOWN. The "any" monitor may. The leader marks the node connectivity impaired,
// raises the system alert node_impaired on the system topic, and clears both
// after the egress is back. The target itself stays healthy throughout, so
// every DOWN alert of the run would be a false alarm.
func runAS05(ctx context.Context, env *Env) error {
	rep := env.Rep
	// Every voter probes the reference target; the blocked node fails it (ADR 0022).
	env.Cfg.SelfCheckTargets = []string{env.RefURL("/healthz")}
	env.RefTarget.SetFailing(false)
	env.Target.SetFailing(false)
	cfg := env.Cfg
	interval := cfg.Interval
	intervalSec := int(interval / time.Second)
	n := cfg.Voters
	slots := voterRange(n)
	if n < 3 {
		rep.Check("Voter slots", "at least 3", fmt.Sprint(n), false)
		return errors.New("AS-05 needs at least three voter slots")
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
	sysTopic := "as05-system-" + suffix
	// The system tenant is created by the leader's system job, a few seconds
	// after the bootstrap: a 404 before that is expected and measured.
	sysStart := time.Now()
	var sys ucapi.NotificationChannel
	sysNotFound, sysUnavailable := 0, 0
	sysOK, sysErr := pollUntil(ctx, 90*time.Second, time.Second, func() (bool, error) {
		ch, err := api.CreateSystemChannel(ctx, "as05-system-"+suffix, env.NtfyURL(sysTopic), []string{"system"})
		if err != nil {
			switch ucapi.StatusCode(err) {
			case 404:
				sysNotFound++
			case 503: // Retry-After while the _system tenant does not exist yet
				sysUnavailable++
			}
			return false, err
		}
		sys = ch
		return true, nil
	})
	rep.Info("System channel POST before the system tenant exists", "404 or 503 (Retry-After) until the system job creates the tenant",
		fmt.Sprintf("%d HTTP 404 and %d HTTP 503 answers, created after %s", sysNotFound, sysUnavailable, time.Since(sysStart).Round(time.Second)))
	if !sysOK {
		rep.Check("System channel created", "201, event system", errSuffix(sysErr), false)
		return errors.New("system channel not created")
	}
	rep.Check("System channel created", "201, event system", fmt.Sprintf("channel %s, events %v", sys.ID, sys.Events),
		len(sys.Events) == 1 && sys.Events[0] == "system")
	sysRes, err := api.TestSystemChannel(ctx, sys.ID)
	if err != nil || !sysRes.Delivered {
		rep.Check("System channel test delivery", "delivered to ntfy", fmt.Sprintf("%+v%s", sysRes, errSuffix(err)), false)
		return errors.New("system channel test delivery failed")
	}
	rep.Check("System channel test delivery", "delivered to ntfy", fmt.Sprintf("delivered in %d ms", sysRes.DurationMs), true)

	ch, err := api.CreateChannel(ctx, "as05-ntfy-"+suffix, env.NtfyURL("as05-"+suffix), []string{"down", "up"})
	if err != nil {
		rep.Check("Notification channel created", "201, events down and up", err.Error(), false)
		return err
	}
	res, err := api.TestChannel(ctx, ch.ID)
	if err != nil || !res.Delivered {
		rep.Check("Channel test delivery", "delivered to ntfy", fmt.Sprintf("%+v%s", res, errSuffix(err)), false)
		return errors.New("channel test delivery failed")
	}
	rep.Check("Channel test delivery", "delivered to ntfy", fmt.Sprintf("delivered in %d ms", res.DurationMs), true)

	// Three monitors: one with policy any, two with policy majority.
	policies := []struct{ key, policy string }{{"any", "any"}, {"maj1", "majority"}, {"maj2", "majority"}}
	var names, ids []string
	created := time.Now()
	for _, p := range policies {
		name := fmt.Sprintf("as05-%s-%s", suffix, p.key)
		m, err := createPolicyMonitor(ctx, api, name, env.TargetURL("/as05/"+suffix+"/"+p.key), intervalSec, []string{ch.ID}, p.policy)
		if err != nil {
			rep.Check("HTTP monitors created", "201, assignment all", err.Error(), false)
			return err
		}
		names = append(names, m.Name)
		ids = append(ids, m.ID)
	}
	anyName, majNames := names[0], names[1:]
	rep.Check("HTTP monitors created", fmt.Sprintf("3 monitors (policy any: 1, majority: 2), assignment all, interval %ds, retries %d", intervalSec, as03Retries),
		fmt.Sprintf("%d monitors", len(ids)), len(ids) == 3)

	up, _ := waitMonitorStatus(ctx, api, ids, "UP", 2*interval+replicationRound)
	rep.Check("Monitors UP before the fault", "all monitors UP", fmt.Sprintf("all UP: %v", up), up)
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
	rep.Check("Every voter checks every monitor before the fault", "heartbeats of all voters for all monitors",
		fmt.Sprintf("complete: %v", allSeen), allSeen)

	// Baseline: the self-checks reach the reference target, and nothing is impaired.
	refBefore := env.RefTarget.Hits("/healthz")
	if err := waitUntil(ctx, time.Now().Add(65*time.Second)); err != nil {
		return err
	}
	refHits := env.RefTarget.Hits("/healthz") - refBefore
	rep.Check("Self-checks reach the reference target before the fault", "at least 3 hits on /healthz in 65 s (3 voters, 30 s rounds)",
		fmt.Sprintf("%d hits", refHits), refHits >= n)
	pre, err := api.ListClusterNodes(ctx)
	if err != nil {
		rep.Check("Cluster nodes before the fault", "answered", err.Error(), false)
		return err
	}
	impairedBefore := impairedNodes(pre)
	rep.Check("No node impaired before the fault", "connectivity ok on all voters", fmt.Sprintf("impaired: %v", impairedBefore), len(impairedBefore) == 0)
	preAlerts, _ := api.ListSystemAlerts(ctx, false)
	rep.Check("No node_impaired system alert before the fault", "none active", fmt.Sprintf("%d active system alerts", len(preAlerts)),
		countKind(preAlerts, "node_impaired") == 0)

	// The blocked node is a follower, so the leader stays put.
	leader, leaderID, err := leaderSlot(ctx, cl)
	if err != nil {
		rep.Check("One leader before the fault", "identical leader ID", err.Error(), false)
		return err
	}
	blocked := without(slots, leader)[0]
	others := without(slots, blocked)
	blockedID := nodes[blocked]
	rep.Check("Layout before the fault", "blocked node is a follower",
		fmt.Sprintf("leader voter %d (node %s), blocked follower voter %d (node %s)", leader, leaderID, blocked, blockedID), true)

	hostIP, err := cl.HostIP(ctx, blocked, cl.CallbackHost())
	rep.Check("Callback host resolved on the blocked voter", "IPv4 address", fmt.Sprintf("%s%s", hostIP, errSuffix(err)), err == nil)
	if err != nil {
		return err
	}
	ports := []int{env.TargetPort, env.RefPort}

	// Fault: egress of the follower to the target and the reference target only.
	t0 := time.Now()
	if err := cl.BlockEgress(ctx, blocked, hostIP, ports); err != nil {
		rep.Check("Block egress of "+cl.Names().Voter(blocked), "OUTPUT DROP to the host on target and reference ports", err.Error(), false)
		return err
	}
	rep.Check("Block egress of "+cl.Names().Voter(blocked), "OUTPUT DROP to the host on target and reference ports, cluster traffic kept",
		fmt.Sprintf("ports %v to %s", ports, hostIP), true)

	// Observe until both signals are seen and at least 90 s have passed, or 2 min.
	var impairedAt, sysAt time.Time
	var reason string
	for {
		if err := waitUntil(ctx, time.Now().Add(2*time.Second)); err != nil {
			return err
		}
		if impairedAt.IsZero() {
			if list, err := cl.Client(others[0]).ListClusterNodes(ctx); err == nil {
				if node, ok := findNode(list, blockedID); ok && node.ConnectivityImpaired {
					impairedAt, reason = time.Now(), node.ConnectivityReason
				}
			}
		}
		if sysAt.IsZero() {
			if k, at := countWhere(env.Ntfy, t0, nodeImpairedAlert(sysTopic, blockedID, false)); k > 0 {
				sysAt = at[0]
			}
		}
		done := !impairedAt.IsZero() && !sysAt.IsZero() && time.Since(t0) >= 90*time.Second
		if done || time.Since(t0) >= as05ImpairedLimit+30*time.Second {
			break
		}
	}
	impairedDur := sinceOrNone(impairedAt, t0)
	rep.Check("Cluster API shows the node connectivity impaired with a reason",
		fmt.Sprintf("connectivityImpaired true, reason set, within %s", as05ImpairedLimit),
		fmt.Sprintf("impaired after %s, reason %q", impairedDur, reason), !impairedAt.IsZero() && impairedAt.Sub(t0) <= as05ImpairedLimit && reason != "")
	rep.Check("System alert node_impaired on the system topic", fmt.Sprintf("within %s (ADR 0022, ADR 0025)", as05SystemLimit),
		fmt.Sprintf("alert after %s", sinceOrNone(sysAt, t0)), !sysAt.IsZero() && sysAt.Sub(t0) <= as05SystemLimit)

	post, err := cl.Client(others[0]).ListClusterNodes(ctx)
	if err == nil {
		b, _ := findNode(post, blockedID)
		raftOK := b.Healthy != nil && *b.Healthy
		rep.Check("Raft health of the blocked node unchanged", "healthy true (connectivity is a separate dimension, ADR 0022)",
			fmt.Sprintf("healthy %v", raftOK), raftOK)
		otherImpaired := 0
		for _, o := range others {
			if list, err := cl.Client(o).ListClusterNodes(ctx); err == nil {
				for _, nd := range list.Nodes {
					if nd.ID != blockedID && nd.ConnectivityImpaired {
						otherImpaired++
					}
				}
			}
		}
		rep.Check("Other voters not excluded", "no other node impaired", fmt.Sprint(otherImpaired), otherImpaired == 0)
	}
	lead, lerr := cl.LeaderOf(ctx, slots)
	rep.Check("No leader change while the egress is blocked", "leader unchanged "+leaderID,
		fmt.Sprintf("leader %s%s", lead, errSuffix(lerr)), lerr == nil && lead == leaderID)

	majDown, _ := countWhere(env.Ntfy, t0, downOf(majNames))
	rep.Check("No DOWN for the majority monitors while the egress is blocked", "0 DOWN (target healthy)", fmt.Sprint(majDown), majDown == 0)
	anyDown, anyAt := countWhere(env.Ntfy, t0, downOf([]string{anyName}))
	rep.Info("DOWN for the 'any' monitor during the block",
		"ADR 0022 Konsequenzen: bei any kann bis zum Ausschluss ein falsches DOWN entstehen; reported, not judged",
		fmt.Sprintf("%d DOWN, first +%s, impaired at +%s", anyDown, firstSince(anyAt, t0), sinceOrNone(impairedAt, t0)))

	// Recovery: egress back; the impairment and its alert clear.
	t1 := time.Now()
	if err := cl.UnblockEgress(ctx, blocked, hostIP, ports); err != nil {
		rep.Check("Unblock egress of "+cl.Names().Voter(blocked), "rules removed", err.Error(), false)
		return err
	}
	rep.Check("Unblock egress of "+cl.Names().Voter(blocked), "rules removed", "rules removed", true)
	var clearedAt, clearMsgAt time.Time
	for {
		if err := waitUntil(ctx, time.Now().Add(3*time.Second)); err != nil {
			return err
		}
		if clearedAt.IsZero() {
			if list, err := cl.Client(others[0]).ListClusterNodes(ctx); err == nil {
				if node, ok := findNode(list, blockedID); ok && !node.ConnectivityImpaired {
					clearedAt = time.Now()
				}
			}
		}
		if clearMsgAt.IsZero() {
			if k, at := countWhere(env.Ntfy, t1, nodeImpairedAlert(sysTopic, blockedID, true)); k > 0 {
				clearMsgAt = at[0]
			}
		}
		if (!clearedAt.IsZero() && !clearMsgAt.IsZero()) || time.Since(t1) >= as05ClearLimit {
			break
		}
	}
	rep.Check("Connectivity impaired cleared", fmt.Sprintf("connectivityImpaired false within %s (hysteresis 2 min)", as05ClearLimit),
		fmt.Sprintf("cleared after %s", sinceOrNone(clearedAt, t1)), !clearedAt.IsZero())
	rep.Check("System alert node_impaired cleared on the system topic", fmt.Sprintf("'cleared' message within %s", as05ClearLimit),
		fmt.Sprintf("cleared message after %s", sinceOrNone(clearMsgAt, t1)), !clearMsgAt.IsZero())
	active, err := api.ListSystemAlerts(ctx, false)
	rep.Check("Active system alerts no longer list node_impaired", "0 active node_impaired for the blocked node",
		fmt.Sprintf("%d%s", countKind(active, "node_impaired"), errSuffix(err)), err == nil && countKind(active, "node_impaired") == 0)

	// The whole run: no DOWN for the majority monitors, and every monitor back to UP.
	majDownAll, _ := countWhere(env.Ntfy, t0, downOf(majNames))
	rep.Check("No DOWN for the majority monitors during the whole run", "0 DOWN", fmt.Sprint(majDownAll), majDownAll == 0)
	anyDownAll, _ := countWhere(env.Ntfy, t0, downOf([]string{anyName}))
	anyUp, _ := countWhere(env.Ntfy, t0, func(m fake.Message) bool { return m.Title == "UP: "+anyName })
	rep.Info("'any' monitor after the run", "information only",
		fmt.Sprintf("%d DOWN and %d UP alerts in total", anyDownAll, anyUp))
	allUp, _ := waitMonitorStatus(ctx, api, ids, "UP", 2*interval+30*time.Second)
	rep.Check("All monitors UP at the end", "all monitors UP", fmt.Sprintf("all UP: %v", allUp), allUp)
	other := 0
	for _, r := range env.Ntfy.Messages() {
		if !r.At.Before(t0) && r.Path == "/"+sysTopic && !strings.Contains(r.Title, "node_impaired") {
			other++
		}
	}
	rep.Info("Other system alerts during the run", "none expected", fmt.Sprint(other))

	equal, diffs, _ := awaitSnapshots(ctx, cl, slots, 120*time.Second)
	rep.Check("Snapshots of all voters equal at the end", "normalized snapshots equal", snapshotSummary(equal, diffs), equal)
	return nil
}

// findNode returns the cluster node with the given ID as seen in list.
func findNode(list ucapi.ClusterNodeList, id string) (ucapi.ClusterNode, bool) {
	for _, nd := range list.Nodes {
		if nd.ID == id {
			return nd, true
		}
	}
	return ucapi.ClusterNode{}, false
}

// impairedNodes returns the IDs of the nodes whose connectivity is impaired.
func impairedNodes(list ucapi.ClusterNodeList) []string {
	var out []string
	for _, nd := range list.Nodes {
		if nd.ConnectivityImpaired {
			out = append(out, nd.ID)
		}
	}
	return out
}

// countKind counts the alerts of one kind.
func countKind(alerts []ucapi.SystemAlert, kind string) int {
	n := 0
	for _, a := range alerts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

// nodeImpairedAlert matches the system alert of one node on the system topic:
// the active alert, or the cleared one.
func nodeImpairedAlert(topic, nodeID string, cleared bool) func(fake.Message) bool {
	return func(m fake.Message) bool {
		if m.Path != "/"+topic || !strings.Contains(m.Title, "node_impaired") || !strings.Contains(m.Title, nodeID) {
			return false
		}
		return strings.Contains(m.Title, "cleared") == cleared
	}
}

// downOf matches the DOWN alerts of the given monitors.
func downOf(names []string) func(fake.Message) bool {
	return func(m fake.Message) bool {
		for _, name := range names {
			if m.Title == "DOWN: "+name {
				return true
			}
		}
		return false
	}
}

// sinceOrNone formats the offset of t from since, or "none".
func sinceOrNone(t, since time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.Sub(since).Round(time.Second).String()
}

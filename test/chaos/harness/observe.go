// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// agreeLeader returns the leader ID that all answers agree on. An empty
// answer means "no leader known" and is an error, as is disagreement.
func agreeLeader(answers map[int]string) (string, error) {
	if len(answers) == 0 {
		return "", errors.New("no voter answered")
	}
	idx := make([]int, 0, len(answers))
	for i := range answers {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	leader := ""
	for _, i := range idx {
		id := answers[i]
		if id == "" {
			return "", fmt.Errorf("voter %d knows no leader", i)
		}
		if leader == "" {
			leader = id
		} else if id != leader {
			return "", fmt.Errorf("voters disagree on the leader: voter %d says %q, voter %d says %q",
				idx[0], leader, i, id)
		}
	}
	return leader, nil
}

// LeaderOf asks the given voters for the leader and returns it only if every
// voter that answers agrees. Voters that do not answer are skipped; if none
// answers, it is an error.
func (c *Cluster) LeaderOf(ctx context.Context, voters []int) (string, error) {
	answers := make(map[int]string, len(voters))
	for _, i := range voters {
		list, err := c.Client(i).ListClusterNodes(ctx)
		if err != nil {
			// An unreachable voter does not vote; agreement is judged on the rest.
			continue
		}
		answers[i] = list.LeaderID
	}
	leader, err := agreeLeader(answers)
	if err != nil {
		return "", err
	}
	return leader, nil
}

// Leader returns the leader as seen by all running voters. See LeaderOf.
func (c *Cluster) Leader(ctx context.Context) (string, error) {
	running, err := c.RunningVoters(ctx)
	if err != nil {
		return "", err
	}
	return c.LeaderOf(ctx, running)
}

// WaitLeader polls until a leader other than exclude is visible to all
// reachable running voters, or until timeout. It returns the time from the
// call until that point and the new leader's ID. Call it right after the
// old leader has been stopped or killed.
func (c *Cluster) WaitLeader(ctx context.Context, exclude string, timeout time.Duration) (time.Duration, string, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	var last error
	for {
		leader, err := c.Leader(ctx)
		if err == nil && leader != "" && leader != exclude {
			return time.Since(start), leader, nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("leader is still %q", leader)
		}
		if time.Now().After(deadline) {
			return 0, "", fmt.Errorf("no new leader within %s: %w", timeout, last)
		}
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return 0, "", err
		}
	}
}

// NodeID returns the Raft node ID of voter i, read from its own member list.
func (c *Cluster) NodeID(ctx context.Context, i int) (string, error) {
	if err := c.checkIndex(i); err != nil {
		return "", err
	}
	list, err := c.Client(i).ListClusterNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range list.Nodes {
		if n.Self {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("%s does not list itself", c.names.Voter(i))
}

// Members returns the member list as seen by voter i.
func (c *Cluster) Members(ctx context.Context, i int) (ucapi.ClusterNodeList, error) {
	if err := c.checkIndex(i); err != nil {
		return ucapi.ClusterNodeList{}, err
	}
	return c.Client(i).ListClusterNodes(ctx)
}

// HeartbeatGap returns the largest gap between heartbeats of one producing
// node for one monitor, as seen by voter viewer, within [from, to]. See
// MaxGap for the exact definition. Heartbeats outside the window are read to
// anchor the first and last gap.
func (c *Cluster) HeartbeatGap(ctx context.Context, viewer int, monitorID, producer string, from, to time.Time, interval time.Duration) (time.Duration, error) {
	if err := c.checkIndex(viewer); err != nil {
		return 0, err
	}
	pad := 3 * interval
	hbs, err := c.Client(viewer).ListHeartbeats(ctx, monitorID, ucapi.HeartbeatQuery{
		From: from.Add(-pad),
		To:   to.Add(pad),
		Node: producer,
	})
	if err != nil {
		return 0, fmt.Errorf("list heartbeats on %s: %w", c.names.Voter(viewer), err)
	}
	ts := make([]time.Time, 0, len(hbs))
	for _, h := range hbs {
		ts = append(ts, h.TS)
	}
	return MaxGap(ts, from, to)
}

// ProducerCounts returns, for one monitor and voter viewer, how many
// heartbeats each producing node has in [from, to].
func (c *Cluster) ProducerCounts(ctx context.Context, viewer int, monitorID string, from, to time.Time) (map[string]int, error) {
	if err := c.checkIndex(viewer); err != nil {
		return nil, err
	}
	hbs, err := c.Client(viewer).ListHeartbeats(ctx, monitorID, ucapi.HeartbeatQuery{From: from, To: to})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, h := range hbs {
		out[h.NodeID]++
	}
	return out, nil
}

// StatusSummary counts the heartbeats of one monitor seen by voter viewer in
// [from, to] by "status" or "status/errorClass" (for example "DOWN/connect").
// It is meant for reports and diagnosis.
func (c *Cluster) StatusSummary(ctx context.Context, viewer int, monitorID string, from, to time.Time) (map[string]int, error) {
	if err := c.checkIndex(viewer); err != nil {
		return nil, err
	}
	hbs, err := c.Client(viewer).ListHeartbeats(ctx, monitorID, ucapi.HeartbeatQuery{From: from, To: to})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, h := range hbs {
		key := h.Status
		if h.ErrorClass != "" {
			key += "/" + h.ErrorClass
		}
		out[key]++
	}
	return out, nil
}

// FormatCounts renders a count map as "key n, key n" in key order.
func FormatCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

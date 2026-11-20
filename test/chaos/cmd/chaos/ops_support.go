// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
	"github.com/joinsider/pulsequorum/test/internal/fake"
	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

// Helpers shared by the operations scenarios (AS-09, replace, recovery). They
// carry the ops prefix so that they do not depend on the helpers of other
// scenario files.

// opsLoginGrace bounds how long a (re)started voter may take to answer logins.
const opsLoginGrace = 60 * time.Second

// opsReplicationRound is the anti-entropy period of heartbeats (cluster.md).
const opsReplicationRound = 30 * time.Second

// opsWaitUntil blocks until t or until ctx ends.
func opsWaitUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// opsJoinLogin logs a joined voter in. A voter that joins before its
// replicated state has arrived creates no signing key and answers logins with
// HTTP 500 until it restarts (known product issue, see test/chaos/README.md).
// After the grace period the voter is restarted, as AS-02 does, and the
// restart is recorded as an INFO step.
func opsJoinLogin(ctx context.Context, env *Env, cl *harness.Cluster, i int) error {
	name := cl.Names().Voter(i)
	ok, err := pollUntil(ctx, opsLoginGrace, 2*time.Second, func() (bool, error) {
		if err := cl.Client(i).Login(ctx); err != nil {
			return false, err
		}
		return true, nil
	})
	if ok {
		return nil
	}
	env.Rep.Info("Login on "+name+" after join", "login answers 200",
		fmt.Sprintf("not within %s: %s; restarting %s", opsLoginGrace, opsErr(err), name))
	if err := cl.Restart(ctx, i); err != nil {
		return fmt.Errorf("restart %s: %w", name, err)
	}
	return opsLogin(ctx, cl, i)
}

// opsLogin logs voter i in, polling until it answers or the grace period ends.
// A voter that just restarted or joined may need a moment for its signing key.
func opsLogin(ctx context.Context, cl *harness.Cluster, i int) error {
	ok, err := pollUntil(ctx, opsLoginGrace, 2*time.Second, func() (bool, error) {
		if err := cl.Client(i).Login(ctx); err != nil {
			return false, err
		}
		return true, nil
	})
	if !ok {
		return fmt.Errorf("login on %s: %s", cl.Names().Voter(i), opsErr(err))
	}
	return nil
}

// opsRefreshLogins logs every listed voter in again. Access tokens expire
// during long scenarios; a fresh login per step keeps the clients usable.
func opsRefreshLogins(ctx context.Context, cl *harness.Cluster, voters []int) error {
	for _, i := range voters {
		if err := cl.Client(i).Login(ctx); err != nil {
			return fmt.Errorf("login on voter %d: %w", i, err)
		}
	}
	return nil
}

// opsErr renders an error for a report cell ("" for nil).
func opsErr(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// opsSnapshotDiffs compares the normalized snapshots of the given voters with
// the first one. An empty result means equal data.
func opsSnapshotDiffs(ctx context.Context, cl *harness.Cluster, voters []int) ([]string, error) {
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

// opsAwaitSnapshots polls until the snapshots of the voters are equal.
func opsAwaitSnapshots(ctx context.Context, cl *harness.Cluster, voters []int, timeout time.Duration) (bool, []string, error) {
	var diffs []string
	ok, err := pollUntil(ctx, timeout, 2*time.Second, func() (bool, error) {
		d, err := opsSnapshotDiffs(ctx, cl, voters)
		if err != nil {
			return false, err
		}
		diffs = d
		return len(d) == 0, nil
	})
	return ok, diffs, err
}

// opsNodeIDs returns the node IDs of the voters, read from their own lists.
func opsNodeIDs(ctx context.Context, cl *harness.Cluster, voters []int) (map[int]string, error) {
	out := make(map[int]string, len(voters))
	for _, i := range voters {
		id, err := cl.NodeID(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("node ID of voter %d: %w", i, err)
		}
		out[i] = id
	}
	return out, nil
}

// opsAllProducers reports whether every viewer has heartbeats of every
// producer for every monitor since since.
func opsAllProducers(ctx context.Context, cl *harness.Cluster, viewers []int, monitorIDs []string, producers []string, since time.Time) (bool, error) {
	for _, v := range viewers {
		for _, m := range monitorIDs {
			counts, err := cl.ProducerCounts(ctx, v, m, since, time.Now())
			if err != nil {
				return false, err
			}
			for _, p := range producers {
				if counts[p] == 0 {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// opsCreateChannel creates an ntfy channel for the events down and up and
// checks one synchronous test delivery to the fake ntfy server.
func opsCreateChannel(ctx context.Context, env *Env, api *ucapi.Client, name, topic string) (ucapi.NotificationChannel, error) {
	ch, err := api.CreateChannel(ctx, name, env.NtfyURL(topic), []string{"down", "up"})
	if err != nil {
		return ch, fmt.Errorf("create channel: %w", err)
	}
	res, err := api.TestChannel(ctx, ch.ID)
	if err != nil {
		return ch, fmt.Errorf("test channel: %w", err)
	}
	if !res.Delivered {
		return ch, fmt.Errorf("test channel not delivered: %s", res.Error)
	}
	return ch, nil
}

// opsHTTPMonitor creates an HTTP monitor with assignment all against the fake
// target and the given channels.
func opsHTTPMonitor(ctx context.Context, env *Env, api *ucapi.Client, name, path string, channels []string) (ucapi.Monitor, error) {
	return api.CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:                   name,
		Type:                   "http",
		Config:                 map[string]any{"url": env.TargetURL(path)},
		IntervalSeconds:        int(env.Cfg.Interval / time.Second),
		Retries:                1,
		Assignment:             &ucapi.Assignment{Mode: "all"},
		NotificationChannelIDs: channels,
	})
}

// opsNotifyCounts counts the recorded notifications of one topic by title
// ("DOWN: <monitor>", "UP: <monitor>") that arrived at or after since.
func opsNotifyCounts(rec *fake.Recorder, topicPath string, since time.Time) map[string]int {
	out := make(map[string]int)
	for _, m := range rec.Messages() {
		if m.Path != topicPath || m.At.Before(since) {
			continue
		}
		out[m.Title]++
	}
	return out
}

// opsWriteVisible creates a monitor through voter writer and reports how long
// each voter in seen needs to list it (5 s limit, as in AS-02).
func opsWriteVisible(ctx context.Context, cl *harness.Cluster, env *Env, writer int, seen []int, name, path string) (string, bool, error) {
	if _, err := opsHTTPMonitor(ctx, env, cl.Client(writer), name, path, nil); err != nil {
		return "", false, fmt.Errorf("write through voter %d: %w", writer, err)
	}
	written := time.Now()
	var parts []string
	all := true
	for _, i := range seen {
		ok, _ := pollUntil(ctx, 5*time.Second, 200*time.Millisecond, func() (bool, error) {
			mons, err := cl.Client(i).ListMonitors(ctx)
			if err != nil {
				return false, err
			}
			for _, m := range mons {
				if m.Name == name {
					return true, nil
				}
			}
			return false, nil
		})
		parts = append(parts, fmt.Sprintf("voter %d after %s", i, time.Since(written).Round(100*time.Millisecond)))
		all = all && ok
	}
	return strings.Join(parts, ", "), all, nil
}

// opsRandomSecret returns n random bytes encoded as URL-safe base64. Used for
// the admin password of reset-password; it is never printed.
func opsRandomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// opsVoterCount counts the voters of a member list.
func opsVoterCount(list ucapi.ClusterNodeList) int {
	n := 0
	for _, m := range list.Nodes {
		if m.Voter {
			n++
		}
	}
	return n
}

// opsCleanup removes objects a scenario created outside the cluster
// (copies, stale containers). It runs with a fresh context.
func opsCleanup(ctx context.Context, cl *harness.Cluster, containers, volumes []string) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	return cl.RemoveObjects(cctx, containers, volumes)
}

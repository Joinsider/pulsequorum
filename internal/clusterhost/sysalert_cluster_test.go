// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// FA-OBS-01 and ADR 0025, sections 3, 4 and 6: a stopped voter raises the
// system alert voter_offline on the system channel, a restarted voter clears
// it, and the FSM checkpoints of all voters agree without a fingerprint
// mismatch.
//
// The test starts three voters as TestLeaderChangeAlerting does and runs the
// collector, the system jobs and the alert dispatcher on every voter, as
// cmd/pulsequorum does. The system channel delivers to a httptest webhook
// with the default template; the test checks its system object and the
// structured payload in the outbox.

const (
	sysAlertChannelID = "sys-hook"
	// sysAlertInterval is the evaluation period of the collector (default 30 s).
	sysAlertInterval = 500 * time.Millisecond
	// sysAlertCheckpoint is the checkpoint period (default 1 h). The jobs tick
	// every 5 s, so a checkpoint follows about every 5 s.
	sysAlertCheckpoint = 2 * time.Second
	// sysAlertDispatch is the period of the alert dispatcher.
	sysAlertDispatch = 20 * time.Millisecond
	// sysAlertWait bounds each wait for an alert, a replicated row or a checkpoint.
	sysAlertWait = 90 * time.Second
)

// sysHookBody is the part of the default webhook body that the test reads.
type sysHookBody struct {
	Event   string `json:"event"`
	AlertID string `json:"alertId"`
	Message string `json:"message"`
	System  struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
		Active  bool   `json:"active"`
	} `json:"system"`
}

// sysHook records the webhook bodies of the system channel.
type sysHook struct {
	mu   sync.Mutex
	seen []sysHookBody
}

// newSysHook starts an httptest server that records the bodies it receives.
func newSysHook(t *testing.T) (string, *sysHook) {
	t.Helper()
	h := &sysHook{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b sysHookBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.seen = append(h.seen, b)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, h
}

// waitFor waits until a recorded body matches pred and returns it.
func (h *sysHook) waitFor(t *testing.T, what string, pred func(sysHookBody) bool) sysHookBody {
	t.Helper()
	var found sysHookBody
	eventually(t, what, sysAlertWait, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, b := range h.seen {
			if pred(b) {
				found = b
				return true
			}
		}
		return false
	})
	return found
}

// sysHookIs matches the body of a system alert of kind and subject. The alert ID
// is sys:<kind>:<subject>:<since>:<active|clear> (ADR 0025, section 6), and the
// message is the text of systemText.
func sysHookIs(kind, subject string, active bool) func(sysHookBody) bool {
	prefix := "sys:" + kind + ":" + subject + ":"
	suffix := ":clear"
	text := fmt.Sprintf("System alert %s (%s)", kind, subject)
	if active {
		suffix = ":active"
	} else {
		text += " cleared"
	}
	return func(b sysHookBody) bool {
		return b.Event == store.EventSystem && strings.HasPrefix(b.AlertID, prefix) &&
			strings.HasSuffix(b.AlertID, suffix) && b.Message == text
	}
}

// sysPayloadOf decodes the stored payload of an outbox entry of the system channel.
func sysPayloadOf(ctx context.Context, t *testing.T, n *testNode, alertID string) cluster.AlertPayload {
	t.Helper()
	ad, err := n.state.GetAlert(ctx, alertID, sysAlertChannelID)
	if err != nil {
		t.Fatalf("outbox entry %s: %v", alertID, err)
	}
	var p cluster.AlertPayload
	if err := json.Unmarshal(ad.Payload, &p); err != nil {
		t.Fatalf("decode payload of %s: %v", alertID, err)
	}
	return p
}

// systemAlertRow returns the stored system alert of kind and subject on n, active or not.
func systemAlertRow(ctx context.Context, t *testing.T, n *testNode, kind, subject string) (store.SystemAlert, bool) {
	t.Helper()
	alerts, err := n.state.ListSystemAlerts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		if a.Kind == kind && a.Subject == subject {
			return a, true
		}
	}
	return store.SystemAlert{}, false
}

// waitSystemAlert waits until n stores the alert of kind and subject with the given active flag.
func waitSystemAlert(ctx context.Context, t *testing.T, n *testNode, kind, subject string, active bool) {
	t.Helper()
	eventually(t, "system alert "+kind+"/"+subject+" on "+n.id, sysAlertWait, func() bool {
		a, ok := systemAlertRow(ctx, t, n, kind, subject)
		return ok && a.Active == active
	})
}

// startSysStack runs the collector, the system jobs and the alert dispatcher of
// voter n as cmd/pulsequorum does. The stack stops with the cancel func that
// alertCluster keeps for n, so stopNode ends it before the voter stops.
func startSysStack(c *alertCluster, n *testNode) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.cancel[n] = cancel
	discard := slog.New(slog.DiscardHandler)
	sender := alert.NewSender(&checker.Dialer{}, discard)
	dispatcher := alert.NewDispatcher(alert.DispatcherOptions{
		State: n.state, Cluster: n.host.Node, Keyring: n.keyring, Sender: sender,
		IsLeader: n.host.Node.IsLeader, Logger: discard, Interval: sysAlertDispatch, ReminderInterval: time.Hour,
	})
	collector := sysalert.New(sysalert.Options{
		NodeID: n.id, IsLeader: n.host.Node.IsLeader, State: n.state, CommandLevel: n.host.Node.CommandLevel,
		Cluster: n.host.Node, Self: n.host.SelfSignals, Peers: n.host.PeerSignals, ClockOffsets: n.host.ClockOffsets,
		Interval: sysAlertInterval, Logger: discard,
	})
	jobs := sysalert.NewJobs(sysalert.JobOptions{
		IsLeader: n.host.Node.IsLeader, CommandLevel: n.host.Node.CommandLevel, State: n.state, Cluster: n.host.Node,
		CheckpointInterval: sysAlertCheckpoint, Logger: discard,
	})
	for _, run := range []func(context.Context){dispatcher.Run, collector.Run, jobs.Run} {
		go run(ctx)
	}
}

// TestSystemAlertsVoterOfflineAndCheckpoint covers FA-OBS-01 (voter_offline),
// the clear after a restart, and the FSM checkpoints of ADR 0025, section 4.
func TestSystemAlertsVoterOfflineAndCheckpoint(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("starts three voters and waits for health checks and checkpoints")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := newAlertCluster(ctx, t)
	hookURL, hook := newSysHook(t)
	start := time.Now()

	// The system jobs create the system tenant at command level 6 (ADR 0025).
	eventually(t, "command level 6 on every voter", sysAlertWait, func() bool {
		for _, n := range c.nodes {
			level, err := n.host.Node.CommandLevel(ctx)
			if err != nil || level < cluster.SystemAlertCommandLevel {
				return false
			}
		}
		return true
	})
	leader := c.waitLeader()
	for _, n := range c.nodes {
		startSysStack(c, n)
	}
	eventually(t, "system tenant", sysAlertWait, func() bool {
		_, err := leader.state.GetTenant(ctx, store.SystemTenantID)
		return err == nil
	})
	t.Logf("command level 6 and system tenant after %s", time.Since(start).Round(time.Millisecond))

	// The system channel, as the admin API creates it (internal/api/system_channels.go).
	enc, err := alert.EncryptURL(leader.keyring, sysAlertChannelID, hookURL)
	if err != nil {
		t.Fatal(err)
	}
	chCmd, err := cluster.NewPutChannel("", store.SystemTenantID, sysAlertChannelID, cluster.ChannelDocument{
		Name: sysAlertChannelID, Kind: store.ChannelWebhook, URLEnc: enc, Events: []string{store.EventSystem},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.apply(chCmd)

	// Stop a follower: the leader must raise voter_offline for it.
	var follower *testNode
	for _, n := range c.live() {
		if n != leader {
			follower = n
			break
		}
	}
	if follower == nil {
		t.Fatal("no follower")
	}
	stoppedAt := time.Now()
	c.stopNode(follower)
	off := hook.waitFor(t, "voter_offline webhook", sysHookIs(sysalert.KindVoterOffline, follower.id, true))
	t.Logf("voter_offline webhook after %s (alertId %s)", time.Since(stoppedAt).Round(time.Millisecond), off.AlertID)
	if off.System.Kind != "voter_offline" || !off.System.Active {
		t.Errorf("webhook system object = %+v, want an active voter_offline", off.System)
	}
	p := sysPayloadOf(ctx, t, leader, off.AlertID)
	if p.System == nil || p.System.Kind != sysalert.KindVoterOffline || p.System.Subject != follower.id || !p.System.Active {
		t.Fatalf("stored voter_offline payload = %+v, want kind %s, subject %s, active", p.System, sysalert.KindVoterOffline, follower.id)
	}
	for _, n := range c.live() {
		waitSystemAlert(ctx, t, n, sysalert.KindVoterOffline, follower.id, true)
	}

	// Restart the follower from its data directory at the same address.
	restartedAt := time.Now()
	moved, err := launch(ctx, t, &testNode{id: follower.id, addr: follower.addr, dir: follower.dir,
		replInterval: follower.replInterval, caps: follower.caps, maxVersion: follower.maxVersion,
		clock: follower.clock, clockInterval: follower.clockInterval}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range c.nodes {
		if n == follower {
			c.nodes[i] = moved
		}
	}
	startSysStack(c, moved)

	cleared := hook.waitFor(t, "voter_offline clear webhook", sysHookIs(sysalert.KindVoterOffline, follower.id, false))
	t.Logf("voter_offline clear webhook after %s (alertId %s)", time.Since(restartedAt).Round(time.Millisecond), cleared.AlertID)
	for _, n := range c.live() {
		waitSystemAlert(ctx, t, n, sysalert.KindVoterOffline, follower.id, false)
	}

	// Checkpoints: all voters report one index and the same fingerprint at it.
	var fps map[string]string
	eventually(t, "one checkpoint index on all voters", sysAlertWait, func() bool {
		fps = map[string]string{}
		var index uint64
		for i, n := range c.live() {
			idx, fp := n.host.Node.LastCheckpoint()
			if idx == 0 || (i > 0 && idx != index) {
				return false
			}
			index = idx
			fps[n.id] = fp
		}
		return true
	})
	var want string
	for id, fp := range fps {
		if fp == "" {
			t.Errorf("voter %s reports an empty checkpoint fingerprint", id)
		}
		if want == "" {
			want = fp
		} else if fp != want {
			t.Errorf("checkpoint fingerprints differ: %v", fps)
		}
	}

	// Give the collector a few evaluations to compare the fingerprints of the
	// checkpoint before the absence of a mismatch row is checked.
	time.Sleep(3 * sysAlertInterval)
	for _, n := range c.live() {
		alerts, err := n.state.ListSystemAlerts(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range alerts {
			if a.Kind == sysalert.KindFingerprint {
				t.Errorf("fingerprint_mismatch row %q on %s", a.Subject, n.id)
			}
		}
	}
	t.Logf("checkpoint agreement on %d voters, total %s", len(fps), time.Since(start).Round(time.Millisecond))
}

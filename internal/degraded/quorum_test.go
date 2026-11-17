// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package degraded

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/sysalert"
)

// channelsState returns the channels of the tenant, as the replicated state does.
type channelsState struct {
	store.State
	channels []store.NotificationChannel
}

func (s channelsState) ListChannels(_ context.Context, tenantID string) ([]store.NotificationChannel, error) {
	var out []store.NotificationChannel
	for _, ch := range s.channels {
		if ch.TenantID == tenantID {
			out = append(out, ch)
		}
	}
	return out, nil
}

// quorumRig holds a notifier with a controllable detector state.
type quorumRig struct {
	queue *fakeQueue
	n     *QuorumNotifier
	st    State
}

func newQuorumRig(t *testing.T, channels ...store.NotificationChannel) *quorumRig {
	t.Helper()
	r := &quorumRig{queue: &fakeQueue{}}
	r.st = State{Degraded: true, Since: rjBase, Sender: testNode, Partition: []string{testNode, "node-b"}, Reachable: 2, Total: 3}
	r.n = NewQuorumNotifier(QuorumOptions{
		NodeID: testNode, State: channelsState{channels: channels}, Keyring: plainKeyring{},
		Dispatcher: r.queue, Detector: func() State { return r.st },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return r
}

func systemChannel(id, state string, events ...string) store.NotificationChannel {
	return store.NotificationChannel{
		TenantID: store.SystemTenantID, ID: id, Kind: store.ChannelWebhook, URLEnc: "https://ops.example/" + id,
		State: state, Events: events,
	}
}

// ADR 0025, section 5: the sender of a degraded period announces "quorum lost"
// once, to the system channels that subscribe to "system" and to no others.
func TestQuorumLostOncePerPeriodToSystemChannels(t *testing.T) {
	r := newQuorumRig(t,
		systemChannel("sys-a", store.ChannelOK, store.EventSystem),
		systemChannel("sys-off", store.ChannelDisabled, store.EventSystem),
		systemChannel("sys-plain", store.ChannelOK, store.EventDown),
		// A tenant channel that subscribes to "system" must never receive it.
		store.NotificationChannel{TenantID: "t1", ID: "tenant", Kind: store.ChannelWebhook, URLEnc: "https://t1.example",
			State: store.ChannelOK, Events: []string{store.EventSystem}},
	)
	ctx := context.Background()
	r.n.Tick(ctx)
	dels := r.queue.take()
	if len(dels) != 1 || dels[0].ChannelID != "sys-a" {
		t.Fatalf("deliveries = %+v, want one to sys-a", dels)
	}
	p := dels[0].Payload
	if p.Event != store.EventSystem || p.TenantID != store.SystemTenantID || p.Confirmed {
		t.Errorf("payload = %+v, want an unconfirmed system event", p)
	}
	if p.AlertID != "degraded:node-a:quorum:"+strconv.FormatInt(rjBase.UnixMilli(), 10) {
		t.Errorf("AlertID = %q", p.AlertID)
	}
	if p.System == nil || p.System.Kind != sysalert.KindQuorum || p.System.Subject != sysalert.SubjectCluster || !p.System.Active {
		t.Fatalf("system info = %+v", p.System)
	}
	var details map[string]int
	if err := json.Unmarshal(p.System.Details, &details); err != nil || details["reachable"] != 2 || details["total"] != 3 {
		t.Errorf("details = %s, want reachable 2 and total 3", p.System.Details)
	}
	if p.Sender == nil || p.Sender.NodeID != testNode || p.Sender.Confirmed || p.Sender.ReachableVoters != 2 {
		t.Errorf("sender = %+v", p.Sender)
	}

	// The same degraded period is announced once, however often the node looks.
	r.n.Tick(ctx)
	r.n.Tick(ctx)
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("announced again within one period: %d deliveries", n)
	}

	// The period ends, and the next one is announced again.
	r.st = State{Since: rjBase.Add(time.Hour)}
	r.n.Tick(ctx)
	r.st = State{Degraded: true, Since: rjBase.Add(time.Hour), Sender: testNode, Reachable: 2, Total: 3}
	r.n.Tick(ctx)
	if n := len(r.queue.take()); n != 1 {
		t.Errorf("next period deliveries = %d, want 1", n)
	}
}

// Only the elected sender announces, and a failed self-check sends nothing.
func TestQuorumLostOnlyFromSenderAndNotWhenSelfCheckFailed(t *testing.T) {
	ch := systemChannel("sys-a", store.ChannelOK, store.EventSystem)
	r := newQuorumRig(t, ch)
	r.st.Sender = "node-b"
	r.n.Tick(context.Background())
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("non-sender announced: %d", n)
	}

	r = newQuorumRig(t, ch)
	r.n.opts.SelfCheckFailed = func() bool { return true }
	r.n.Tick(context.Background())
	if n := len(r.queue.take()); n != 0 {
		t.Errorf("failed self-check announced: %d", n)
	}
}

// ADR 0025, section 5: the restored alert clears the quorum alert of the outage.
// Without a degraded period there is no quorum alert, so nothing is proposed.
func TestGapJobRestoresQuorumAfterDegradedGap(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.opts.CommandLevel = rjCmdLevel(cluster.SystemAlertCommandLevel)
	degraded := true
	rig.job.opts.Degraded = func() bool { return degraded }
	rig.st.lastIssued = rjMs(rjBase.Add(-2 * time.Hour))
	rig.job.sample(context.Background()) // the node is degraded while it has no leader
	degraded = false
	rig.lead(t)
	if !rig.job.Done() {
		t.Fatal("Done() = false, want true")
	}
	cmds := rig.prop.all()
	if len(cmds) != 2 {
		t.Fatalf("commands = %d, want active and then cleared", len(cmds))
	}
	for i, want := range []bool{true, false} {
		set := cmds[i].GetSetSystemAlert()
		if set == nil || set.GetKind() != sysalert.KindQuorum || set.GetSubject() != sysalert.SubjectCluster || set.GetActive() != want {
			t.Fatalf("command %d = %v, want quorum active=%v", i, cmds[i], want)
		}
		var d map[string]int64
		if err := json.Unmarshal(set.GetDetailsJson(), &d); err != nil || d["gapSeconds"] != 7200 {
			t.Errorf("command %d details = %s, want gapSeconds 7200", i, set.GetDetailsJson())
		}
	}
}

func TestGapJobNoQuorumRestoreWithoutDegradedPeriod(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.opts.CommandLevel = rjCmdLevel(cluster.SystemAlertCommandLevel)
	rig.job.opts.Degraded = func() bool { return false }
	rig.st.lastIssued = rjMs(rjBase.Add(-2 * time.Hour))
	rig.job.sample(context.Background())
	rig.lead(t)
	if n := len(rig.prop.all()); n != 0 {
		t.Errorf("commands = %d, want none for a gap without a degraded period", n)
	}
}

func TestGapJobQuorumRestoreNeedsLevel6AndNeverBlocks(t *testing.T) {
	rig := newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.opts.Degraded = func() bool { return true }
	rig.st.lastIssued = rjMs(rjBase.Add(-2 * time.Hour))
	rig.job.sample(context.Background())
	rig.lead(t) // the default rig is at command level 5
	if n := len(rig.prop.all()); n != 0 {
		t.Errorf("below level 6 commands = %d, want none", n)
	}

	rig = newRjGapRig(t, rjMonitor("m1", store.StatusUp, rjBase.Add(-10*time.Hour)))
	rig.job.opts.CommandLevel = rjCmdLevel(cluster.SystemAlertCommandLevel)
	rig.job.opts.Degraded = func() bool { return true }
	rig.st.lastIssued = rjMs(rjBase.Add(-2 * time.Hour))
	rig.job.sample(context.Background())
	rig.prop.fail = func(*pb.Command) error { return errors.New("no quorum") }
	rig.lead(t)
	if !rig.job.Done() {
		t.Error("a failed restore kept the gap job open")
	}
}

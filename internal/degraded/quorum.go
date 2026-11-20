// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package degraded

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/sysalert"
)

// QuorumOptions configures a QuorumNotifier. State, Keyring, Dispatcher and
// Detector are required.
type QuorumOptions struct {
	NodeID string
	// State reads the channels of the system tenant from the local replicated state.
	State store.State
	// Keyring decrypts the channel targets.
	Keyring alert.Keyring
	// Dispatcher delivers the notifications in memory (ADR 0024, section 4).
	Dispatcher Queue
	// Detector returns the degraded state of this node.
	Detector func() State
	// SelfCheckFailed reports whether the own self-check failed (ADR 0022); then
	// nothing is sent (PF-28). Nil means passed.
	SelfCheckFailed func() bool
	// Interval is the polling period of the detector state; zero means one second.
	Interval time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
}

// QuorumNotifier announces the loss of the quorum (ADR 0025, section 5). Once
// per degraded period, the sender of that period sends "quorum lost" to the
// channels of the system tenant that subscribe to "system". The notification is
// unconfirmed and writes no row of the local log: a leader without quorum cannot
// record it, and the restored alert (GapJob) closes it.
//
// It reads the detector state on every tick instead of the Changes signal,
// which the node uses for the overlay reset; two readers would split it.
type QuorumNotifier struct {
	opts QuorumOptions
	// sentSince is the start of the degraded period that was announced; zero
	// if none. Only Run's goroutine touches it.
	sentSince time.Time
}

// NewQuorumNotifier creates a QuorumNotifier with defaults applied to opts.
func NewQuorumNotifier(opts QuorumOptions) *QuorumNotifier {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &QuorumNotifier{opts: opts}
}

// Run ticks until ctx ends.
func (q *QuorumNotifier) Run(ctx context.Context) {
	ticker := time.NewTicker(q.opts.Interval)
	defer ticker.Stop()
	for {
		q.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick announces the degraded period if this node is its sender and has not
// announced it yet.
func (q *QuorumNotifier) Tick(ctx context.Context) {
	st := q.opts.Detector()
	if !st.Degraded {
		q.sentSince = time.Time{}
		return
	}
	if st.Sender != q.opts.NodeID || st.Since.Equal(q.sentSince) {
		return
	}
	q.sentSince = st.Since
	q.announce(ctx, st)
}

// announce sends the quorum-lost notification to the system channels.
func (q *QuorumNotifier) announce(ctx context.Context, st State) {
	if q.opts.SelfCheckFailed != nil && q.opts.SelfCheckFailed() {
		q.opts.Logger.Info("quorum lost not announced: own self-check failed", "since", st.Since)
		return
	}
	channels, err := q.opts.State.ListChannels(ctx, store.SystemTenantID)
	if err != nil {
		q.opts.Logger.Warn("degraded: list system channels for quorum alert", "err", err)
		return
	}
	details, err := json.Marshal(map[string]int{"reachable": st.Reachable, "total": st.Total})
	if err != nil {
		q.opts.Logger.Warn("degraded: encode quorum details", "err", err)
		return
	}
	since := st.Since.UnixMilli()
	payload := cluster.AlertPayload{
		AlertID:  fmt.Sprintf("degraded:%s:quorum:%d", q.opts.NodeID, since),
		TenantID: store.SystemTenantID,
		Event:    store.EventSystem,
		SinceMs:  since,
		Sender: &cluster.AlertSender{
			NodeID: q.opts.NodeID, ReachableVoters: st.Reachable, TotalVoters: st.Total, Confirmed: false,
		},
		System: &cluster.SystemAlertInfo{
			Kind: sysalert.KindQuorum, Subject: sysalert.SubjectCluster, Active: true, Details: details,
		},
	}
	sent := 0
	for _, ch := range channels {
		if ch.State == store.ChannelDisabled || !ch.Subscribes(store.EventSystem) {
			continue
		}
		target, err := alert.DecryptTarget(ctx, q.opts.Keyring, ch)
		if err != nil {
			q.opts.Logger.Warn("degraded: system channel secrets unreadable", "channel", ch.ID, "err", err)
			continue
		}
		q.opts.Dispatcher.Enqueue(alert.LocalDelivery{
			TenantID: ch.TenantID, ChannelID: ch.ID, Target: target, Payload: payload,
			Done: func(outcome string, err error) {
				if err != nil {
					q.opts.Logger.Warn("degraded: quorum alert not delivered", "channel", ch.ID, "outcome", outcome, "err", err)
				}
			},
		})
		sent++
	}
	q.opts.Logger.Warn("quorum lost: announced to system channels", "since", st.Since,
		"reachable", st.Reachable, "total", st.Total, "channels", sent)
}

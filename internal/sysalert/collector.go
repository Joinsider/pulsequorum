// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package sysalert

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Defaults of the collector (ADR 0025, section 3).
const (
	// DefaultInterval is the evaluation period of the leader.
	DefaultInterval = 30 * time.Second
	// DefaultReplicationLag is REPLICATION_LAG_ALERT.
	DefaultReplicationLag = 5 * time.Minute
	// DefaultClockSkew is CLOCK_SKEW_ALERT.
	DefaultClockSkew = 2 * time.Second
	// DefaultDiskFree is DISK_FREE_ALERT.
	DefaultDiskFree = 0.15
	// DefaultWALSize is WAL_SIZE_ALERT, 512 MiB.
	DefaultWALSize int64 = 512 << 20

	// mixedAfter is how long the command versions of the voters may differ
	// before mixed_versions is raised.
	mixedAfter = 24 * time.Hour
	// applyTimeout bounds one proposal.
	applyTimeout = 5 * time.Second
	// confirmRounds is the number of consecutive evaluations that must agree
	// before a condition changes its alert (hysteresis, ADR 0025).
	confirmRounds = 2
	// confirmNow is used where the register already debounces the condition
	// (voter_offline, node_impaired) or where it is a plain status.
	confirmNow = 1
)

// Thresholds of the signal alerts. The zero value means the defaults.
type Thresholds struct {
	ReplicationLag time.Duration
	ClockSkew      time.Duration
	DiskFree       float64
	WALSize        int64
}

// Applier proposes Raft commands. *cluster.Node satisfies it.
type Applier interface {
	Apply(ctx context.Context, cmd *pb.Command) (cluster.ApplyResult, error)
}

// State is the replicated state the collector reads.
type State interface {
	ListNodes(ctx context.Context) ([]store.Node, error)
	ListSystemAlerts(ctx context.Context, activeOnly bool) ([]store.SystemAlert, error)
	ListNodeIncarnations(ctx context.Context) ([]store.NodeIncarnation, error)
	ListClusterCAs(ctx context.Context) ([]store.ClusterCA, error)
}

// Options configure a Collector. NodeID, IsLeader, State, CommandLevel,
// Cluster, Self and Peers are required.
type Options struct {
	NodeID string
	// IsLeader reports whether this node leads. Only the leader evaluates.
	IsLeader func() bool
	State    State
	// CommandLevel returns the command level of the cluster; below
	// cluster.SystemAlertCommandLevel the collector only logs.
	CommandLevel func(ctx context.Context) (uint32, error)
	Cluster      Applier
	// Self returns the signals of this node. The collector stamps AnsweredAt.
	Self func() Signals
	// Peers returns the signals of the peer voters by node ID (see
	// clusterhost.Host.PeerSignals).
	Peers func() map[string]Signals
	// ClockOffsets returns the clock offset of each peer in milliseconds,
	// positive when the peer is ahead (ADR 0020). Nil means none.
	ClockOffsets func() map[string]int64
	Thresholds   Thresholds
	// Interval is the evaluation period; zero means DefaultInterval.
	Interval time.Duration
	// Now is the clock of this node (ADR 0020); nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// key identifies one system alert.
type key struct{ kind, subject string }

// alertState is the stored state of a system alert. The level of an inactive
// alert is always zero.
type alertState struct {
	active bool
	level  int
}

// streak counts the consecutive evaluations that want the same state.
type streak struct {
	want alertState
	n    int
}

// verdict is the condition of one alert in one evaluation.
type verdict struct {
	key
	active  bool
	level   int
	details map[string]any
	// confirm is the number of consecutive evaluations needed for a change.
	confirm int
}

// round is the input of one evaluation.
type round struct {
	now     time.Time
	voters  []store.Node
	signals map[string]Signals // fresh signals only; the own node is always fresh
	offsets map[string]int64
	// leaderCommit is the commit index of this node, the reference of raft_stale.
	leaderCommit uint64
}

// Collector evaluates the system alerts on the leader (ADR 0025, section 3).
// It runs in one goroutine; only the Run loop touches its fields.
type Collector struct {
	opts Options
	th   Thresholds

	leaderSince time.Time
	// current is the stored state of the alerts. Without a command level that
	// stores it, it is the collector's own record.
	current map[key]alertState
	streaks map[key]streak
	// incs is the last known incarnation of each node; incRetry holds the
	// level of an incarnation alert whose write failed.
	incs     map[string]string
	incRetry map[string]int
	// never is the first evaluation at which a voter reported that it never
	// caught up; the replication lag alert waits for it.
	never map[string]time.Time
	// mixedSince is the first evaluation at which the voters differed in their
	// command versions.
	mixedSince time.Time
}

// New creates a Collector with defaults applied to opts.
func New(opts Options) *Collector {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	th := opts.Thresholds
	if th.ReplicationLag <= 0 {
		th.ReplicationLag = DefaultReplicationLag
	}
	if th.ClockSkew <= 0 {
		th.ClockSkew = DefaultClockSkew
	}
	if th.DiskFree <= 0 {
		th.DiskFree = DefaultDiskFree
	}
	if th.WALSize <= 0 {
		th.WALSize = DefaultWALSize
	}
	return &Collector{
		opts: opts, th: th,
		current:  map[key]alertState{},
		streaks:  map[key]streak{},
		incs:     map[string]string{},
		incRetry: map[string]int{},
		never:    map[string]time.Time{},
	}
}

// Run evaluates every Interval until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick runs one evaluation if this node leads and the leadership has lasted a
// full interval. The first evaluation of a leadership waits for one full round
// of GetStatus answers, as in ADR 0022.
func (c *Collector) Tick(ctx context.Context) {
	now := c.opts.Now()
	if !c.opts.IsLeader() {
		c.stepDown()
		return
	}
	if c.leaderSince.IsZero() {
		c.leaderSince = now
	}
	if now.Sub(c.leaderSince) < c.opts.Interval {
		return
	}
	level, err := c.opts.CommandLevel(ctx)
	if err != nil {
		c.opts.Logger.Warn("system alerts: read command level", "err", err)
		return
	}
	persist := level >= cluster.SystemAlertCommandLevel
	nodes, err := c.opts.State.ListNodes(ctx)
	if err != nil {
		c.opts.Logger.Warn("system alerts: read node registry", "err", err)
		return
	}
	if persist {
		if err := c.loadState(ctx); err != nil {
			c.opts.Logger.Warn("system alerts: read stored state", "err", err)
			return
		}
	}
	r := c.snapshot(now, nodes)

	var verdicts []verdict
	for _, n := range r.voters {
		verdicts = append(verdicts, c.nodeVerdicts(r, n)...)
	}
	verdicts = append(verdicts, c.caVerdict(ctx, now)...)
	verdicts = append(verdicts, c.mixedVerdict(now, r.voters)...)
	c.reconcile(ctx, persist, verdicts)
	c.incarnations(ctx, persist, r)
	c.fingerprints(ctx, persist, r)
}

// stepDown forgets the timers and streaks of a former leader. The stored
// alert state is read again on the next leadership.
func (c *Collector) stepDown() {
	if c.leaderSince.IsZero() {
		return
	}
	c.leaderSince = time.Time{}
	c.streaks = map[key]streak{}
	c.never = map[string]time.Time{}
	c.mixedSince = time.Time{}
	c.incRetry = map[string]int{}
	c.opts.Logger.Info("no longer leader, system alert evaluation stopped")
}

// loadState replaces the stored alert state and the incarnations with the
// replicated ones.
func (c *Collector) loadState(ctx context.Context) error {
	alerts, err := c.opts.State.ListSystemAlerts(ctx, false)
	if err != nil {
		return err
	}
	incs, err := c.opts.State.ListNodeIncarnations(ctx)
	if err != nil {
		return err
	}
	c.current = make(map[key]alertState, len(alerts))
	for _, a := range alerts {
		st := alertState{active: a.Active}
		if a.Active {
			st.level = a.Level
		}
		c.current[key{a.Kind, a.Subject}] = st
	}
	c.incs = make(map[string]string, len(incs))
	for _, in := range incs {
		c.incs[in.NodeID] = in.Incarnation
	}
	return nil
}

// snapshot collects the inputs of one evaluation. A peer counts as fresh if it
// answered within two intervals.
func (c *Collector) snapshot(now time.Time, nodes []store.Node) round {
	r := round{now: now, signals: map[string]Signals{}, offsets: map[string]int64{}}
	for _, n := range nodes {
		if n.Role == store.NodeRoleVoter {
			r.voters = append(r.voters, n)
		}
	}
	if c.opts.Self != nil {
		s := c.opts.Self()
		s.NodeID, s.AnsweredAt = c.opts.NodeID, now
		r.signals[c.opts.NodeID] = s
		r.leaderCommit = s.CommitIndex
	}
	if c.opts.Peers != nil {
		for id, s := range c.opts.Peers() {
			if id == c.opts.NodeID || now.Sub(s.AnsweredAt) > 2*c.opts.Interval {
				continue
			}
			r.signals[id] = s
		}
	}
	if c.opts.ClockOffsets != nil {
		r.offsets = c.opts.ClockOffsets()
	}
	return r
}

// nodeVerdicts evaluates one voter. A voter that does not answer gets only
// voter_offline (ADR 0025, section 3).
func (c *Collector) nodeVerdicts(r round, n store.Node) []verdict {
	out := []verdict{{key: key{KindVoterOffline, n.ID}, active: !n.Healthy, confirm: confirmNow}}
	s, fresh := r.signals[n.ID]
	if !fresh {
		return out
	}
	out = append(out, verdict{key: key{KindNodeImpaired, n.ID}, active: n.ConnectivityImpaired, confirm: confirmNow})
	out = append(out, c.lagVerdict(r.now, s)...)
	if off, ok := r.offsets[n.ID]; ok {
		out = append(out, verdict{
			key:    key{KindClockSkew, n.ID},
			active: absInt64(off) > c.th.ClockSkew.Milliseconds(),
			details: map[string]any{
				"offsetMs": off,
			},
			confirm: confirmRounds,
		})
	}
	if s.HasSystemFields() {
		out = append(out, c.systemFieldVerdicts(r.now, s)...)
	}
	out = append(out, verdict{
		key:     key{KindRaftStale, n.ID},
		active:  RaftStale(s, r.leaderCommit),
		details: map[string]any{"appliedGap": gap(r.leaderCommit, s.AppliedIndex), "lastContactMs": s.LastContactMs},
		confirm: confirmRounds,
	})
	return out
}

// lagVerdict evaluates the replication lag of a voter. A voter that never
// caught up counts from the first evaluation that saw it so.
func (c *Collector) lagVerdict(now time.Time, s Signals) []verdict {
	if !s.HasSystemFields() {
		return nil
	}
	var active bool
	if s.ReplicationLagMs < 0 {
		first, ok := c.never[s.NodeID]
		if !ok {
			first = now
			c.never[s.NodeID] = now
		}
		active = now.Sub(first) >= c.th.ReplicationLag
	} else {
		delete(c.never, s.NodeID)
		active = s.ReplicationLagMs >= c.th.ReplicationLag.Milliseconds()
	}
	return []verdict{{
		key:     key{KindReplicationLag, s.NodeID},
		active:  active,
		details: map[string]any{"lagMs": s.ReplicationLagMs},
		confirm: confirmRounds,
	}}
}

// systemFieldVerdicts evaluates the disk, WAL and certificate signals of a
// voter that sends the fields of ADR 0025.
func (c *Collector) systemFieldVerdicts(now time.Time, s Signals) []verdict {
	var out []verdict
	if s.DiskFreeRatio >= 0 {
		out = append(out, verdict{
			key:     key{KindDiskLow, s.NodeID},
			active:  s.DiskFreeRatio < c.th.DiskFree,
			details: map[string]any{"freeRatio": s.DiskFreeRatio},
			confirm: confirmRounds,
		})
	}
	out = append(out, verdict{
		key:     key{KindWALLarge, s.NodeID},
		active:  max(s.StateWALBytes, s.MetricsWALBytes) > c.th.WALSize,
		details: map[string]any{"stateWalBytes": s.StateWALBytes, "metricsWalBytes": s.MetricsWALBytes},
		confirm: confirmRounds,
	})
	if !s.CertNotAfter.IsZero() {
		out = append(out, certVerdict(now, key{KindCertExpiry, s.NodeID}, s.CertNotAfter))
	}
	return out
}

// caVerdict evaluates the expiry of the cluster CA from the replicated state.
// Like Authority.current, it takes the first CA row.
func (c *Collector) caVerdict(ctx context.Context, now time.Time) []verdict {
	cas, err := c.opts.State.ListClusterCAs(ctx)
	if err != nil {
		c.opts.Logger.Warn("system alerts: read cluster CA", "err", err)
		return nil
	}
	if len(cas) == 0 {
		return nil
	}
	cert, err := x509.ParseCertificate(cas[0].CertDER)
	if err != nil {
		c.opts.Logger.Warn("system alerts: parse cluster CA", "err", err)
		return nil
	}
	return []verdict{certVerdict(now, key{KindCertExpiry, SubjectCA}, cert.NotAfter)}
}

// certVerdict returns the expiry stage of a certificate: 30, 7 or 1 day(s)
// before the expiry, or 0 if it is valid for more than 30 days. A stage is one
// notification; the level of the alert is the stage (ADR 0025, section 3).
func certVerdict(now time.Time, k key, notAfter time.Time) verdict {
	remaining := notAfter.Sub(now)
	stage := 0
	switch {
	case remaining <= 24*time.Hour:
		stage = 1
	case remaining <= 7*24*time.Hour:
		stage = 7
	case remaining <= 30*24*time.Hour:
		stage = 30
	}
	return verdict{
		key:     k,
		active:  stage > 0,
		level:   stage,
		details: map[string]any{"days": int64(remaining / (24 * time.Hour))},
		confirm: confirmRounds,
	}
}

// mixedVerdict raises mixed_versions once a day while the voters differ in
// their command versions for more than mixedAfter. The day is the level, so
// each day gives one notification. Once the versions agree, it clears.
func (c *Collector) mixedVerdict(now time.Time, voters []store.Node) []verdict {
	k := key{KindMixedVersions, SubjectCluster}
	lo, hi, seen := uint32(math.MaxUint32), uint32(0), map[uint32]bool{}
	for _, n := range voters {
		if n.MaxCommandVersion == 0 {
			continue // a node that does not report its version
		}
		seen[n.MaxCommandVersion] = true
		lo, hi = min(lo, n.MaxCommandVersion), max(hi, n.MaxCommandVersion)
	}
	if len(seen) < 2 {
		c.mixedSince = time.Time{}
		return []verdict{{key: k, confirm: confirmRounds}}
	}
	if c.mixedSince.IsZero() {
		c.mixedSince = now
	}
	if now.Sub(c.mixedSince) < mixedAfter {
		// Still within the grace period: keep the state as it is, also after a
		// leader change, which restarts the period.
		return nil
	}
	return []verdict{{
		key:     k,
		active:  true,
		level:   int(now.Unix() / int64(24*time.Hour/time.Second)),
		details: map[string]any{"minVersion": lo, "maxVersion": hi, "versions": len(seen)},
		confirm: confirmRounds,
	}}
}

// reconcile compares every verdict with the stored state and writes the
// changes that held for enough evaluations. An alert that stays as it is
// produces nothing.
func (c *Collector) reconcile(ctx context.Context, persist bool, verdicts []verdict) {
	for _, v := range verdicts {
		want := alertState{active: v.active}
		if v.active {
			want.level = v.level
		}
		cur := c.current[v.key]
		if !cur.active {
			cur.level = 0
		}
		if cur == want {
			delete(c.streaks, v.key)
			continue
		}
		s := c.streaks[v.key]
		if s.want != want {
			s = streak{want: want}
		}
		s.n++
		if s.n < v.confirm {
			c.streaks[v.key] = s
			continue
		}
		delete(c.streaks, v.key)
		if err := c.write(ctx, persist, v); err != nil {
			// The streak is kept full, so the next evaluation proposes it again.
			c.streaks[v.key] = streak{want: want, n: v.confirm}
			c.opts.Logger.Warn("system alert not recorded, retrying", "kind", v.kind, "subject", v.subject, "err", err)
		}
	}
}

// write records one change of an alert. Without a command level that stores
// it, the change is only logged and kept in the collector's own record.
func (c *Collector) write(ctx context.Context, persist bool, v verdict) error {
	details, err := detailsJSON(v.details)
	if err != nil {
		return err
	}
	if persist {
		callCtx, cancel := context.WithTimeout(ctx, applyTimeout)
		_, err := c.opts.Cluster.Apply(callCtx, cluster.NewSetSystemAlert(v.kind, v.subject, v.active, v.level, details))
		cancel()
		if err != nil {
			return err
		}
	}
	st := alertState{active: v.active}
	if v.active {
		st.level = v.level
	}
	c.current[v.key] = st
	if v.active {
		c.opts.Logger.Warn("system alert", "kind", v.kind, "subject", v.subject, "active", true, "level", v.level, "persisted", persist)
	} else {
		c.opts.Logger.Info("system alert cleared", "kind", v.kind, "subject", v.subject, "persisted", persist)
	}
	return nil
}

// incarnations records the incarnation of every voter that reports one. A new
// node is recorded without an alert; a changed incarnation is recorded and
// raises incarnation_changed once for this change (FA-REP-06).
func (c *Collector) incarnations(ctx context.Context, persist bool, r round) {
	for id, level := range c.incRetry {
		v := verdict{key: key{KindIncarnation, id}, active: true, level: level}
		if err := c.write(ctx, persist, v); err == nil {
			delete(c.incRetry, id)
		}
	}
	for _, n := range r.voters {
		s, fresh := r.signals[n.ID]
		if !fresh || s.Incarnation == "" {
			continue
		}
		prev, known := c.incs[n.ID]
		if known && prev == s.Incarnation {
			continue
		}
		if err := c.recordIncarnation(ctx, persist, n.ID, s.Incarnation); err != nil {
			c.opts.Logger.Warn("system alerts: record incarnation", "node_id", n.ID, "err", err)
			continue
		}
		if !known {
			continue
		}
		level := c.current[key{KindIncarnation, n.ID}].level + 1
		v := verdict{key: key{KindIncarnation, n.ID}, active: true, level: level}
		if err := c.write(ctx, persist, v); err != nil {
			c.incRetry[n.ID] = level
			c.opts.Logger.Warn("incarnation change not recorded as alert, retrying", "node_id", n.ID, "err", err)
		}
	}
}

// recordIncarnation stores the incarnation of a node, first in the replicated
// state if it persists, then in the collector's own record.
func (c *Collector) recordIncarnation(ctx context.Context, persist bool, nodeID, incarnation string) error {
	if persist {
		callCtx, cancel := context.WithTimeout(ctx, applyTimeout)
		_, err := c.opts.Cluster.Apply(callCtx, cluster.NewRecordIncarnation(nodeID, incarnation))
		cancel()
		if err != nil {
			return err
		}
	}
	c.incs[nodeID] = incarnation
	return nil
}

// fingerprints compares the FSM fingerprints of the nodes that report the same
// checkpoint index. A mismatch raises fingerprint_mismatch once per index; the
// collector repairs nothing (ADR 0025, section 4).
func (c *Collector) fingerprints(ctx context.Context, persist bool, r round) {
	byIndex := map[uint64]map[string]string{}
	for id, s := range r.signals {
		if !s.HasSystemFields() || s.FingerprintIndex == 0 || s.Fingerprint == "" {
			continue
		}
		if byIndex[s.FingerprintIndex] == nil {
			byIndex[s.FingerprintIndex] = map[string]string{}
		}
		byIndex[s.FingerprintIndex][id] = s.Fingerprint
	}
	indexes := make([]uint64, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	for _, idx := range indexes {
		nodes := byIndex[idx]
		if !differ(nodes) {
			continue
		}
		k := key{KindFingerprint, strconv.FormatUint(idx, 10)}
		if _, recorded := c.current[k]; recorded {
			continue
		}
		ids := make([]string, 0, len(nodes))
		for id := range nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		v := verdict{key: k, active: true, details: map[string]any{"index": idx, "nodes": ids}}
		if err := c.write(ctx, persist, v); err != nil {
			c.opts.Logger.Warn("fingerprint mismatch not recorded, retrying", "index", idx, "err", err)
		}
	}
}

// differ reports whether the nodes report more than one fingerprint.
func differ(nodes map[string]string) bool {
	var first string
	for _, fp := range nodes {
		if first == "" {
			first = fp
		} else if fp != first {
			return true
		}
	}
	return false
}

// detailsJSON encodes the details of an alert. They hold numbers and node IDs
// only (ADR 0025, section 6); a nil map is an empty object.
func detailsJSON(d map[string]any) (json.RawMessage, error) {
	if len(d) == 0 {
		return json.RawMessage("{}"), nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("encode system alert details: %w", err)
	}
	return raw, nil
}

func gap(commit, applied uint64) uint64 {
	if commit > applied {
		return commit - applied
	}
	return 0
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

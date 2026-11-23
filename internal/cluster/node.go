// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

// Errors returned by Node.Apply (ADR 0012 A8).
var (
	// ErrNotApplied means the command is certainly not in the log (e.g. this
	// node is not the leader); retrying is safe.
	ErrNotApplied = errors.New("command not applied")
	// ErrUnknownOutcome means the command may or may not have been committed;
	// a retry with the same request ID is safe thanks to deduplication.
	ErrUnknownOutcome = errors.New("command outcome unknown")
)

// Defaults from ADR 0005 point 5.
const (
	DefaultSnapshotThreshold = 8192
	DefaultTrailingLogs      = 20000
	DefaultSnapshotInterval  = 2 * time.Minute
	defaultApplyTimeout      = 5 * time.Second
	retainedSnapshots        = 2
	raftDBFilename           = "raft.db"
)

// Bounds of forwarding a command from a follower to the leader (FA-CLU-05).
const (
	// forwardTimeout bounds the whole Apply call on a follower, including the
	// wait until the command is applied locally.
	forwardTimeout = 3 * time.Second
	// forwardBackoffStart and forwardBackoffLimit bound the pause between retries
	// (not leader, no leader known yet).
	forwardBackoffStart = 50 * time.Millisecond
	forwardBackoffLimit = 500 * time.Millisecond
	// appliedPoll is how often a follower checks whether the FSM caught up.
	appliedPoll = 5 * time.Millisecond
)

// Config configures a Raft node.
type Config struct {
	NodeID string
	// Advertise is the stable host:port of this node and its Raft server address.
	Advertise string
	DataDir   string
	// Timeout is HeartbeatTimeout and ElectionTimeout (ADR 0012 A6).
	Timeout time.Duration

	SnapshotThreshold uint64
	TrailingLogs      uint64
	SnapshotInterval  time.Duration

	// Transport defaults to an in-memory transport on Advertise, which is all a
	// single-member cluster in tests needs; voters use the mTLS cluster port.
	Transport raft.Transport

	// Join marks a node that joins an existing cluster: without Raft state it
	// waits to be added instead of bootstrapping (FA-CLU-02/03, ADR 0005 6).
	Join bool

	// MaxCommandVersion overrides CommandVersion; for tests only (ADR 0018).
	// 0 means CommandVersion.
	MaxCommandVersion uint32

	// Clock is the wall clock of this node: it stamps IssuedAtMs and the time
	// of heartbeat observations. Nil means time.Now. Tests give each node its
	// own skewed clock (ADR 0020). Raft's own timing stays on the monotonic clock.
	Clock func() time.Time

	// Forwarder sends commands of followers to the leader. Nil means that a
	// follower does not forward and returns ErrNotApplied (FA-CLU-05).
	Forwarder Forwarder

	Logger *slog.Logger
	// RaftLogLevel and RaftLogJSON configure the library logger.
	RaftLogLevel string
	RaftLogJSON  bool
}

// Forwarder delivers a command to the leader at leaderAddress and returns the
// leader's result. Error contract, which Node.Apply relies on:
//   - ErrNotApplied: the leader certainly did not append the command; the
//     caller may retry at once or later.
//   - ErrUnknownOutcome: the command may or may not be committed; retrying
//     with the same request ID is safe thanks to deduplication.
//   - a *CommandError: the leader rejected the command deterministically. It
//     is returned both in ApplyResult.Err and as the error.
type Forwarder interface {
	Forward(ctx context.Context, leaderAddress string, cmd *pb.Command) (ApplyResult, error)
}

// Node runs Raft with the FSM on top of state.db.
type Node struct {
	cfg    Config
	raft   *raft.Raft
	fsm    *FSM
	bolt   *raftboltdb.BoltStore
	logger *slog.Logger
	clock  func() time.Time

	// observers are the Raft observers of OnPeerResumed; done ends their
	// goroutines at Shutdown.
	observers []*raft.Observer
	done      chan struct{}
	closeOnce sync.Once
}

// Start opens the Raft stores and starts Raft. A node without existing Raft
// state bootstraps a single-member cluster; a node with state never bootstraps
// again, whatever its configuration says (FA-CLU-01/02, PF-01).
func Start(ctx context.Context, cfg Config, state StateStore, onFatal func(error)) (*Node, error) {
	if cfg.NodeID == "" || cfg.Advertise == "" {
		return nil, errors.New("cluster: node ID and advertise address are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	log := cfg.Logger.With("component", "raft")

	dir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create raft directory: %w", err)
	}
	hlog := hclog.New(&hclog.LoggerOptions{
		Name:       "raft",
		Level:      hclog.LevelFromString(strings.ToLower(cfg.RaftLogLevel)),
		JSONFormat: cfg.RaftLogJSON,
		Output:     os.Stderr,
	})

	bolt, snaps, err := openRaftStores(dir, hlog)
	if err != nil {
		return nil, err
	}
	fsm, err := initCompatibleFSM(ctx, cfg, state, onFatal)
	if err != nil {
		_ = bolt.Close()
		return nil, err
	}

	rc := buildRaftConfig(cfg, hlog, snaps, fsm)
	existing, err := raft.HasExistingState(bolt, bolt, snaps)
	if err != nil {
		_ = bolt.Close()
		return nil, fmt.Errorf("inspect raft state: %w", err)
	}

	trans := cfg.Transport
	if trans == nil {
		_, trans = raft.NewInmemTransport(raft.ServerAddress(cfg.Advertise))
	}

	r, err := raft.NewRaft(rc, fsm, bolt, bolt, snaps, trans)
	if err != nil {
		_ = bolt.Close()
		return nil, fmt.Errorf("start raft: %w", err)
	}

	n := &Node{cfg: cfg, raft: r, fsm: fsm, bolt: bolt, logger: log, done: make(chan struct{}), clock: cfg.Clock}
	if n.clock == nil {
		n.clock = time.Now
	}
	return finishRaftStart(n, cfg, log, trans, existing, rc.NoSnapshotRestoreOnStart)
}

func openRaftStores(dir string, hlog hclog.Logger) (*raftboltdb.BoltStore, *raft.FileSnapshotStore, error) {
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, raftDBFilename))
	if err != nil {
		return nil, nil, fmt.Errorf("open raft log store: %w", err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, retainedSnapshots, hlog)
	if err != nil {
		_ = bolt.Close()
		return nil, nil, fmt.Errorf("open snapshot store: %w", err)
	}
	return bolt, snaps, nil
}

func initCompatibleFSM(ctx context.Context, cfg Config, state StateStore, onFatal func(error)) (*FSM, error) {
	fsm, err := NewFSM(ctx, state, cfg.Logger, onFatal)
	if err != nil {
		return nil, err
	}
	if cfg.MaxCommandVersion != 0 {
		fsm.maxVersion = cfg.MaxCommandVersion
	}
	// A binary must not start on state that needs a newer version, and must not
	// start on state older than the oldest version it can still apply (ADR 0018).
	storedLevel, err := state.CommandLevel(ctx)
	if err != nil {
		return nil, fmt.Errorf("read command level: %w", err)
	}
	level := EffectiveCommandLevel(storedLevel)
	if level > fsm.maxVersion {
		return nil, fmt.Errorf("%w: state requires command version %d, this binary supports up to %d: upgrade the binary (ADR 0018)",
			ErrIncompatibleVersion, level, fsm.maxVersion)
	}
	if level < MinCommandLevel {
		return nil, fmt.Errorf("%w: cluster command level %d is older than %d, the oldest this binary supports: upgrade through the previous minor version first (ADR 0018)",
			ErrIncompatibleVersion, level, MinCommandLevel)
	}
	return fsm, nil
}

func buildRaftConfig(cfg Config, hlog hclog.Logger, snaps *raft.FileSnapshotStore, fsm *FSM) *raft.Config {
	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.NodeID)
	rc.Logger = hlog
	if cfg.Timeout > 0 {
		rc.HeartbeatTimeout = cfg.Timeout
		rc.ElectionTimeout = cfg.Timeout
		rc.LeaderLeaseTimeout = cfg.Timeout / 2
	}
	rc.SnapshotThreshold = orDefault(cfg.SnapshotThreshold, DefaultSnapshotThreshold)
	rc.TrailingLogs = orDefault(cfg.TrailingLogs, DefaultTrailingLogs)
	rc.SnapshotInterval = DefaultSnapshotInterval
	if cfg.SnapshotInterval > 0 {
		rc.SnapshotInterval = cfg.SnapshotInterval
	}
	// Skip restoring the latest snapshot if state.db is already at least as new
	// (ADR 0012 A2); Raft then replays the log and the FSM skips old entries.
	if latest, err := snaps.List(); err == nil && len(latest) > 0 && fsm.AppliedIndex() >= latest[0].Index {
		rc.NoSnapshotRestoreOnStart = true
	}
	return rc
}

func finishRaftStart(n *Node, cfg Config, log *slog.Logger, trans raft.Transport, existing, skipSnapshotRestore bool) (*Node, error) {
	if existing {
		log.Info("resuming from existing raft state", "node_id", cfg.NodeID,
			"applied_index", n.fsm.AppliedIndex(), "skip_snapshot_restore", skipSnapshotRestore)
		return n, nil
	}
	if cfg.Join {
		log.Info("no raft state found, waiting to be added to the cluster", "node_id", cfg.NodeID, "address", cfg.Advertise)
		return n, nil
	}
	log.Info("no raft state found, bootstrapping a single-member cluster", "node_id", cfg.NodeID, "address", cfg.Advertise)
	err := n.raft.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter,
		ID:       raft.ServerID(cfg.NodeID),
		Address:  trans.LocalAddr(),
	}}}).Error()
	if err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
		_ = n.Shutdown()
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	return n, nil
}

// HasState reports whether dataDir holds Raft state (log or snapshot), so
// the node belongs to a cluster already (FA-CLU-02). Call it before Start.
func HasState(dataDir string) (bool, error) {
	dir := filepath.Join(dataDir, "raft")
	if _, err := os.Stat(filepath.Join(dir, raftDBFilename)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, raftDBFilename))
	if err != nil {
		return false, fmt.Errorf("open raft log store: %w", err)
	}
	defer func() { _ = bolt.Close() }()
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, retainedSnapshots, hclog.NewNullLogger())
	if err != nil {
		return false, fmt.Errorf("open snapshot store: %w", err)
	}
	return raft.HasExistingState(bolt, bolt, snaps)
}

func orDefault(v, def uint64) uint64 {
	if v == 0 {
		return def
	}
	return v
}

// FSM returns the state machine.
func (n *Node) FSM() *FSM { return n.fsm }

// LastCheckpoint returns the log index and fingerprint of the last checkpoint
// this node applied since it started, or 0 and "" if none (ADR 0025).
func (n *Node) LastCheckpoint() (index uint64, fingerprint string) {
	return n.fsm.LastCheckpoint()
}

// MaxCommandVersion returns the highest Command.version this node applies
// (ADR 0018): CommandVersion, unless a test overrides it.
func (n *Node) MaxCommandVersion() uint32 { return n.fsm.maxVersion }

// CommandLevel returns the command level of the locally applied state
// (ADR 0018).
func (n *Node) CommandLevel(ctx context.Context) (uint32, error) {
	stored, err := n.fsm.state.CommandLevel(ctx)
	if err != nil {
		return 0, fmt.Errorf("read command level: %w", err)
	}
	return EffectiveCommandLevel(stored), nil
}

// checkCommandLevel rejects a command whose version the cluster does not
// support yet: not every voter knows it (ADR 0018). The rejection is not
// stored in the log, so it is not a deterministic command result.
func (n *Node) checkCommandLevel(ctx context.Context, cmd *pb.Command) error {
	level, err := n.CommandLevel(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotApplied, err)
	}
	if cmd.GetVersion() > level {
		return &CommandError{Code: CodeNotEnabled, Message: fmt.Sprintf(
			"command version %d is not enabled yet: the cluster command level is %d; all voters must be upgraded first",
			cmd.GetVersion(), level)}
	}
	return nil
}

// now returns the wall clock of this node (Config.Clock).
func (n *Node) now() time.Time { return n.clock() }

// IsLeader reports whether this node currently leads.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// WaitLeader blocks until the cluster has a leader or ctx ends.
func (n *Node) WaitLeader(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if addr, _ := n.raft.LeaderWithID(); addr != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for leader: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Apply proposes a command and waits until it is applied on this node. On
// the leader the command is appended directly (ApplyLocal). On a follower it
// is forwarded to the leader, and the follower waits until its own FSM has
// applied the result, so a read after a successful Apply sees the command.
// The caller notices nothing; a retry keeps the request ID, and the FSM
// deduplicates it (ADR 0012 A8).
//
// Errors: ErrNotApplied if the command certainly did not commit, ErrUnknownOutcome
// if it may have committed, and a *CommandError if it was rejected.
func (n *Node) Apply(ctx context.Context, cmd *pb.Command) (ApplyResult, error) {
	if cmd.GetVersion() == 0 {
		cmd.Version = minVersion(cmd)
	}
	if err := n.checkCommandLevel(ctx, cmd); err != nil {
		return ApplyResult{}, err
	}
	if n.raft.State() == raft.Leader {
		return n.ApplyLocal(ctx, cmd)
	}
	if n.cfg.Forwarder == nil {
		return ApplyResult{}, fmt.Errorf("%w: not the leader", ErrNotApplied)
	}
	return n.forwardLoop(ctx, cmd)
}

func (n *Node) forwardLoop(parent context.Context, cmd *pb.Command) (ApplyResult, error) {
	ctx, cancel := context.WithTimeout(parent, forwardTimeout)
	defer cancel()
	backoff := forwardBackoffStart
	for {
		if n.raft.State() == raft.Leader {
			return n.ApplyLocal(ctx, cmd)
		}
		// LastContact is taken from the monotonic clock, so it is compared with
		// time.Now and not with the node clock (ADR 0020).
		if leaderContactStale(n.raft.LastContact(), time.Now(), n.heartbeatTimeout()) {
			return ApplyResult{}, fmt.Errorf("%w: no recent contact with the leader", ErrNotApplied)
		}

		addr, _ := n.raft.LeaderWithID()
		if addr != "" {
			res, done, err := n.tryForwardOnce(ctx, string(addr), cmd)
			if done {
				return res, err
			}
		}
		if !sleepContext(ctx, backoff) {
			return ApplyResult{}, forwardTimeoutError(ctx)
		}
		backoff = nextBackoff(backoff)
	}
}

func (n *Node) tryForwardOnce(ctx context.Context, addr string, cmd *pb.Command) (ApplyResult, bool, error) {
	res, err := n.cfg.Forwarder.Forward(ctx, addr, cmd)
	var cmdErr *CommandError
	switch {
	case err == nil, errors.As(err, &cmdErr):
		// Committed, or rejected by the FSM on the leader: either way
		// the entry is in the log and this node must apply it too.
		n.waitApplied(ctx, res.Index)
		return res, true, err
	case errors.Is(err, ErrNotApplied):
		return ApplyResult{}, false, nil
	default:
		return ApplyResult{}, true, err
	}
}

// ApplyLocal proposes a command on this node, which must be the leader; it
// never forwards. The leader stamps issued_at_ms, so the FSM never reads the
// clock (FA-CLU-07). The ClusterService uses it for forwarded commands, so
// that a forwarded command is not forwarded again.
func (n *Node) ApplyLocal(ctx context.Context, cmd *pb.Command) (ApplyResult, error) {
	if n.raft.State() != raft.Leader {
		return ApplyResult{}, fmt.Errorf("%w: not the leader", ErrNotApplied)
	}
	if cmd.GetVersion() == 0 {
		cmd.Version = minVersion(cmd)
	}
	// A forwarded command from a newer follower may need a version this leader
	// does not know yet (ADR 0018).
	if cmd.GetVersion() > n.MaxCommandVersion() {
		return ApplyResult{}, &CommandError{Code: CodeNotEnabled, Message: fmt.Sprintf(
			"command version %d is not supported by this node, which supports up to %d; the leader must be upgraded first",
			cmd.GetVersion(), n.MaxCommandVersion())}
	}
	if err := n.checkCommandLevel(ctx, cmd); err != nil {
		return ApplyResult{}, err
	}
	cmd.IssuedAtMs = n.now().UnixMilli()
	data, err := proto.Marshal(cmd)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: encode command: %w", ErrNotApplied, err)
	}

	timeout := defaultApplyTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	future := n.raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		switch {
		case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrEnqueueTimeout):
			return ApplyResult{}, fmt.Errorf("%w: %w", ErrNotApplied, err)
		default: // ErrLeadershipLost, ErrRaftShutdown, ...
			return ApplyResult{}, fmt.Errorf("%w: %w", ErrUnknownOutcome, err)
		}
	}
	res, ok := future.Response().(ApplyResult)
	if !ok {
		return ApplyResult{}, fmt.Errorf("%w: unexpected FSM response %T", ErrUnknownOutcome, future.Response())
	}
	return res, res.Err
}

// heartbeatTimeout is the Raft HeartbeatTimeout of this node (see Start).
func (n *Node) heartbeatTimeout() time.Duration {
	if n.cfg.Timeout > 0 {
		return n.cfg.Timeout
	}
	return raft.DefaultConfig().HeartbeatTimeout
}

// waitApplied waits until the local FSM applied index. It gives up at ctx's
// end: the command is committed anyway, so the caller still gets the result.
func (n *Node) waitApplied(ctx context.Context, index uint64) {
	ticker := time.NewTicker(appliedPoll)
	defer ticker.Stop()
	for n.fsm.AppliedIndex() < index {
		select {
		case <-ctx.Done():
			n.logger.Debug("result not applied locally before the deadline", "index", index, "applied", n.fsm.AppliedIndex(), "err", ctx.Err())
			return
		case <-ticker.C:
		}
	}
}

// leaderContactStale reports whether the last contact with the leader is too
// old for forwarding: the leader is presumed gone when it was not heard from
// for twice the heartbeat timeout, and a zero time means never.
func leaderContactStale(last, now time.Time, heartbeat time.Duration) bool {
	return last.IsZero() || now.Sub(last) > 2*heartbeat
}

// nextBackoff doubles the retry pause up to forwardBackoffLimit.
func nextBackoff(d time.Duration) time.Duration {
	return min(d*2, forwardBackoffLimit)
}

// sleepContext pauses for d and reports false if ctx ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func forwardTimeoutError(ctx context.Context) error {
	return fmt.Errorf("%w: forwarding to the leader timed out: %w", ErrNotApplied, ctx.Err())
}

// Status reports the Raft and replication state (proto GetStatusResponse,
// ADR 0012 A9).
type Status struct {
	NodeID        string
	RaftState     string
	AppliedIndex  uint64
	CommitIndex   uint64
	LastLogIndex  uint64
	LastContactMs int64
	LeaderID      string
	LeaderAddress string
}

// Status returns the current status of this node.
func (n *Node) Status() Status {
	stats := n.raft.Stats()
	commit, _ := strconv.ParseUint(stats["commit_index"], 10, 64)
	leaderAddr, leaderID := n.raft.LeaderWithID()
	var lastContact int64
	if n.raft.State() != raft.Leader {
		if t := n.raft.LastContact(); !t.IsZero() {
			lastContact = time.Since(t).Milliseconds()
		}
	}
	return Status{
		NodeID:    n.cfg.NodeID,
		RaftState: n.raft.State().String(),
		// Raft's applied index also counts configuration entries, which never
		// reach the FSM; a joiner's own AddNonvoter entry is one of them, and
		// the promotion waits for it (ADR 0012 A3).
		AppliedIndex:  max(n.fsm.AppliedIndex(), n.raft.AppliedIndex()),
		CommitIndex:   commit,
		LastLogIndex:  n.raft.LastIndex(),
		LastContactMs: lastContact,
		LeaderID:      string(leaderID),
		LeaderAddress: string(leaderAddr),
	}
}

// Member is a server of the Raft configuration.
type Member struct {
	ID      string
	Address string
	Voter   bool
}

// Members returns the current Raft configuration and its index, which
// membership changes pass as prevIndex so that a concurrent change makes
// them fail instead of overwriting it (ADR 0012 A4).
func (n *Node) Members() ([]Member, uint64, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, 0, err
	}
	var out []Member
	for _, s := range f.Configuration().Servers {
		out = append(out, Member{ID: string(s.ID), Address: string(s.Address), Voter: s.Suffrage == raft.Voter})
	}
	return out, f.Index(), nil
}

// ErrMembershipChanged means the configuration changed since prevIndex.
var ErrMembershipChanged = errors.New("cluster membership changed concurrently")

func membershipError(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		return fmt.Errorf("%w: %w", ErrNotApplied, err)
	case err != nil && strings.Contains(err.Error(), "configuration changed since"):
		return fmt.Errorf("%w: %w", ErrMembershipChanged, err)
	}
	return err
}

// AddNonvoter adds a server that receives the log but does not vote (or
// updates its address) and returns the index of the configuration entry.
func (n *Node) AddNonvoter(id, address string, prevIndex uint64, timeout time.Duration) (uint64, error) {
	f := n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(address), prevIndex, timeout)
	if err := f.Error(); err != nil {
		return 0, membershipError(err)
	}
	return f.Index(), nil
}

// AddVoter promotes a server to voter (or updates the address of a voter).
func (n *Node) AddVoter(id, address string, prevIndex uint64, timeout time.Duration) (uint64, error) {
	f := n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), prevIndex, timeout)
	if err := f.Error(); err != nil {
		return 0, membershipError(err)
	}
	return f.Index(), nil
}

// RemoveServer removes a server from the configuration.
func (n *Node) RemoveServer(id string, prevIndex uint64, timeout time.Duration) (uint64, error) {
	f := n.raft.RemoveServer(raft.ServerID(id), prevIndex, timeout)
	if err := f.Error(); err != nil {
		return 0, membershipError(err)
	}
	return f.Index(), nil
}

// Leader returns the address and ID of the current leader ("" if unknown).
func (n *Node) Leader() (address, id string) {
	a, i := n.raft.LeaderWithID()
	return string(a), string(i)
}

// ID returns this node's ID.
func (n *Node) ID() string { return n.cfg.NodeID }

// Snapshot forces a Raft snapshot (used by tests and admin tooling).
func (n *Node) Snapshot() error {
	return n.raft.Snapshot().Error()
}

// OnPeerResumed calls fn with the ID of a peer whose heartbeats resumed after
// a failure and with the ID of every peer added to the configuration, until
// the node shuts down. fn must not block for long; it runs on its own
// goroutine. Raft emits these observations on the leader only, so followers
// rely on the reconnect of their push streams (ADR 0012 B6).
func (n *Node) OnPeerResumed(fn func(peerID string)) {
	// Non-blocking: Raft must never wait for this observer. A full channel
	// drops an event, which is acceptable because anti-entropy repairs gaps.
	ch := make(chan raft.Observation, 64)
	obs := raft.NewObserver(ch, false, func(o *raft.Observation) bool {
		switch d := o.Data.(type) {
		case raft.ResumedHeartbeatObservation:
			return true
		case raft.PeerObservation:
			return !d.Removed
		}
		return false
	})
	n.raft.RegisterObserver(obs)
	n.observers = append(n.observers, obs)
	go func() {
		for {
			select {
			case <-n.done:
				return
			case o := <-ch:
				switch d := o.Data.(type) {
				case raft.ResumedHeartbeatObservation:
					fn(string(d.PeerID))
				case raft.PeerObservation:
					fn(string(d.Peer.ID))
				}
			}
		}
	}()
}

// OnHeartbeat calls fn with the ID of a peer whose heartbeat from the leader
// failed (ok false) or resumed after failures (ok true), with the time of the
// observation, until the node shuts down. fn must not block for long; it runs
// on its own goroutine. Raft emits these observations on the leader only (ADR
// 0017).
func (n *Node) OnHeartbeat(fn func(peerID string, ok bool, at time.Time)) {
	// Non-blocking, like OnPeerResumed: a full channel drops an observation,
	// and the next failure or resume follows anyway.
	ch := make(chan raft.Observation, 64)
	obs := raft.NewObserver(ch, false, func(o *raft.Observation) bool {
		switch o.Data.(type) {
		case raft.FailedHeartbeatObservation, raft.ResumedHeartbeatObservation:
			return true
		}
		return false
	})
	n.raft.RegisterObserver(obs)
	n.observers = append(n.observers, obs)
	go func() {
		for {
			select {
			case <-n.done:
				return
			case o := <-ch:
				switch d := o.Data.(type) {
				case raft.FailedHeartbeatObservation:
					fn(string(d.PeerID), false, n.now())
				case raft.ResumedHeartbeatObservation:
					fn(string(d.PeerID), true, n.now())
				}
			}
		}
	}()
}

// Shutdown hands over leadership if there are other voters, stops Raft and
// closes the log store (FA-NODE-04).
func (n *Node) Shutdown() error {
	n.closeOnce.Do(func() {
		for _, obs := range n.observers {
			n.raft.DeregisterObserver(obs)
		}
		close(n.done)
	})
	if n.IsLeader() {
		if cfg := n.raft.GetConfiguration(); cfg.Error() == nil && len(cfg.Configuration().Servers) > 1 {
			if err := n.raft.LeadershipTransfer().Error(); err != nil {
				n.logger.Warn("leadership transfer failed", "err", err)
			}
		}
	}
	err := n.raft.Shutdown().Error()
	return errors.Join(err, n.bolt.Close())
}

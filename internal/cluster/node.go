// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

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
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
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
	// single-member cluster needs; the mTLS transport follows in phase 2.
	Transport raft.Transport

	Logger *slog.Logger
	// RaftLogLevel and RaftLogJSON configure the library logger.
	RaftLogLevel string
	RaftLogJSON  bool
}

// Node runs Raft with the FSM on top of state.db.
type Node struct {
	cfg    Config
	raft   *raft.Raft
	fsm    *FSM
	bolt   *raftboltdb.BoltStore
	logger *slog.Logger
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

	bolt, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("open raft log store: %w", err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, retainedSnapshots, hlog)
	if err != nil {
		_ = bolt.Close()
		return nil, fmt.Errorf("open snapshot store: %w", err)
	}
	fsm, err := NewFSM(ctx, state, cfg.Logger, onFatal)
	if err != nil {
		_ = bolt.Close()
		return nil, err
	}

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

	n := &Node{cfg: cfg, raft: r, fsm: fsm, bolt: bolt, logger: log}
	if existing {
		log.Info("resuming from existing raft state", "node_id", cfg.NodeID,
			"applied_index", fsm.AppliedIndex(), "skip_snapshot_restore", rc.NoSnapshotRestoreOnStart)
		return n, nil
	}

	log.Info("no raft state found, bootstrapping a single-member cluster", "node_id", cfg.NodeID, "address", cfg.Advertise)
	err = r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{
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

func orDefault(v, def uint64) uint64 {
	if v == 0 {
		return def
	}
	return v
}

// FSM returns the state machine.
func (n *Node) FSM() *FSM { return n.fsm }

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

// Apply proposes a command and waits until it is applied on this node. The
// leader stamps issued_at_ms, so the FSM never reads the clock (FA-CLU-07).
// Forwarding from followers follows in phase 2 (FA-CLU-05).
func (n *Node) Apply(ctx context.Context, cmd *pb.Command) (ApplyResult, error) {
	if n.raft.State() != raft.Leader {
		return ApplyResult{}, fmt.Errorf("%w: not the leader", ErrNotApplied)
	}
	if cmd.GetVersion() == 0 {
		cmd.Version = CommandVersion
	}
	cmd.IssuedAtMs = time.Now().UnixMilli()
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
}

// Status returns the current status of this node.
func (n *Node) Status() Status {
	stats := n.raft.Stats()
	commit, _ := strconv.ParseUint(stats["commit_index"], 10, 64)
	_, leaderID := n.raft.LeaderWithID()
	var lastContact int64
	if n.raft.State() != raft.Leader {
		if t := n.raft.LastContact(); !t.IsZero() {
			lastContact = time.Since(t).Milliseconds()
		}
	}
	return Status{
		NodeID:        n.cfg.NodeID,
		RaftState:     n.raft.State().String(),
		AppliedIndex:  n.fsm.AppliedIndex(),
		CommitIndex:   commit,
		LastLogIndex:  n.raft.LastIndex(),
		LastContactMs: lastContact,
		LeaderID:      string(leaderID),
	}
}

// Snapshot forces a Raft snapshot (used by tests and admin tooling).
func (n *Node) Snapshot() error {
	return n.raft.Snapshot().Error()
}

// Shutdown hands over leadership if there are other voters, stops Raft and
// closes the log store (FA-NODE-04).
func (n *Node) Shutdown() error {
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

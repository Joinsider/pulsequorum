// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
	bboltErrors "go.etcd.io/bbolt/errors"
)

// ErrNodeRunning means that a running node holds the Raft log (ADR 0019).
var ErrNodeRunning = errors.New("the node is still running: stop it first (the Raft log is locked)")

const (
	// raftLockTimeout bounds the wait for the bbolt file lock of a running node.
	raftLockTimeout = time.Second
	// recoveryBackupPrefix starts the name of a backup directory (ADR 0019).
	recoveryBackupPrefix = "recovery-backup-"
	// recoveryBackupLayout formats the time of a backup directory (UTC).
	recoveryBackupLayout = "20060102T150405Z"
)

// errInspectOnly is returned by untouchedFSM: an inspection never touches the FSM.
var errInspectOnly = errors.New("inspection must not touch the state machine")

// RaftInfo is the offline view of a node's Raft state (ADR 0019).
type RaftInfo struct {
	NodeID       string       `json:"nodeId"`
	LastIndex    uint64       `json:"lastIndex"`
	LastTerm     uint64       `json:"lastTerm"`
	AppliedIndex uint64       `json:"appliedIndex"`
	CommandLevel uint32       `json:"commandLevel"`
	Servers      []RaftServer `json:"servers"`
}

// RaftServer is a server of the last Raft configuration.
type RaftServer struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Voter   bool   `json:"voter"`
}

// RecoveryPlan describes a recovery: the configuration before, the one that
// replaces it, and the members that are dropped (ADR 0019).
type RecoveryPlan struct {
	Old []RaftServer `json:"old"`
	New []RaftServer `json:"new"`
	// Removed holds the IDs of all other former members, sorted.
	Removed []string `json:"removed"`
	// Self is this node as listed in Old; New holds it as the only voter.
	Self RaftServer `json:"self"`
}

// InspectRaft reads the Raft state of a stopped node without changing it.
// The latest configuration is read with raft.GetConfiguration, which does not
// apply log entries: the snapshot is not restored, so the FSM is never called.
// A running node holds the log lock, which yields ErrNodeRunning.
func InspectRaft(ctx context.Context, dataDir, nodeID string, state StateStore) (RaftInfo, error) {
	if err := ctx.Err(); err != nil {
		return RaftInfo{}, err
	}
	if _, err := os.Stat(filepath.Join(dataDir, "raft", "raft.db")); errors.Is(err, fs.ErrNotExist) {
		return RaftInfo{}, fmt.Errorf("no Raft state in %s: this node never joined a cluster", dataDir)
	} else if err != nil {
		return RaftInfo{}, fmt.Errorf("inspect raft state: %w", err)
	}

	logs, err := openRaftLog(dataDir, true)
	if err != nil {
		return RaftInfo{}, err
	}
	defer func() { _ = logs.Close() }()
	snaps, err := openSnapshots(dataDir)
	if err != nil {
		return RaftInfo{}, err
	}

	conf := offlineRaftConfig(nodeID)
	// Without this, NewRaft restores the latest snapshot into the FSM.
	conf.NoSnapshotRestoreOnStart = true
	_, trans := raft.NewInmemTransport(raft.ServerAddress(nodeID))
	configuration, err := raft.GetConfiguration(conf, untouchedFSM{}, logs, inspectStable{logs}, snaps, trans)
	if err != nil {
		return RaftInfo{}, fmt.Errorf("read Raft configuration: %w", err)
	}

	lastIndex, lastTerm, err := lastLogPosition(logs, snaps)
	if err != nil {
		return RaftInfo{}, err
	}
	applied, err := state.AppliedIndex(ctx)
	if err != nil {
		return RaftInfo{}, fmt.Errorf("read applied index: %w", err)
	}
	level, err := state.CommandLevel(ctx)
	if err != nil {
		return RaftInfo{}, fmt.Errorf("read command level: %w", err)
	}

	servers := make([]RaftServer, 0, len(configuration.Servers))
	for _, s := range configuration.Servers {
		servers = append(servers, RaftServer{ID: string(s.ID), Address: string(s.Address), Voter: s.Suffrage == raft.Voter})
	}
	return RaftInfo{
		NodeID:       nodeID,
		LastIndex:    lastIndex,
		LastTerm:     lastTerm,
		AppliedIndex: applied,
		CommandLevel: EffectiveCommandLevel(level),
		Servers:      servers,
	}, nil
}

// PlanRecovery computes the configuration that makes this node the only voter.
// It fails if this node is not in the last configuration: such a node cannot
// take over the cluster, and another survivor must be recovered.
func PlanRecovery(info RaftInfo) (RecoveryPlan, error) {
	var self *RaftServer
	var removed []string
	for _, s := range info.Servers {
		if s.ID == info.NodeID {
			self = &RaftServer{ID: s.ID, Address: s.Address, Voter: s.Voter}
			continue
		}
		removed = append(removed, s.ID)
	}
	if self == nil {
		return RecoveryPlan{}, errors.New("this node is not a member of its last configuration; recover another node")
	}
	sort.Strings(removed)
	return RecoveryPlan{
		Old:     info.Servers,
		New:     []RaftServer{{ID: self.ID, Address: self.Address, Voter: true}},
		Removed: removed,
		Self:    *self,
	}, nil
}

// Recover makes this stopped node the only voter of a new configuration (ADR
// 0019). It backs up raft/ and state.db into DATA_DIR/recovery-backup-<time>/
// before it changes anything, and then runs raft.RecoverCluster. The FSM
// applies the log entries past the snapshot to state.db. It returns the plan
// and the backup directory; the backup directory is set even if the recovery
// fails afterwards.
func Recover(ctx context.Context, dataDir, nodeID string, state StateStore, log *slog.Logger, now time.Time) (RecoveryPlan, string, error) {
	info, err := InspectRaft(ctx, dataDir, nodeID, state)
	if err != nil {
		return RecoveryPlan{}, "", err
	}
	plan, err := PlanRecovery(info)
	if err != nil {
		return RecoveryPlan{}, "", err
	}
	if log == nil {
		log = slog.Default()
	}

	// Opening the log for writing fails while a node runs; that is checked
	// before the backup and before anything is written.
	logs, err := openRaftLog(dataDir, false)
	if err != nil {
		return plan, "", err
	}
	defer func() { _ = logs.Close() }()
	snaps, err := openSnapshots(dataDir)
	if err != nil {
		return plan, "", err
	}

	backupDir, err := backupForRecovery(ctx, dataDir, state, now)
	if err != nil {
		return plan, "", err
	}

	var fatalErr error
	fsm, err := NewFSM(ctx, state, log.With("component", "recovery"), func(err error) { fatalErr = err })
	if err != nil {
		return plan, backupDir, err
	}
	_, trans := raft.NewInmemTransport(raft.ServerAddress(plan.Self.Address))
	configuration := raft.Configuration{Servers: []raft.Server{{
		Suffrage: raft.Voter,
		ID:       raft.ServerID(plan.Self.ID),
		Address:  raft.ServerAddress(plan.Self.Address),
	}}}
	if err := raft.RecoverCluster(offlineRaftConfig(nodeID), fsm, logs, logs, snaps, trans, configuration); err != nil {
		return plan, backupDir, fmt.Errorf("recover cluster (backup in %s): %w", backupDir, err)
	}
	if fatalErr != nil {
		return plan, backupDir, fmt.Errorf("apply log to state (backup in %s): %w", backupDir, fatalErr)
	}

	if len(plan.Removed) > 0 {
		revoke := RecoveryRevoke{NodeIDs: plan.Removed, RecoveredAt: now.UTC()}
		if err := WriteRecoveryRevoke(dataDir, revoke); err != nil {
			return plan, backupDir, fmt.Errorf("record revoked nodes (backup in %s): %w", backupDir, err)
		}
	}
	log.Info("cluster recovered", "node_id", nodeID, "backup", backupDir, "revoked", len(plan.Removed))
	return plan, backupDir, nil
}

// inspectStable reads the stable store and drops writes. NewRaft writes back
// the current term it has just read, and an error there panics; the term is
// kept in memory only, so discarding the write leaves the file as it was.
type inspectStable struct{ raft.StableStore }

func (inspectStable) Set([]byte, []byte) error { return nil }

func (inspectStable) SetUint64([]byte, uint64) error { return nil }

// untouchedFSM is the FSM of an inspection. Nothing calls it, because the
// snapshot is not restored and the configuration is read from the log only.
type untouchedFSM struct{}

func (untouchedFSM) Apply(*raft.Log) any { return errInspectOnly }

func (untouchedFSM) Snapshot() (raft.FSMSnapshot, error) { return nil, errInspectOnly }

func (untouchedFSM) Restore(io.ReadCloser) error { return errInspectOnly }

// openRaftLog opens dataDir/raft/raft.db. A file lock that a running node holds
// blocks for raftLockTimeout and then yields ErrNodeRunning.
func openRaftLog(dataDir string, readOnly bool) (*raftboltdb.BoltStore, error) {
	logs, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dataDir, "raft", "raft.db"),
		BoltOptions: &bbolt.Options{Timeout: raftLockTimeout, ReadOnly: readOnly},
	})
	if errors.Is(err, bboltErrors.ErrTimeout) {
		return nil, ErrNodeRunning
	}
	if err != nil {
		return nil, fmt.Errorf("open raft log store: %w", err)
	}
	return logs, nil
}

func openSnapshots(dataDir string) (*raft.FileSnapshotStore, error) {
	snaps, err := raft.NewFileSnapshotStoreWithLogger(filepath.Join(dataDir, "raft"), retainedSnapshots, hclog.NewNullLogger())
	if err != nil {
		return nil, fmt.Errorf("open snapshot store: %w", err)
	}
	return snaps, nil
}

func offlineRaftConfig(nodeID string) *raft.Config {
	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(nodeID)
	conf.Logger = hclog.NewNullLogger()
	return conf
}

// lastLogPosition returns the index and term of the last log entry, or of the
// latest snapshot if the log is empty.
func lastLogPosition(logs *raftboltdb.BoltStore, snaps raft.SnapshotStore) (uint64, uint64, error) {
	lastIndex, err := logs.LastIndex()
	if err != nil {
		return 0, 0, fmt.Errorf("read last log index: %w", err)
	}
	if lastIndex > 0 {
		var entry raft.Log
		if err := logs.GetLog(lastIndex, &entry); err != nil {
			return 0, 0, fmt.Errorf("read last log entry: %w", err)
		}
		return entry.Index, entry.Term, nil
	}
	metas, err := snaps.List()
	if err != nil {
		return 0, 0, fmt.Errorf("list snapshots: %w", err)
	}
	if len(metas) == 0 {
		return 0, 0, nil
	}
	// List returns the newest snapshot first.
	return metas[0].Index, metas[0].Term, nil
}

// backupForRecovery copies raft/ and state.db into a new backup directory. On
// failure the directory is removed again, so nothing is left behind.
func backupForRecovery(ctx context.Context, dataDir string, state StateStore, now time.Time) (string, error) {
	dir := filepath.Join(dataDir, recoveryBackupPrefix+now.UTC().Format(recoveryBackupLayout))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create backup directory: %w", err)
	}
	if err := copyTree(filepath.Join(dataDir, "raft"), filepath.Join(dir, "raft")); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("back up raft state: %w", err)
	}
	if err := writeStateBackup(ctx, state, filepath.Join(dir, "state.db")); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("back up state.db: %w", err)
	}
	return dir, nil
}

// copyTree copies the regular files and directories of src to dst. Directories
// get mode 0700 and files 0600.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: path inside the data directory
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path inside the backup directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// writeStateBackup writes a consistent copy of state.db to path.
func writeStateBackup(ctx context.Context, state StateStore, path string) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path inside the backup directory
	if err != nil {
		return err
	}
	if err := state.WriteSnapshot(ctx, out); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

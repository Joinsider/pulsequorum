// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"
)

const recoveryTestNodeID = "11111111-1111-1111-1111-111111111111"

func recoveryTestConfig(dir string) Config {
	return Config{NodeID: recoveryTestNodeID, Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}
}

func TestPlanRecovery(t *testing.T) {
	t.Parallel()
	voter := func(id, addr string) RaftServer { return RaftServer{ID: id, Address: addr, Voter: true} }
	tests := []struct {
		name        string
		info        RaftInfo
		wantRemoved []string
		wantNew     RaftServer
		wantErr     string
	}{
		{
			name: "middle member of three voters",
			info: RaftInfo{NodeID: "b", Servers: []RaftServer{
				voter("a", "a:9443"), voter("b", "b:9443"), voter("c", "c:9443")}},
			wantRemoved: []string{"a", "c"},
			wantNew:     voter("b", "b:9443"),
		},
		{
			name: "removed IDs are sorted",
			info: RaftInfo{NodeID: "m", Servers: []RaftServer{
				voter("z", "z:1"), voter("c", "c:1"), voter("m", "m:1"), voter("k", "k:1")}},
			wantRemoved: []string{"c", "k", "z"},
			wantNew:     voter("m", "m:1"),
		},
		{
			name: "non-voter becomes the only voter",
			info: RaftInfo{NodeID: "b", Servers: []RaftServer{
				voter("a", "a:9443"), {ID: "b", Address: "b:9443", Voter: false}}},
			wantRemoved: []string{"a"},
			wantNew:     voter("b", "b:9443"),
		},
		{
			name:        "single member has nothing to revoke",
			info:        RaftInfo{NodeID: "a", Servers: []RaftServer{voter("a", "a:9443")}},
			wantRemoved: nil,
			wantNew:     voter("a", "a:9443"),
		},
		{
			name:    "node not in its last configuration",
			info:    RaftInfo{NodeID: "x", Servers: []RaftServer{voter("a", "a:9443"), voter("b", "b:9443")}},
			wantErr: "this node is not a member of its last configuration; recover another node",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanRecovery(tc.info)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("PlanRecovery() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(plan.Removed, tc.wantRemoved) {
				t.Errorf("Removed = %v, want %v", plan.Removed, tc.wantRemoved)
			}
			if len(plan.New) != 1 || plan.New[0] != tc.wantNew {
				t.Errorf("New = %+v, want [%+v]", plan.New, tc.wantNew)
			}
			if plan.Self.ID != tc.info.NodeID {
				t.Errorf("Self = %+v, want ID %q", plan.Self, tc.info.NodeID)
			}
			if !slices.Equal(plan.Old, tc.info.Servers) {
				t.Errorf("Old = %+v, want %+v", plan.Old, tc.info.Servers)
			}
		})
	}
}

// ADR 0019: a running node holds the Raft log; inspection reports that instead of waiting.
func TestInspectRaftWhileNodeRuns(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, recoveryTestConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Shutdown() }()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = InspectRaft(ctx, dir, recoveryTestNodeID, st)
	if !errors.Is(err, ErrNodeRunning) {
		t.Fatalf("InspectRaft() error = %v, want ErrNodeRunning", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("InspectRaft took %v while the node runs; want a prompt error", elapsed)
	}
}

func TestInspectRaftWithoutState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	_, err := InspectRaft(ctx, dir, recoveryTestNodeID, st)
	if err == nil || !strings.Contains(err.Error(), "no Raft state in") {
		t.Fatalf("InspectRaft() error = %v, want no Raft state", err)
	}
}

// ADR 0019: inspection must not apply log entries. A command that is in the log
// but not yet in state.db (planted after shutdown) must stay out of state.db.
// Recovery applies it.
func TestInspectRaftLeavesStateUntouched(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, recoveryTestConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	cmd, _ := NewPutTenant("t1", "default", TenantDocument{Name: "Default", Plan: "default"})
	if _, err := n.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := n.Snapshot(); err != nil {
		t.Fatal(err)
	}
	cmd, _ = NewPutMonitor("m1", "default", "m1", monitorDoc("one"), 0)
	if _, err := n.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// Append an entry that state.db has not applied.
	planted := plantCommandEntry(t, dir, "extra-req", "extra")
	appliedBefore, _ := st.AppliedIndex(ctx)
	fingerprintBefore, _ := st.Fingerprint(ctx)

	info, err := InspectRaft(ctx, dir, recoveryTestNodeID, st)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastIndex != planted.Index || info.LastTerm != planted.Term {
		t.Errorf("last log = %d/%d, want %d/%d", info.LastIndex, info.LastTerm, planted.Index, planted.Term)
	}
	if info.AppliedIndex != appliedBefore {
		t.Errorf("AppliedIndex = %d, want unchanged %d", info.AppliedIndex, appliedBefore)
	}
	if fp, _ := st.Fingerprint(ctx); fp != fingerprintBefore {
		t.Error("InspectRaft changed state.db")
	}
	if applied, _ := st.AppliedIndex(ctx); applied != appliedBefore {
		t.Errorf("state applied index = %d after inspection, want %d", applied, appliedBefore)
	}

	// Recovery is the only step that applies the entry.
	if _, _, err := Recover(ctx, dir, recoveryTestNodeID, st, slog.New(slog.DiscardHandler), time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if applied, _ := st.AppliedIndex(ctx); applied != planted.Index {
		t.Errorf("state applied index after recovery = %d, want %d", applied, planted.Index)
	}
	if fp, _ := st.Fingerprint(ctx); fp == fingerprintBefore {
		t.Error("recovery did not apply the log entry to state.db")
	}
}

// plantCommandEntry appends a tenant command to the Raft log of the stopped node
// in dir and returns the entry.
func plantCommandEntry(t *testing.T, dir, requestID, tenantID string) raft.Log {
	t.Helper()
	cmd, err := NewPutTenant(requestID, tenantID, TenantDocument{Name: "Extra", Plan: "default"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Version = minVersion(cmd)
	cmd.IssuedAtMs = time.Now().UnixMilli()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatal(err)
	}
	return appendLogEntry(t, dir, raft.LogCommand, data)
}

// plantConfigurationEntry appends a configuration entry with the given servers
// to the Raft log of the stopped node in dir.
func plantConfigurationEntry(t *testing.T, dir string, servers ...raft.Server) raft.Log {
	t.Helper()
	data := raft.EncodeConfiguration(raft.Configuration{Servers: servers})
	return appendLogEntry(t, dir, raft.LogConfiguration, data)
}

// appendLogEntry appends an entry after the last one of the log in dir.
func appendLogEntry(t *testing.T, dir string, typ raft.LogType, data []byte) raft.Log {
	t.Helper()
	logs, err := raftboltdb.NewBoltStore(filepath.Join(dir, "raft", "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	last, err := logs.LastIndex()
	if err != nil {
		t.Fatal(err)
	}
	var prev raft.Log
	if err := logs.GetLog(last, &prev); err != nil {
		t.Fatal(err)
	}
	entry := raft.Log{Index: prev.Index + 1, Term: prev.Term, Type: typ, Data: data}
	if err := logs.StoreLog(&entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

// ADR 0019: the members that recovery drops are written to the revoke file, so
// that they are revoked once the recovered node leads again.
func TestRecoverRevokesDroppedMembers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, recoveryTestConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// Three voters: this node in the middle, two members that were lost.
	plantConfigurationEntry(t, dir,
		raft.Server{Suffrage: raft.Voter, ID: "node-c", Address: "node-c:9443"},
		raft.Server{Suffrage: raft.Voter, ID: recoveryTestNodeID, Address: "node1:9443"},
		raft.Server{Suffrage: raft.Voter, ID: "node-b", Address: "node-b:9443"},
	)

	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	plan, _, err := Recover(ctx, dir, recoveryTestNodeID, st, slog.New(slog.DiscardHandler), now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"node-b", "node-c"}; !slices.Equal(plan.Removed, want) {
		t.Errorf("Removed = %v, want %v", plan.Removed, want)
	}
	revoke, ok, err := ReadRecoveryRevoke(dir)
	if err != nil || !ok {
		t.Fatalf("ReadRecoveryRevoke() = %+v, %v, %v; want the revoke file", revoke, ok, err)
	}
	if want := []string{"node-b", "node-c"}; !slices.Equal(revoke.NodeIDs, want) {
		t.Errorf("revoked = %v, want %v", revoke.NodeIDs, want)
	}
	if !revoke.RecoveredAt.Equal(now) {
		t.Errorf("RecoveredAt = %v, want %v", revoke.RecoveredAt, now)
	}
}

// ADR 0019: recovery of a single-member cluster keeps all data, writes a backup
// and starts again as leader with the earlier data.
func TestRecoverSingleNode(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, recoveryTestConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	cmd, _ := NewPutTenant("t1", "default", TenantDocument{Name: "Default", Plan: "default"})
	if _, err := n.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	cmd, _ = NewPutMonitor("m1", "default", "m1", monitorDoc("one"), 0)
	if _, err := n.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := st.Fingerprint(ctx)
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	plan, backupDir, err := Recover(ctx, dir, recoveryTestNodeID, st, slog.New(slog.DiscardHandler), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Removed) != 0 {
		t.Errorf("Removed = %v, want none", plan.Removed)
	}
	if want := filepath.Join(dir, "recovery-backup-20261025T120000Z"); backupDir != want {
		t.Errorf("backup dir = %q, want %q", backupDir, want)
	}
	for _, p := range []string{filepath.Join(backupDir, "state.db"), filepath.Join(backupDir, "raft", "raft.db")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("backup file missing: %v", err)
			continue
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, RecoveryRevokeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("revoke file exists without removed nodes: %v", err)
	}
	if fp, _ := st.Fingerprint(ctx); fp != fingerprint {
		t.Error("recovery changed the data of a single-member cluster")
	}

	n2, err := Start(ctx, recoveryTestConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n2.Shutdown() }()
	if err := n2.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	cmd, _ = NewPutMonitor("m1", "default", "m1", monitorDoc("one"), 0)
	res, err := n2.Apply(ctx, cmd)
	if err != nil || !res.Duplicate {
		t.Errorf("retry after recovery = %+v, %v; want deduplicated", res, err)
	}
}

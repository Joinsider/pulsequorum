// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

func openStore(t *testing.T, dir string) *sqlite.StateStore {
	t.Helper()
	s, err := sqlite.OpenState(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func monitorDoc(name string) MonitorDocument {
	return MonitorDocument{Name: name, Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority", Tags: []string{"b", "a"}}
}

// commandLog builds a fixed sequence including duplicates, a conflict, an
// invalid command and undecodable bytes.
func commandLog(t *testing.T) []*raft.Log {
	t.Helper()
	must := func(c *pb.Command, err error) *pb.Command {
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cmds := []*pb.Command{
		must(NewPutTenant("r1", "default", TenantDocument{Name: "Default", Plan: "default"})),
		must(NewPutMonitor("r2", "default", "m1", monitorDoc("one"), 0)),
		must(NewPutMonitor("r2", "default", "m1", monitorDoc("one"), 0)), // duplicate request
		must(NewPutMonitor("r3", "default", "m1", monitorDoc("one v2"), 1)),
		must(NewPutMonitor("r4", "default", "m1", monitorDoc("stale"), 1)), // conflict
		must(NewPutMonitor("r5", "missing-tenant", "m2", monitorDoc("x"), 0)),
		NewSetFeatureLevel("r6", 0), // invalid level
		NewDeleteMonitor("r7", "default", "m1"),
		NewRegisterNode("r8", store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleVoter,
			Capabilities: []string{"icmp", "http"}, Labels: map[string]string{"env": "prod"},
			Region: "eu", Version: "1.0.0", MaxCommandVersion: 2}),
		NewSetNodeHealth("r9", "node-1", false),
	}
	logs := make([]*raft.Log, 0, len(cmds)+1)
	for i, c := range cmds {
		c.IssuedAtMs = int64(1_000 + i)
		data, err := proto.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		logs = append(logs, &raft.Log{Index: uint64(i + 1), Type: raft.LogCommand, Data: data})
	}
	return append(logs, &raft.Log{Index: uint64(len(cmds) + 1), Type: raft.LogCommand, Data: []byte{0xff, 0x01}})
}

// TestFSMDeterminism applies the same log to two FSMs and compares the
// resulting state (FA-CLU-07).
func TestFSMDeterminism(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var prints []string
	for range 2 {
		st := openStore(t, t.TempDir())
		fsm, err := NewFSM(ctx, st, slog.New(slog.DiscardHandler), func(err error) { t.Fatalf("unexpected halt: %v", err) })
		if err != nil {
			t.Fatal(err)
		}
		var results []ApplyResult
		for _, l := range commandLog(t) {
			results = append(results, fsm.Apply(l).(ApplyResult))
		}
		if !results[2].Duplicate || string(results[2].Result) != string(results[1].Result) {
			t.Errorf("duplicate request not deduplicated: %+v", results[2])
		}
		if !errors.Is(results[4].Err, store.ErrConflict) || !errors.Is(results[5].Err, store.ErrNotFound) {
			t.Errorf("conflict/not found results: %v / %v", results[4].Err, results[5].Err)
		}
		last := len(results) - 1
		if !errors.Is(results[6].Err, ErrInvalidCommand) || !errors.Is(results[last].Err, ErrInvalidCommand) {
			t.Errorf("invalid results: %v / %v", results[6].Err, results[last].Err)
		}
		if results[8].Err != nil || results[9].Err != nil {
			t.Errorf("node commands: %v / %v", results[8].Err, results[9].Err)
		}
		if idx, _ := st.AppliedIndex(ctx); idx != uint64(len(results)) || fsm.AppliedIndex() != uint64(len(results)) {
			t.Errorf("applied index = %d/%d, want %d", idx, fsm.AppliedIndex(), len(results))
		}
		// Replayed entries are skipped (ADR 0012 A2).
		if r := fsm.Apply(commandLog(t)[1]).(ApplyResult); r.Result != nil || r.Err != nil {
			t.Errorf("replayed entry applied again: %+v", r)
		}
		fp, err := st.Fingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		prints = append(prints, fp)
	}
	if prints[0] != prints[1] {
		t.Fatalf("replicas diverged: %s vs %s", prints[0], prints[1])
	}
}

func TestFSMHaltsOnStoreFailure(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	halted := make(chan error, 1)
	fsm, err := NewFSM(context.Background(), st, slog.New(slog.DiscardHandler), func(err error) { halted <- err })
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	res := fsm.Apply(commandLog(t)[0]).(ApplyResult)
	if !errors.Is(res.Err, ErrFSMHalted) || fsm.Healthy() == nil {
		t.Fatalf("result = %+v, want ErrFSMHalted", res)
	}
	select {
	case err := <-halted:
		if !errors.Is(err, ErrFSMHalted) {
			t.Errorf("onFatal error = %v, want ErrFSMHalted", err)
		}
	default:
		t.Fatal("onFatal not called")
	}
}

// TestNodeBootstrapAndRestart covers FA-CLU-01/02 and the snapshot path.
func TestNodeBootstrapAndRestart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	cfg := Config{NodeID: "11111111-1111-1111-1111-111111111111", Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}

	st := openStore(t, dir)
	n, err := Start(ctx, cfg, st, nil)
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
	before, _ := st.Fingerprint(ctx)
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}

	n2, err := Start(ctx, cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n2.Shutdown() }()
	if err := n2.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _ := st.Fingerprint(ctx); after != before {
		t.Error("state changed by restart")
	}
	cmd, _ = NewPutMonitor("m1", "default", "m1", monitorDoc("one"), 0)
	res, err := n2.Apply(ctx, cmd)
	if err != nil || !res.Duplicate {
		t.Errorf("retry after restart = %+v, %v; want deduplicated", res, err)
	}
	if s := n2.Status(); s.RaftState != "Leader" || s.AppliedIndex < 3 {
		t.Errorf("status = %+v", s)
	}
}

func TestEnsureDefaultTenantAndReady(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)
	n, err := Start(ctx, Config{NodeID: "22222222-2222-2222-2222-222222222222", Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Shutdown() }()

	for range 2 { // idempotent
		if err := EnsureDefaultTenant(ctx, n, st); err != nil {
			t.Fatal(err)
		}
	}
	if tn, err := st.GetTenant(ctx, DefaultTenantID); err != nil || tn.Name != "Default" {
		t.Fatalf("default tenant = %+v, %v", tn, err)
	}
	if err := n.Ready(); err != nil {
		t.Errorf("Ready = %v, want nil", err)
	}
}

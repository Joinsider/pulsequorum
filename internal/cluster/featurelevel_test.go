// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

// ADR 0018: SetFeatureLevel raises the stored command level and never lowers it.
func TestApplySetFeatureLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		steps      []uint32
		wantErr    error
		wantResult uint32
		wantStored uint32
	}{
		{"raise above base", []uint32{3}, nil, 3, 3},
		{"equal to base is a no-op", []uint32{BaseCommandLevel}, nil, BaseCommandLevel, 0},
		{"lower after raise keeps level", []uint32{3, 2}, nil, 3, 3},
		{"zero is invalid", []uint32{0}, ErrInvalidCommand, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFSM(t)
			var res ApplyResult
			for _, level := range tt.steps {
				res = f.apply(NewSetFeatureLevel("", level), 1_000)
			}
			if !errors.Is(res.Err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", res.Err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			var got FeatureLevelResult
			if err := json.Unmarshal(res.Result, &got); err != nil {
				t.Fatal(err)
			}
			if got.Level != tt.wantResult {
				t.Errorf("result level = %d, want %d", got.Level, tt.wantResult)
			}
			stored, err := f.st.CommandLevel(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stored != tt.wantStored {
				t.Errorf("stored level = %d, want %d", stored, tt.wantStored)
			}
		})
	}
}

// versionThreeNodeHealth is a command that needs version 3, to test the gate
// without a real version-3 payload.
func versionThreeNodeHealth(requestID string) *pb.Command {
	cmd := NewSetNodeHealth(requestID, "node-x", true)
	cmd.Version = 3
	return cmd
}

func startGateNode(ctx context.Context, t *testing.T, dir string, st StateStore, maxVersion uint32) *Node {
	t.Helper()
	n, err := startNodeWithMax(ctx, dir, st, maxVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Shutdown() })
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	return n
}

func startNodeWithMax(ctx context.Context, dir string, st StateStore, maxVersion uint32) (*Node, error) {
	return Start(ctx, Config{NodeID: "33333333-3333-3333-3333-333333333333", Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error",
		MaxCommandVersion: maxVersion}, st, nil)
}

func notEnabled(err error) bool {
	var cmdErr *CommandError
	return errors.As(err, &cmdErr) && cmdErr.Code == CodeNotEnabled
}

// ADR 0018: a command needing a version the cluster does not have yet is
// rejected before it is proposed.
func TestCommandLevelGate(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("rejected until the level is raised", func(t *testing.T) {
		dir := t.TempDir()
		n := startGateNode(ctx, t, dir, openStore(t, dir), 3)
		if _, err := n.Apply(ctx, versionThreeNodeHealth("g1")); !notEnabled(err) {
			t.Fatalf("err = %v, want CodeNotEnabled while level is 2", err)
		}
		if _, err := n.Apply(ctx, NewSetFeatureLevel("g2", 3)); err != nil {
			t.Fatal(err)
		}
		if level, err := n.CommandLevel(ctx); err != nil || level != 3 {
			t.Fatalf("level = %d, %v; want 3", level, err)
		}
		cmd := NewSetFeatureLevel("g3", 3)
		cmd.Version = 3
		if _, err := n.Apply(ctx, cmd); err != nil {
			t.Errorf("version-3 command after raising the level: %v", err)
		}
	})

	t.Run("node that cannot apply version 3 rejects it locally", func(t *testing.T) {
		dir := t.TempDir()
		n := startGateNode(ctx, t, dir, openStore(t, dir), 2)
		if _, err := n.Apply(ctx, NewSetFeatureLevel("h1", 3)); err != nil {
			t.Fatal(err)
		}
		if _, err := n.ApplyLocal(ctx, versionThreeNodeHealth("h2")); !notEnabled(err) {
			t.Errorf("ApplyLocal err = %v, want CodeNotEnabled from the node's own maximum", err)
		}
		if _, err := n.Apply(ctx, versionThreeNodeHealth("h3")); !notEnabled(err) {
			t.Errorf("Apply err = %v, want CodeNotEnabled", err)
		}
	})
}

// ADR 0018: a binary refuses to start on state that needs a newer version.
func TestStartRefusesNewerCommandLevel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	st := openStore(t, dir)

	n, err := startNodeWithMax(ctx, dir, st, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Apply(ctx, NewSetFeatureLevel("s1", 3)); err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}

	if _, err := startNodeWithMax(ctx, dir, st, 2); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("start with version 2 on level 3: err = %v, want ErrIncompatibleVersion", err)
	}

	n2, err := startNodeWithMax(ctx, dir, st, 3)
	if err != nil {
		t.Fatalf("start with version 3 on level 3: %v", err)
	}
	if err := n2.Shutdown(); err != nil {
		t.Fatal(err)
	}
}

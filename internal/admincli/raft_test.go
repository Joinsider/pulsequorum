// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/node"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

const testNodeID = "11111111-1111-1111-1111-111111111111"

// prepareNode creates a stopped single-member node in a new data directory.
func prepareNode(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	writeNodeID(t, dir, testNodeID)

	st, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := cluster.Start(ctx, nodeConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}
	cmd, _ := cluster.NewPutTenant("t1", "default", cluster.TenantDocument{Name: "Default", Plan: "default"})
	if _, err := n.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeNodeID(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, node.IDFile), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nodeConfig(dir string) cluster.Config {
	return cluster.Config{NodeID: testNodeID, Advertise: "node1:9443", DataDir: dir,
		Timeout: 200 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), RaftLogLevel: "error"}
}

// runCLI runs an admin command against dataDir and returns its exit code and output.
func runCLI(t *testing.T, dataDir string, args ...string) (int, string, string) {
	t.Helper()
	vars := map[string]string{"DATA_DIR": dataDir, "NODE_NAME": "node1"}
	var stdout, stderr bytes.Buffer
	env := Env{
		Getenv: func(key string) string { return vars[key] },
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	code := Run(context.Background(), args, env)
	return code, stdout.String(), stderr.String()
}

// dirEntries lists the names in dir, to detect any change made to it.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRaftInfoText(t *testing.T) {
	dir := prepareNode(t)
	code, out, errOut := runCLI(t, dir, "raft-info")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"node ID:        " + testNodeID, "applied index:", "command level:", "voter", "(this node)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRaftInfoJSON(t *testing.T) {
	dir := prepareNode(t)
	code, out, errOut := runCLI(t, dir, "raft-info", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var info cluster.RaftInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if info.NodeID != testNodeID {
		t.Errorf("NodeID = %q, want %q", info.NodeID, testNodeID)
	}
	if info.AppliedIndex == 0 || info.AppliedIndex > info.LastIndex {
		t.Errorf("AppliedIndex = %d, LastIndex = %d; want 0 < applied <= last", info.AppliedIndex, info.LastIndex)
	}
	if want := []cluster.RaftServer{{ID: testNodeID, Address: "node1:9443", Voter: true}}; !slices.Equal(info.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", info.Servers, want)
	}
	if !strings.Contains(out, `"nodeId"`) {
		t.Errorf("JSON lacks camelCase keys:\n%s", out)
	}
}

func TestRecoverDryRunChangesNothing(t *testing.T) {
	dir := prepareNode(t)
	before := dirEntries(t, dir)

	code, out, errOut := runCLI(t, dir, "recover")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"Nothing has been changed", "WARNING", "--yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if after := dirEntries(t, dir); !slices.Equal(before, after) {
		t.Errorf("dry run changed the data directory: %v -> %v", before, after)
	}
}

func TestRecoverDryRunJSON(t *testing.T) {
	dir := prepareNode(t)
	code, out, errOut := runCLI(t, dir, "recover", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var res recoverResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if res.Applied || res.BackupDir != "" {
		t.Errorf("dry run reports %+v, want not applied", res)
	}
	if res.Plan.Self.ID != testNodeID || len(res.Plan.Removed) != 0 {
		t.Errorf("plan = %+v", res.Plan)
	}
}

func TestRecoverYesCreatesBackup(t *testing.T) {
	dir := prepareNode(t)

	code, out, errOut := runCLI(t, dir, "recover", "--yes")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "Backup of raft/ and state.db:") || !strings.Contains(out, "Next steps") {
		t.Errorf("output lacks the backup path or the next steps:\n%s", out)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "recovery-backup-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup directories = %v, %v; want exactly one", backups, err)
	}
	for _, p := range []string{filepath.Join(backups[0], "state.db"), filepath.Join(backups[0], "raft", "raft.db")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("backup lacks %s: %v", p, err)
		}
	}
	if !strings.Contains(out, backups[0]) {
		t.Errorf("output does not name the backup directory %s:\n%s", backups[0], out)
	}

	// The recovered node is still readable with the same tools.
	if code, _, errOut := runCLI(t, dir, "raft-info"); code != ExitOK {
		t.Errorf("raft-info after recovery: exit %d, stderr %q", code, errOut)
	}
}

func TestRecoverRefusesRunningNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := prepareNode(t)

	st, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	n, err := cluster.Start(ctx, nodeConfig(dir), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Shutdown() }()
	if err := n.WaitLeader(ctx); err != nil {
		t.Fatal(err)
	}

	code, _, errOut := runCLI(t, dir, "recover", "--yes")
	if code != ExitError {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitError, errOut)
	}
	if !strings.Contains(errOut, "still running") {
		t.Errorf("stderr lacks the running-node hint: %q", errOut)
	}
	if backups, _ := filepath.Glob(filepath.Join(dir, "recovery-backup-*")); len(backups) != 0 {
		t.Errorf("backup created for a running node: %v", backups)
	}
}

func TestEmptyDataDirFails(t *testing.T) {
	for _, cmd := range []string{"raft-info", "recover"} {
		t.Run(cmd, func(t *testing.T) {
			dir := t.TempDir()
			code, _, errOut := runCLI(t, dir, cmd)
			if code != ExitError {
				t.Fatalf("exit %d, want %d; stderr %q", code, ExitError, errOut)
			}
			if !strings.Contains(errOut, "DATA_DIR") {
				t.Errorf("stderr lacks the DATA_DIR hint: %q", errOut)
			}
			if entries := dirEntries(t, dir); len(entries) != 0 {
				t.Errorf("command created files in an empty data directory: %v", entries)
			}
		})
	}
}

func TestFlagErrors(t *testing.T) {
	dir := prepareNode(t)
	tests := []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"help prints usage and succeeds", []string{"raft-info", "-h"}, ExitOK},
		{"help of recover", []string{"recover", "-help"}, ExitOK},
		{"unknown flag", []string{"raft-info", "--bogus"}, ExitUsage},
		{"unexpected argument", []string{"recover", "extra"}, ExitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCLI(t, dir, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d; stderr %q", code, tc.wantCode, errOut)
			}
		})
	}
}

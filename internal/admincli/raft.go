// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package admincli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store/sqlite"
)

// recoverResult is the JSON output of "recover".
type recoverResult struct {
	Applied   bool                 `json:"applied"`
	BackupDir string               `json:"backupDir,omitempty"`
	Plan      cluster.RecoveryPlan `json:"plan"`
}

// runRaftInfo prints the Raft log position and configuration (ADR 0019).
// -h prints the flags and counts as success (exit 0).
func runRaftInfo(ctx context.Context, cfg config.Config, args []string, env Env) error {
	flags := flag.NewFlagSet("raft-info", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print the Raft state as JSON")
	if help, err := parseFlags(flags, args, env); help || err != nil {
		return err
	}

	info, state, err := inspectNode(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()

	if *asJSON {
		return writeJSON(env.Stdout, info)
	}
	return writeRaftInfo(env.Stdout, info)
}

// runRecover makes this node the only voter after quorum loss (ADR 0019). Without
// --yes it only shows the plan.
func runRecover(ctx context.Context, cfg config.Config, args []string, env Env) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "perform the recovery; without it only the plan is shown")
	asJSON := flags.Bool("json", false, "print the plan and the result as JSON")
	if help, err := parseFlags(flags, args, env); help || err != nil {
		return err
	}

	info, state, err := inspectNode(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	plan, err := cluster.PlanRecovery(info)
	if err != nil {
		return err
	}

	if !*yes {
		if *asJSON {
			return writeJSON(env.Stdout, recoverResult{Plan: plan})
		}
		return writeRecoveryPlan(env.Stdout, info, plan)
	}

	logger := slog.New(slog.NewTextHandler(env.Stderr, nil))
	plan, backupDir, err := cluster.Recover(ctx, cfg.DataDir, info.NodeID, state, logger, time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, recoverResult{Applied: true, BackupDir: backupDir, Plan: plan})
	}
	return writeRecoveryDone(env.Stdout, plan, backupDir)
}

// parseFlags parses the flags of a command. It reports help=true after -h, when
// the usage has been printed; the command then returns nil.
func parseFlags(flags *flag.FlagSet, args []string, env Env) (help bool, err error) {
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(env.Stderr)
			_, _ = fmt.Fprintf(env.Stderr, "usage: pulsequorum admin %s [flags]\n\nflags:\n", flags.Name())
			flags.PrintDefaults()
			return true, nil
		}
		return false, usageError("%v", err)
	}
	if flags.NArg() > 0 {
		return false, usageError("unexpected argument %q", flags.Arg(0))
	}
	return false, nil
}

// inspectNode reads the Raft state of the stopped node in cfg.DataDir. The
// returned state store is open and must be closed by the caller. Nothing is
// created: a data directory without state.db or node ID is an error.
func inspectNode(ctx context.Context, cfg config.Config) (cluster.RaftInfo, *sqlite.StateStore, error) {
	id, err := nodeID(cfg.DataDir)
	if err != nil {
		return cluster.RaftInfo{}, nil, err
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, sqlite.StateFile)); errors.Is(err, fs.ErrNotExist) {
		return cluster.RaftInfo{}, nil, fmt.Errorf("no %s in %s (is DATA_DIR set correctly?)", sqlite.StateFile, cfg.DataDir)
	} else if err != nil {
		return cluster.RaftInfo{}, nil, fmt.Errorf("check %s: %w", sqlite.StateFile, err)
	}
	state, err := sqlite.OpenState(ctx, cfg.DataDir)
	if err != nil {
		return cluster.RaftInfo{}, nil, fmt.Errorf("open state: %w", err)
	}
	info, err := cluster.InspectRaft(ctx, cfg.DataDir, id, state)
	if err != nil {
		_ = state.Close()
		return cluster.RaftInfo{}, nil, err
	}
	return info, state, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeRaftInfo(w io.Writer, info cluster.RaftInfo) error {
	var b strings.Builder
	fmt.Fprintf(&b, "node ID:        %s\n", info.NodeID)
	fmt.Fprintf(&b, "last log index: %d (term %d)\n", info.LastIndex, info.LastTerm)
	fmt.Fprintf(&b, "applied index:  %d\n", info.AppliedIndex)
	fmt.Fprintf(&b, "command level:  %d\n\n", info.CommandLevel)
	fmt.Fprintf(&b, "last configuration (%d servers):\n", len(info.Servers))
	serverTable(&b, info.Servers, info.NodeID)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeRecoveryPlan(w io.Writer, info cluster.RaftInfo, plan cluster.RecoveryPlan) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Recovery plan for node %s. Nothing has been changed.\n\n", info.NodeID)
	b.WriteString("Old configuration:\n")
	serverTable(&b, plan.Old, info.NodeID)
	b.WriteString("\nNew configuration:\n")
	serverTable(&b, plan.New, info.NodeID)
	b.WriteString("\nNodes that will be revoked:\n")
	if len(plan.Removed) == 0 {
		b.WriteString("  none\n")
	}
	for _, id := range plan.Removed {
		fmt.Fprintf(&b, "  %s\n", id)
	}
	b.WriteString(`
WARNING: recover is the last resort after a loss of quorum, and it can lose data.
  - Entries that were committed only on the lost nodes are lost for good.
  - Run it on ONE survivor only: the one with the highest last log term, and
    then the highest last log index. Compare "admin raft-info" on all survivors.
  - Every other survivor must be wiped and joined again as a new node with a
    new join token. Its old node ID stays revoked.

To apply the plan, run the same command with --yes while this node is stopped.
`)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeRecoveryDone(w io.Writer, plan cluster.RecoveryPlan, backupDir string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Recovered: node %s is the only voter now.\n", plan.Self.ID)
	fmt.Fprintf(&b, "Backup of raft/ and state.db: %s\n", backupDir)
	if len(plan.Removed) == 0 {
		b.WriteString("No other members to revoke.\n")
	} else {
		fmt.Fprintf(&b, "Revoked once this node leads again: %s\n", strings.Join(plan.Removed, ", "))
	}
	b.WriteString(`
Next steps:
  1. Start this node as before. Once it leads, it revokes the old members listed above.
  2. For every other survivor: stop it, move its DATA_DIR aside (do not delete it),
     and join it as a new node with a new join token issued by this cluster.
  3. Follow docs/betrieb/recovery.md for the checks after the recovery.
`)
	_, err := io.WriteString(w, b.String())
	return err
}

// serverTable writes servers as an aligned table; self marks this node.
func serverTable(b *strings.Builder, servers []cluster.RaftServer, self string) {
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  ID\tADDRESS\tROLE\t")
	for _, s := range servers {
		role := "non-voter"
		if s.Voter {
			role = "voter"
		}
		mark := ""
		if s.ID == self {
			mark = "(this node)"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.ID, s.Address, role, mark)
	}
	_ = tw.Flush()
}

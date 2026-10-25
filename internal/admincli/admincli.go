// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package admincli implements the "uptimecluster admin" subcommands for
// recovery tasks (FA-OBS-04, ADR 0019). They read the same environment as the
// server; none of them prints a secret or accepts one as an argument.
package admincli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/node"
)

// Exit codes (ADR 0019).
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is the process environment of a command, replaceable in tests.
type Env struct {
	Getenv func(string) string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// command is one subcommand. run returns an exit code; errors it returns are
// printed to stderr and end with ExitError.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, cfg config.Config, args []string, env Env) error
}

// errUsage marks a wrong invocation; it ends with ExitUsage.
var errUsage = errors.New("usage")

func commands() []command {
	return []command{
		{"raft-info", "show this node's Raft log position and configuration (node stopped)", runRaftInfo},
		{"recover", "make this node the only voter after quorum loss (node stopped)", runRecover},
		{"reset-password", "set a new password for a local user (node running)", runResetPassword},
	}
}

// Run executes "admin <command> [flags]" and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(env.Stdout)
		return ExitOK
	}
	var cmd *command
	for _, c := range commands() {
		if c.name == args[0] {
			cmd = &c
			break
		}
	}
	if cmd == nil {
		_, _ = fmt.Fprintf(env.Stderr, "unknown admin command %q\n\n", args[0])
		usage(env.Stderr)
		return ExitUsage
	}
	cfg, err := config.Load(env.Getenv)
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "invalid configuration:\n%v\n", err)
		return ExitUsage
	}
	if err := cmd.run(ctx, cfg, args[1:], env); err != nil {
		if errors.Is(err, errUsage) {
			_, _ = fmt.Fprintf(env.Stderr, "%v\n", err)
			return ExitUsage
		}
		_, _ = fmt.Fprintf(env.Stderr, "admin %s: %v\n", cmd.name, err)
		return ExitError
	}
	return ExitOK
}

func usage(w io.Writer) {
	var b strings.Builder
	b.WriteString("usage: uptimecluster admin <command> [flags]\n\ncommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(&b, "  %-15s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun \"uptimecluster admin <command> -h\" for the flags of a command.\nSee docs/betrieb/recovery.md before using recover.\n")
	_, _ = io.WriteString(w, b.String())
}

// nodeID reads the node ID of the data directory without creating one: an
// admin command on an empty directory is a mistake.
func nodeID(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, node.IDFile)) //nolint:gosec // G304: fixed name inside the configured data directory
	if err != nil {
		return "", fmt.Errorf("read node ID from %s (is DATA_DIR set correctly?): %w", dataDir, err)
	}
	id := strings.TrimSpace(string(raw))
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("node ID in %s is invalid: %w", dataDir, err)
	}
	return id, nil
}

// usageError builds an errUsage with a message.
func usageError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUsage, fmt.Sprintf(format, args...))
}

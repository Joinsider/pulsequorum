// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command loadtest drives load and measurement scenarios against a running
// UptimeCluster voter (NFA-PERF-01 to NFA-PERF-04). Subcommands:
//
//	targets  HTTP target farm whose endpoints the monitors check
//	setup    creates N HTTP monitors through the REST API
//	measure  measures API latency, status-change latency and resource usage
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var (
	errUsage = errors.New("usage error")
	errHelp  = errors.New("help requested")
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "loadtest:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}
	var err error
	switch args[0] {
	case "targets":
		err = runTargets(ctx, args[1:], stderr, log)
	case "setup":
		err = runSetup(ctx, args[1:], stderr, log)
	case "measure":
		err = runMeasure(ctx, args[1:], stdout, stderr, log)
	case "help", "-h", "--help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("%w: unknown subcommand %q", errUsage, args[0])
	}
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}

func usage(w io.Writer) {
	// A failed write to the usage output is not actionable; the exit code still reports the error.
	_, _ = fmt.Fprint(w, `usage: loadtest <subcommand> [flags]

  targets  -listen :18900 -count 1000 [-latency 0s]
  setup    -base URL (-setup-token T | -user U -password P) -count N -interval S
           -target-base URL [-prefix load-] [-workers 16]
  measure  -base URL -user U -password P [-duration 10m] [-targets-control URL]
           (-container NAME | -pid PID) [-out report.json]

Run "loadtest <subcommand> -h" for all flags.
`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("loadtest "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags maps flag.ErrHelp to errHelp so that run returns success.
func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return errHelp
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	return nil
}

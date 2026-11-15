// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command chaos runs container-based cluster scenarios (AP-2.9): it starts
// UptimeCluster voters with podman or docker, adds and removes nodes, injects
// network faults and checks the behaviour against the Pflichtenheft.
//
// Usage (from the repository root):
//
//	go run ./test/chaos/cmd/chaos -scenarios as02
//
// Scenarios register themselves in init functions of this package (see
// as02.go). The result is printed as Markdown on stdout and, with -report,
// written to a file. The exit status is 1 if any step fails.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/joinsider/uptimecluster/test/chaos/harness"
	"github.com/joinsider/uptimecluster/test/internal/fake"
)

// Default ports and timeouts.
const (
	defaultFakeListenBase = 18950
	scenarioTimeout       = 45 * time.Minute
	modulePath            = "module github.com/joinsider/uptimecluster"
)

// options holds the parsed command line.
type options struct {
	runtime        string
	prefix         string
	image          string
	build          string
	version        string
	voters         int
	portBase       int
	fakeListenBase int
	interval       time.Duration
	callbackHost   string
	scenarios      string
	keep           bool
	report         string
	list           bool
}

// parseOptions parses and validates the command line.
func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("chaos", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.runtime, "runtime", "", "container runtime: podman or docker (default: podman if installed, else docker)")
	fs.StringVar(&o.prefix, "prefix", "ucchaos", "name prefix of all containers, volumes and the network")
	fs.StringVar(&o.image, "image", harness.DefaultImage, "voter image")
	fs.StringVar(&o.build, "build", "auto", "build images from the repository: auto (if missing), always, never")
	fs.StringVar(&o.version, "version", "chaos", "VERSION build argument of the voter image")
	fs.IntVar(&o.voters, "voters", 3, "voter slots per scenario (1 to 5)")
	fs.IntVar(&o.portBase, "port-base", 18100, "voter i publishes its HTTP port on 127.0.0.1:port-base+i")
	fs.IntVar(&o.fakeListenBase, "fake-listen-base", defaultFakeListenBase, "host port of the fake ntfy server; the HTTP target uses the next port")
	fs.DurationVar(&o.interval, "interval", harness.DefaultConfig().Interval, "monitor interval used by the scenarios (20s to 24h)")
	fs.StringVar(&o.callbackHost, "callback-host", "", "host name under which containers reach this host (default per runtime)")
	fs.StringVar(&o.scenarios, "scenarios", "all", "comma-separated scenario IDs, or all")
	fs.BoolVar(&o.keep, "keep", false, "keep containers, volumes and the network after the run")
	fs.StringVar(&o.report, "report", "", "also write the Markdown report to this file")
	fs.BoolVar(&o.list, "list", false, "list the scenarios and exit")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if _, err := harness.ParseBuildPolicy(o.build); err != nil {
		return o, err
	}
	if o.fakeListenBase < 1024 || o.fakeListenBase > 65534 {
		return o, errors.New("-fake-listen-base must leave room for two ports in 1024..65534")
	}
	return o, nil
}

// realMain runs the driver and returns the process exit status.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "chaos:", err)
		return 2
	}
	if opts.list {
		for _, s := range registeredScenarios() {
			_, _ = fmt.Fprintf(stdout, "%-10s %s\n", s.ID, s.Title)
		}
		return 0
	}
	selected, err := selectScenarios(opts.scenarios)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "chaos:", err)
		return 2
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reports, err := run(ctx, opts, selected, log)
	if err != nil {
		log.Error("run aborted", "err", err)
		if len(reports) == 0 {
			return 1
		}
	}
	doc := harness.Document("UptimeCluster chaos run", reports...)
	_, _ = fmt.Fprint(stdout, doc)
	if opts.report != "" {
		if err := os.WriteFile(opts.report, []byte(doc), 0o600); err != nil { //nolint:gosec // G703: the report path is chosen by the user on the command line
			_, _ = fmt.Fprintln(stderr, "chaos: write report:", err)
			return 1
		}
		log.Info("report written", "path", opts.report)
	}
	for _, r := range reports {
		if r.Failed() {
			return 1
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

// run prepares the runtime, the images and the fake servers, then runs the
// selected scenarios one after another. Each scenario's clusters are torn
// down before the next one starts.
func run(ctx context.Context, opts options, selected []Scenario, log *slog.Logger) ([]*harness.Report, error) {
	rt, err := harness.NewRuntime(opts.runtime, log)
	if err != nil {
		return nil, err
	}
	if err := rt.Check(ctx); err != nil {
		return nil, err
	}
	log.Info("container runtime", "runtime", rt.Name)

	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	policy, err := harness.ParseBuildPolicy(opts.build)
	if err != nil {
		return nil, err
	}
	if err := ensureImages(ctx, rt, root, opts, policy, log); err != nil {
		return nil, err
	}

	cfg := harness.DefaultConfig()
	cfg.Runtime = rt.Name
	cfg.Prefix = opts.prefix
	cfg.Image = opts.image
	cfg.Voters = opts.voters
	cfg.HostPortBase = opts.portBase
	cfg.Interval = opts.interval
	cfg.Keep = opts.keep
	cfg.CallbackHost = opts.callbackHost
	if cfg.CallbackHost == "" {
		cfg.CallbackHost = harness.DefaultCallbackHost(rt.Name)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	ntfy := fake.NewRecorder()
	target := fake.NewTarget()
	ntfySrv, err := fake.Listen(fmt.Sprintf(":%d", opts.fakeListenBase), ntfy)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ntfySrv.Close() }()
	targetSrv, err := fake.Listen(fmt.Sprintf(":%d", opts.fakeListenBase+1), target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = targetSrv.Close() }()
	log.Info("fake servers listening", "ntfy", ntfySrv.Port(), "target", targetSrv.Port())

	var reports []*harness.Report
	for _, s := range selected {
		if ctx.Err() != nil {
			return reports, ctx.Err()
		}
		rep := harness.NewReport(s.reportLabel(), s.Title)
		env := &Env{
			Cfg:        cfg,
			Runtime:    rt,
			Log:        log.With("scenario", s.ID),
			Ntfy:       ntfy,
			Target:     target,
			NtfyPort:   ntfySrv.Port(),
			TargetPort: targetSrv.Port(),
			Rep:        rep,
		}
		sctx, cancel := context.WithTimeout(ctx, scenarioTimeout)
		runErr := s.Run(sctx, env)
		cancel()
		if runErr != nil {
			log.Error("scenario failed", "scenario", s.ID, "err", runErr)
			if !rep.Failed() {
				rep.Check("Scenario completed", "all steps run", runErr.Error(), false)
			}
		}
		// Teardown runs with a fresh context, so an aborted scenario is cleaned up too.
		if err := env.teardown(ctx); err != nil {
			log.Error("teardown incomplete", "scenario", s.ID, "err", err)
			rep.Info("Teardown", "all objects removed", err.Error())
		}
		reports = append(reports, rep)
	}
	return reports, nil
}

// ensureImages builds the voter image and the sidecar image if the policy
// asks for it. Both builds use absolute paths, because podman and docker
// resolve -f relative to the working directory.
func ensureImages(ctx context.Context, rt *harness.Runtime, root string, opts options, policy harness.BuildPolicy, log *slog.Logger) error {
	if _, err := harness.EnsureImage(ctx, rt, harness.BuildSpec{
		Tag:       opts.image,
		File:      filepath.Join(root, "Dockerfile"),
		Context:   root,
		BuildArgs: map[string]string{"VERSION": opts.version},
	}, policy, log); err != nil {
		return err
	}
	netemDir := filepath.Join(root, "test", "chaos", "netem")
	if _, err := harness.EnsureImage(ctx, rt, harness.BuildSpec{
		Tag:     harness.DefaultNetemImage,
		File:    filepath.Join(netemDir, "Containerfile"),
		Context: netemDir,
	}, policy, log); err != nil {
		return err
	}
	return nil
}

// findRepoRoot walks up from the working directory to the module root.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: fixed name in a directory of the working tree
		if err == nil && strings.Contains(string(data), modulePath+"\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("run the chaos driver inside the UptimeCluster repository")
		}
		dir = parent
	}
}

// selectScenarios resolves the -scenarios value. "all" selects every
// registered scenario in ID order.
func selectScenarios(spec string) ([]Scenario, error) {
	all := registeredScenarios()
	if strings.TrimSpace(spec) == "all" {
		if len(all) == 0 {
			return nil, errors.New("no scenarios registered")
		}
		return all, nil
	}
	var out []Scenario
	seen := make(map[string]bool)
	for _, raw := range strings.Split(spec, ",") {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		s, ok := registry[id]
		if !ok {
			return nil, fmt.Errorf("unknown scenario %q (see -list)", id)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("-scenarios selects nothing")
	}
	return out, nil
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

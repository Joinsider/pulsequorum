// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"time"
)

var uptimeRanges = []string{"24h", "7d", "30d", "365d"}

// recorder collects all samples of one measure run.
type recorder struct {
	mu                sync.Mutex
	api               []apiSample
	status            []statusSample
	resources         []resourceSample
	resErrs           []string
	resErrN           int
	flipped           map[string]bool // monitors the status probe changed
	stateEvents       []stateEvent    // all state events of the stream
	streamConnections int
	streamReconnects  int
}

func (r *recorder) markFlipped(monitorID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flipped == nil {
		r.flipped = make(map[string]bool)
	}
	r.flipped[monitorID] = true
}

func (r *recorder) addAPI(s apiSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.api = append(r.api, s)
}

func (r *recorder) addStatus(s statusSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = append(r.status, s)
}

func (r *recorder) addResource(s resourceSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = append(r.resources, s)
}

func (r *recorder) addResourceError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resErrN++
	if len(r.resErrs) < 3 {
		r.resErrs = append(r.resErrs, err.Error())
	}
}

type measureOptions struct {
	base           string
	user           string
	duration       time.Duration
	targetsControl string
	container      string
	pid            int
	out            string
	apiInterval    time.Duration
	resInterval    time.Duration
	statusInterval time.Duration
	statusTimeout  time.Duration
}

func runMeasure(ctx context.Context, args []string, stdout, stderr io.Writer, log *slog.Logger) error {
	fs := newFlagSet("measure", stderr)
	base := fs.String("base", "http://127.0.0.1:8080", "voter base URL")
	user := fs.String("user", "", "username")
	password := fs.String("password", "", "password")
	opts := measureOptions{}
	fs.DurationVar(&opts.duration, "duration", 10*time.Minute, "measurement duration")
	fs.StringVar(&opts.targetsControl, "targets-control", "http://127.0.0.1:18900", "target farm base URL (control endpoint)")
	fs.StringVar(&opts.container, "container", "", "container name for docker stats (podman shim supported)")
	fs.IntVar(&opts.pid, "pid", 0, "process ID for ps (alternative to -container)")
	fs.StringVar(&opts.out, "out", "report.json", "JSON report path (empty = none)")
	fs.DurationVar(&opts.apiInterval, "api-interval", 5*time.Second, "API sampling interval")
	fs.DurationVar(&opts.resInterval, "resource-interval", 5*time.Second, "resource sampling interval")
	fs.DurationVar(&opts.statusInterval, "status-interval", 60*time.Second, "status cycle interval")
	fs.DurationVar(&opts.statusTimeout, "status-timeout", 150*time.Second, "max wait for one state event")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *user == "" || *password == "" {
		return fmt.Errorf("%w: -user and -password are required", errUsage)
	}
	if opts.duration <= 0 || opts.apiInterval <= 0 || opts.resInterval <= 0 || opts.statusInterval <= 0 || opts.statusTimeout <= 0 {
		return fmt.Errorf("%w: durations and intervals must be positive", errUsage)
	}
	if opts.container != "" && opts.pid != 0 {
		return fmt.Errorf("%w: use either -container or -pid", errUsage)
	}
	opts.base = *base
	opts.user = *user

	client, err := newAPIClient(*base, *user, *password)
	if err != nil {
		return err
	}
	if err := client.login(ctx); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	initial, err := client.listMonitors(ctx)
	if err != nil {
		return fmt.Errorf("list monitors: %w", err)
	}
	if len(initial) == 0 {
		return errors.New("no monitors found; run setup first")
	}

	rec := &recorder{}
	startedAt := time.Now()
	log.Info("measure started", "monitors", len(initial), "duration", opts.duration)
	measureLoops(ctx, opts, client, initial, rec, log)
	endedAt := time.Now()
	log.Info("measure finished", "elapsed", endedAt.Sub(startedAt).Round(time.Second))

	rep := buildReport(opts, initial, rec, startedAt, endedAt)
	if opts.out != "" {
		if err := writeJSON(opts.out, rep); err != nil {
			return err
		}
		log.Info("JSON report written", "path", opts.out)
	}
	_, err = io.WriteString(stdout, renderMarkdown(rep))
	return err
}

// measureLoops runs the API sampler, the resource sampler and the status
// probe for opts.duration. The status probe gets a grace period after the
// duration to finish cycles that are in progress.
func measureLoops(ctx context.Context, opts measureOptions, client *apiClient, monitors []monitorRef, rec *recorder, log *slog.Logger) {
	runCtx, stopRun := context.WithTimeout(ctx, opts.duration)
	defer stopRun()

	// The stream outlives the probe cycles so that they can finish.
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	stream := newStreamListener(client, log)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		stream.run(streamCtx)
	}()

	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		runAPISampler(runCtx, client, opts.apiInterval, rec, log)
	}()
	go func() {
		defer loops.Done()
		runResourceSampler(runCtx, opts, rec, log)
	}()

	var targets []monitorRef
	for _, m := range monitors {
		if m.TargetIndex >= 0 {
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		log.Warn("no monitor uses the target farm URL, status latency is not measured")
	} else {
		loops.Add(1)
		go func() {
			defer loops.Done()
			if err := stream.waitReady(runCtx, 30*time.Second); err != nil {
				log.Warn("stream not ready, first status cycles may time out", "err", err)
			}
			probe := newStatusProbe(client, stream, opts.targetsControl, opts.statusTimeout, targets, rec, log)
			grace := opts.statusTimeout + 30*time.Second
			probe.schedule(runCtx, ctx, opts.statusInterval, grace)
		}()
	}

	loops.Wait()
	cancelStream()
	<-streamDone
	rec.mu.Lock()
	rec.streamConnections = stream.connectionCount()
	rec.streamReconnects = stream.reconnectCount()
	rec.stateEvents = stream.snapshot()
	rec.mu.Unlock()
}

// runAPISampler times the full monitor listing and one uptime request for a
// random monitor every interval.
func runAPISampler(ctx context.Context, client *apiClient, interval time.Duration, rec *recorder, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		apiRound(ctx, client, rec, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func apiRound(ctx context.Context, client *apiClient, rec *recorder, log *slog.Logger) {
	start := time.Now()
	mons, err := client.listMonitors(ctx)
	dur := time.Since(start)
	if ctx.Err() != nil {
		return
	}
	s := apiSample{Group: "monitors_list", At: start, DurationMs: ms(dur), Count: len(mons)}
	if err != nil {
		s.Error = err.Error()
		log.Warn("monitor listing failed", "err", err)
	}
	rec.addAPI(s)
	if err != nil || len(mons) == 0 {
		return
	}

	m := mons[rand.IntN(len(mons))]
	rng := uptimeRanges[rand.IntN(len(uptimeRanges))]
	start = time.Now()
	err = client.uptime(ctx, m.ID, rng)
	dur = time.Since(start)
	if ctx.Err() != nil {
		return
	}
	u := apiSample{Group: "uptime", At: start, DurationMs: ms(dur), Range: rng}
	if err != nil {
		u.Error = err.Error()
	}
	rec.addAPI(u)
}

// runResourceSampler reads CPU and memory of the voter if -container or -pid is set.
func runResourceSampler(ctx context.Context, opts measureOptions, rec *recorder, log *slog.Logger) {
	var sample sampleFunc
	switch {
	case opts.container != "":
		sample = dockerSampler(opts.container)
	case opts.pid != 0:
		sample = psSampler(opts.pid)
	default:
		log.Warn("neither -container nor -pid given, resource usage is not measured")
		return
	}
	ticker := time.NewTicker(opts.resInterval)
	defer ticker.Stop()
	for {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		s, err := sample(sctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			rec.addResourceError(err)
			log.Warn("resource sample failed", "err", err)
		} else {
			rec.addResource(s)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

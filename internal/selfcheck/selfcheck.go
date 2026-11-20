// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package selfcheck probes the reference targets of a node to tell whether the
// node itself has lost its connectivity (ADR 0022, FA-CON-04). A round counts
// as failed only when every target fails; a single reachable target proves
// that egress works.
package selfcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Defaults for Options fields that are left at their zero value (ADR 0022).
const (
	DefaultInterval = 30 * time.Second
	DefaultTimeout  = 5 * time.Second
)

// Target is one parsed reference target.
type Target struct {
	// Label is the log-safe form: host:port for TCP, scheme://host[:port] for
	// HTTP. It never contains a path or query.
	Label string
	// Addr is host:port of a TCP target; empty for HTTP targets.
	Addr string
	// URL is the http(s) URL that is sent a HEAD request; empty for TCP targets.
	URL string
}

// ParseTargets parses SELF_CHECK_TARGETS entries. Each entry is either
// host:port (TCP connect) or an http(s) URL (HTTP HEAD). Blank entries are
// skipped. All invalid entries are reported together; each error names its entry.
func ParseTargets(entries []string) ([]Target, error) {
	var targets []Target
	var errs []error
	for _, raw := range entries {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		t, err := parseTarget(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", raw, err))
			continue
		}
		targets = append(targets, t)
	}
	return targets, errors.Join(errs...)
}

func parseTarget(raw string) (Target, error) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return Target{}, errors.New("must be host:port or an http(s) URL")
		}
		if u.Host == "" || u.User != nil || u.Fragment != "" {
			return Target{}, errors.New("URL needs a host and must not carry user info or a fragment")
		}
		return Target{Label: u.Scheme + "://" + u.Host, URL: raw}, nil
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return Target{}, errors.New("must be host:port or an http(s) URL")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return Target{}, errors.New("port must be a number from 1 to 65535")
	}
	return Target{Label: raw, Addr: raw}, nil
}

// Options configures a Prober. Dialer is required: it is the SSRF-aware
// dialer of the checkers, so self-checks obey the same address policy.
type Options struct {
	Targets  []Target
	Dialer   *checker.Dialer
	Interval time.Duration // default DefaultInterval
	Timeout  time.Duration // per target, default DefaultTimeout
	Now      func() time.Time
	Logger   *slog.Logger
}

// Status is the result of the latest completed round.
type Status struct {
	// Configured is true when at least one target is configured.
	Configured bool
	// Failed is true when all targets failed in the latest round.
	Failed bool
	// AtMs is the Unix time in milliseconds of the latest completed round; 0
	// before the first round.
	AtMs int64
}

// Prober runs the reference targets periodically. Status may be called
// concurrently with Run.
type Prober struct {
	targets  []Target
	dialer   *checker.Dialer
	client   *http.Client
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	log      *slog.Logger

	mu     sync.RWMutex
	failed bool
	atMs   int64
}

// New validates opts and returns a Prober. It does not start any probing.
func New(opts Options) (*Prober, error) {
	if opts.Dialer == nil {
		return nil, errors.New("selfcheck: Dialer is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	p := &Prober{
		targets:  slices.Clone(opts.Targets),
		dialer:   opts.Dialer,
		interval: opts.Interval,
		timeout:  opts.Timeout,
		now:      opts.Now,
		log:      opts.Logger,
	}
	p.client = &http.Client{
		Transport: &http.Transport{
			// Proxy stays nil: HTTP_PROXY is ignored, as for the checkers (FA-CHK-13).
			DialContext:       opts.Dialer.DialContext,
			DisableKeepAlives: true,
		},
		// A redirect is a response, so the first answer decides; redirects are
		// not followed, which keeps every hop out of the probe.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return p, nil
}

// Status returns the state of the latest completed round.
func (p *Prober) Status() Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Status{Configured: len(p.targets) > 0, Failed: p.failed, AtMs: p.atMs}
}

// Run probes immediately and then every Interval until ctx is done. It returns
// at once when no targets are configured.
func (p *Prober) Run(ctx context.Context) {
	if len(p.targets) == 0 {
		return
	}
	for {
		p.round(ctx)
		timer := time.NewTimer(p.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// round probes all targets in parallel and stores the result. A round that is
// interrupted by ctx cancellation leaves the previous status untouched.
func (p *Prober) round(ctx context.Context) {
	ok := make([]bool, len(p.targets))
	var wg sync.WaitGroup
	for i, t := range p.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok[i] = p.probe(ctx, t) == nil
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	var failed []string
	for i, good := range ok {
		if !good {
			failed = append(failed, p.targets[i].Label)
		}
	}
	allFailed := len(failed) == len(p.targets)

	p.mu.Lock()
	prev := p.failed
	p.failed = allFailed
	p.atMs = p.now().UnixMilli()
	p.mu.Unlock()

	// Before the first round the state counts as ok, so a first failed round is
	// logged as a change as well.
	switch {
	case allFailed && !prev:
		p.log.Info("node self-check failed: all reference targets unreachable", "targets", failed)
	case !allFailed && prev:
		p.log.Info("node self-check recovered: a reference target is reachable again")
	}
}

// probe checks one target. Errors are returned for the caller to count only;
// their text is not logged, because URL errors can contain the query.
func (p *Prober) probe(ctx context.Context, t Target) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if t.URL == "" {
		conn, err := p.dialer.DialContext(ctx, "tcp", t.Addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, t.URL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Remote is the self-check state of a peer as the leader last received it
// through GetStatus (ADR 0022). Ages are kept apart from the clocks: the peer
// reports its own wall clock, so its round age does not depend on the skew.
type Remote struct {
	Status Status
	// ReceivedAt is the local time at which the answer arrived.
	ReceivedAt time.Time
	// Age is how old the round was when the peer answered, on the peer's clock.
	Age time.Duration
}

// AgeAt returns the age of the round at now (local clock): the age at the
// answer plus the time since then.
func (r Remote) AgeAt(now time.Time) time.Duration {
	return r.Age + now.Sub(r.ReceivedAt)
}

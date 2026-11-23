// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command as01 automates acceptance scenario AS-01 (docs/pflichtenheft.md,
// section 13.2) against a running single-node Pulsequorum.
//
// The node is started separately (Compose or binary). The driver starts an
// in-process fake ntfy server and an HTTP target, sets up the admin, creates
// a shoutrrr notification channel and an HTTP monitor, switches the target
// to failing, and checks the DOWN notification timing, the open incident and
// the 24 h uptime. Then it switches the target back and checks the recovery.
//
// Usage:
//
//	as01 -base http://127.0.0.1:8080 -setup-token TOKEN -callback-host host.containers.internal
//
// The result is printed as a Markdown table on stdout; the exit status is 1
// if any step failed. Secrets (access token, passwords) are never printed.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joinsider/pulsequorum/test/internal/fake"
	"github.com/joinsider/pulsequorum/test/internal/ucapi"
)

// Limits accepted by the node's API (api/openapi.yaml, MonitorInput).
const (
	minIntervalSeconds = 20
	maxIntervalSeconds = 86400
	maxRetries         = 10

	stepAdminSetUp           = "Admin set up"
	stepDownNotificationNtfy = "DOWN notification in ntfy"
	stepOpenIncidentExists   = "Open incident exists"
	expectAtLeastOneIncident = "at least 1 open incident"
	stepUptimeBelow100       = "Uptime 24 h below 100 %"
	expectBelow100Percent    = "< 100 %"
)

// options holds the parsed command line.
type options struct {
	base         string
	setupToken   string
	user         string
	password     string
	listen       string
	targetListen string
	callbackHost string
	interval     time.Duration
	retries      int
}

// parseOptions parses and validates the command line.
func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	var intervalSeconds int
	fs := flag.NewFlagSet("as01", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.base, "base", "http://127.0.0.1:8080", "base URL of the node")
	fs.StringVar(&o.setupToken, "setup-token", "", "one-time setup token from the node log (fresh node)")
	fs.StringVar(&o.user, "user", "admin", "admin username")
	fs.StringVar(&o.password, "password", "", "admin password; generated for a fresh node if empty")
	fs.StringVar(&o.listen, "listen", ":18950", "listen address of the fake ntfy server")
	fs.StringVar(&o.targetListen, "target-listen", ":18951", "listen address of the HTTP target")
	fs.StringVar(&o.callbackHost, "callback-host", "host.containers.internal",
		"host name under which the node reaches this driver (ntfy and target)")
	fs.IntVar(&intervalSeconds, "interval", 20, "monitor interval in seconds (20 to 86400)")
	fs.IntVar(&o.retries, "retries", 1, "monitor retries (0 to 10)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	u, err := url.Parse(o.base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return o, fmt.Errorf("-base must be an absolute http(s) URL, got %q", o.base)
	}
	if intervalSeconds < minIntervalSeconds || intervalSeconds > maxIntervalSeconds {
		return o, fmt.Errorf("-interval must be between %d and %d seconds", minIntervalSeconds, maxIntervalSeconds)
	}
	if o.retries < 0 || o.retries > maxRetries {
		return o, fmt.Errorf("-retries must be between 0 and %d", maxRetries)
	}
	if strings.TrimSpace(o.callbackHost) == "" {
		return o, errors.New("-callback-host must not be empty")
	}
	if o.user == "" {
		return o, errors.New("-user must not be empty")
	}
	if o.setupToken == "" && o.password == "" {
		return o, errors.New("need -setup-token (fresh node) or -password (existing admin)")
	}
	o.interval = time.Duration(intervalSeconds) * time.Second
	return o, nil
}

// realMain runs the driver and returns the process exit status.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "as01:", err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep := &report{}
	s := newScenario(opts, rep, logger)
	runErr := s.run(ctx)
	if runErr != nil {
		logger.Error("scenario stopped", "err", runErr)
	}
	_, _ = fmt.Fprint(stdout, rep.Markdown())
	if runErr != nil || rep.Failed() {
		return 1
	}
	return 0
}

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// scenario holds the state of one AS-01 run.
type scenario struct {
	opts       options
	rep        *report
	log        *slog.Logger
	ntfy       *fake.Recorder
	target     *fake.Target
	api        *ucapi.Client
	topic      string
	ntfyURL    string
	targetURL  string
	channelID  string
	monitorID  string
	monitorNm  string
	incidentID string
	// generatedPassword is set when the driver generated the admin password.
	generatedPassword bool
	servers           []*http.Server
}

func newScenario(opts options, rep *report, log *slog.Logger) *scenario {
	s := &scenario{
		opts:   opts,
		rep:    rep,
		log:    log,
		ntfy:   fake.NewRecorder(),
		target: fake.NewTarget(),
		api:    ucapi.New(opts.base, opts.user, opts.password),
	}
	return s
}

// fail records a FAIL row and returns err for the caller to stop with.
func (s *scenario) fail(name, expected, measured string, err error) error {
	s.rep.Add(name, expected, measured, false)
	return fmt.Errorf("%s: %w", name, err)
}

// run executes the scenario. Steps after a fatal failure are not run.
func (s *scenario) run(ctx context.Context) error {
	defer s.shutdown()

	if err := s.startServers(); err != nil {
		return s.fail("Start fake ntfy and target", "both listeners bound", err.Error(), err)
	}
	ntfyPort := s.listenerPort(0)
	targetPort := s.listenerPort(1)
	s.rep.Add("Start fake ntfy and target", "both listeners bound",
		fmt.Sprintf("ntfy :%d, target :%d", ntfyPort, targetPort), true)

	if _, err := s.api.Health(ctx); err != nil {
		return s.fail("Node reachable", "GET /health answers 200", err.Error(), err)
	}
	s.rep.Add("Node reachable", "GET /health answers 200", "200", true)

	if err := s.setupAdmin(ctx); err != nil {
		return err
	}

	suffix, err := randomHex(4)
	if err != nil {
		return err
	}
	s.topic = "as01-" + suffix
	s.ntfyURL = fmt.Sprintf("ntfy://%s/%s?scheme=http", net.JoinHostPort(s.opts.callbackHost, strconv.Itoa(ntfyPort)), s.topic)
	s.targetURL = "http://" + net.JoinHostPort(s.opts.callbackHost, strconv.Itoa(targetPort)) + "/"

	// Notification channel: create, then a synchronous test delivery.
	ch, err := s.api.CreateChannel(ctx, "as01-ntfy-"+suffix, s.ntfyURL, []string{"down", "up"})
	if err != nil {
		return s.fail("Notification channel created", "201, kind shoutrrr, default", err.Error(), err)
	}
	s.channelID = ch.ID
	s.rep.Add("Notification channel created", "201, kind shoutrrr, isDefault", "channel "+ch.ID, true)

	res, err := s.api.TestChannel(ctx, s.channelID)
	if err != nil || !res.Delivered {
		msg := res.Error
		if err != nil {
			msg = err.Error()
		}
		return s.fail("Channel test delivery", "delivered to ntfy", msg, errors.New("test delivery failed"))
	}
	s.rep.Add("Channel test delivery", "delivered to ntfy", fmt.Sprintf("delivered in %d ms", res.DurationMs), true)

	// HTTP monitor against the target.
	s.monitorNm = "as01-http-" + suffix
	mon, err := s.api.CreateMonitor(ctx, s.monitorNm, s.targetURL, int(s.opts.interval/time.Second), s.opts.retries, []string{s.channelID})
	if err != nil {
		return s.fail("HTTP monitor created", "201", err.Error(), err)
	}
	s.monitorID = mon.ID
	s.rep.Add("HTTP monitor created", "201, type http, retries and interval as given",
		fmt.Sprintf("monitor %s, interval %s, retries %d", mon.ID, s.opts.interval, s.opts.retries), true)

	if err := s.waitUp(ctx); err != nil {
		return err
	}
	if err := s.checkDown(ctx); err != nil {
		return err
	}
	if err := s.checkRecovery(ctx); err != nil {
		return err
	}
	return nil
}

// startServers binds the fake ntfy and the target listener.
func (s *scenario) startServers() error {
	ntfyLn, err := net.Listen("tcp", s.opts.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.listen, err)
	}
	targetLn, err := net.Listen("tcp", s.opts.targetListen)
	if err != nil {
		_ = ntfyLn.Close()
		return fmt.Errorf("listen %s: %w", s.opts.targetListen, err)
	}
	s.servers = append(s.servers, serve(ntfyLn, s.ntfy), serve(targetLn, s.target))
	s.log.Info("fake ntfy and target listening", "ntfy", ntfyLn.Addr().String(), "target", targetLn.Addr().String())
	return nil
}

// listenerPort returns the port of the i-th started server.
func (s *scenario) listenerPort(i int) int {
	if i >= len(s.servers) {
		return 0
	}
	addr := s.servers[i].Addr
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

// shutdown stops the in-process servers.
func (s *scenario) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range s.servers {
		_ = srv.Shutdown(ctx)
	}
}

// serve starts an HTTP server on ln in the background.
func serve(ln net.Listener, h http.Handler) *http.Server {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	srv.Addr = ln.Addr().String()
	go func() { _ = srv.Serve(ln) }()
	return srv
}

// setupAdmin creates the first admin with the setup token, or logs in when
// the node is already set up.
func (s *scenario) setupAdmin(ctx context.Context) error {
	if s.opts.setupToken != "" {
		if s.opts.password == "" {
			pw, err := randomHex(16)
			if err != nil {
				return err
			}
			s.api.Password = pw
			s.generatedPassword = true
		}
		err := s.api.Setup(ctx, s.opts.setupToken, "AS-01 acceptance")
		if err == nil {
			s.rep.Add(stepAdminSetUp, "201, admin logged in", fmt.Sprintf("user %q created", s.opts.user), true)
			return nil
		}
		var ae *ucapi.APIError
		if !errors.As(err, &ae) || (ae.Status != http.StatusForbidden && ae.Status != http.StatusConflict) {
			return s.fail(stepAdminSetUp, "201, admin logged in", err.Error(), err)
		}
		s.log.Info("setup not possible, trying login", "status", ae.Status)
		s.api.Token = ""
	}
	if s.generatedPassword {
		err := errors.New("node already has an admin; pass its password with -password")
		return s.fail(stepAdminSetUp, "201 or login", err.Error(), err)
	}
	if err := s.api.Login(ctx); err != nil {
		return s.fail("Admin logged in", "200", err.Error(), err)
	}
	s.rep.Add("Admin logged in", "200", fmt.Sprintf("user %q", s.opts.user), true)
	return nil
}

// waitUp waits until the monitor reports UP (before the target is switched).
func (s *scenario) waitUp(ctx context.Context) error {
	limit := upDeadline(s.opts.interval)
	start := time.Now()
	var status string
	ok, lastErr := poll(ctx, limit, time.Second, func(c context.Context) (bool, error) {
		m, err := s.api.GetMonitor(c, s.monitorID)
		if err != nil {
			return false, err
		}
		status = m.Status
		return m.Status == "UP", nil
	})
	measured := fmt.Sprintf("status %s after %s", status, time.Since(start).Round(time.Second))
	if !ok {
		if lastErr != nil {
			measured += " (last error: " + lastErr.Error() + ")"
		}
		return s.fail("Monitor UP before failure", "UP within "+limit.String(), measured, errors.New("monitor not UP"))
	}
	s.rep.Add("Monitor UP before failure", "UP within "+limit.String(), measured, true)
	return nil
}

// checkDown switches the target to failing and checks the DOWN notification,
// the monitor status, the open incident and the uptime.
func (s *scenario) checkDown(ctx context.Context) error {
	limit, err := downDeadline(s.opts.interval, s.opts.retries)
	if err != nil {
		return err
	}
	s.target.SetFailing(true)
	t0 := time.Now()
	s.rep.Add("Target switched to 503", "target answers 503",
		"t0 = "+t0.UTC().Format(time.RFC3339Nano), true)

	waitCtx, cancel := context.WithTimeout(ctx, limit+time.Minute)
	defer cancel()
	msg, ok := s.ntfy.WaitFor(waitCtx, t0, s.matchEvent("DOWN"))
	expected := fmt.Sprintf("DOWN notification within %s = (retries+1) x interval + 30 s", limit)
	if !ok {
		return s.fail(stepDownNotificationNtfy, expected,
			fmt.Sprintf("no DOWN notification within %s", limit+time.Minute),
			errors.New("no DOWN notification"))
	}
	elapsed := msg.At.Sub(t0)
	measured := fmt.Sprintf("after %s", elapsed.Round(100*time.Millisecond))
	if elapsed > limit {
		return s.fail(stepDownNotificationNtfy, expected, measured,
			fmt.Errorf("DOWN after %s, limit %s", elapsed, limit))
	}
	s.rep.Add(stepDownNotificationNtfy, expected, measured, true)

	st, err := s.api.GetMonitor(ctx, s.monitorID)
	if err != nil {
		return s.fail("Monitor status DOWN", "DOWN", err.Error(), err)
	}
	s.rep.Add("Monitor status DOWN", "DOWN", st.Status, st.Status == "DOWN")

	incidents, err := s.openIncidents(ctx)
	if err != nil {
		return s.fail(stepOpenIncidentExists, expectAtLeastOneIncident, err.Error(), err)
	}
	if len(incidents) == 0 {
		return s.fail(stepOpenIncidentExists, expectAtLeastOneIncident, "0 open incidents",
			errors.New("no open incident"))
	}
	s.rep.Add(stepOpenIncidentExists, expectAtLeastOneIncident,
		fmt.Sprintf("%d open, cause %q", len(incidents), incidents[0].Cause), true)
	s.incidentID = incidents[0].ID

	uptime, err := s.waitUptimeBelow100(ctx)
	if err != nil {
		return s.fail(stepUptimeBelow100, expectBelow100Percent, err.Error(), err)
	}
	s.rep.Add(stepUptimeBelow100, expectBelow100Percent, fmt.Sprintf("%.2f %%", uptime), uptime < 100)
	if uptime >= 100 {
		return s.fail(stepUptimeBelow100, expectBelow100Percent, fmt.Sprintf("%.2f %%", uptime),
			errors.New("uptime is 100 %"))
	}
	return nil
}

// openIncidents lists open incidents of the monitor, polling briefly for the
// incident that is created together with the DOWN decision.
func (s *scenario) openIncidents(ctx context.Context) ([]ucapi.Incident, error) {
	var found []ucapi.Incident
	_, lastErr := poll(ctx, 30*time.Second, time.Second, func(c context.Context) (bool, error) {
		items, err := s.api.OpenIncidents(c, s.monitorID)
		if err != nil {
			return false, err
		}
		found = items
		return len(items) > 0, nil
	})
	if len(found) == 0 {
		return nil, lastErr
	}
	return found, nil
}

// waitUptimeBelow100 polls the 24 h uptime until it is known and below 100 %,
// because the rollups lag behind the checks.
func (s *scenario) waitUptimeBelow100(ctx context.Context) (float64, error) {
	var last *float64
	var lastErr error
	ok, _ := poll(ctx, 90*time.Second, 2*time.Second, func(c context.Context) (bool, error) {
		v, err := s.api.Uptime24h(c, s.monitorID)
		if err != nil {
			lastErr = err
			return false, err
		}
		last = v
		return v != nil && *v < 100, nil
	})
	if ok {
		return *last, nil
	}
	if last == nil {
		if lastErr != nil {
			return 0, fmt.Errorf("no uptime value: %w", lastErr)
		}
		return 0, errors.New("no uptime value")
	}
	return *last, nil
}

// checkRecovery switches the target back and checks the UP notification
// (informational) and the resolved incident.
func (s *scenario) checkRecovery(ctx context.Context) error {
	s.target.SetFailing(false)
	tUp := time.Now()
	s.rep.Add("Target switched back to 200", "target answers 200", "t1 = "+tUp.UTC().Format(time.RFC3339Nano), true)

	limit := recoveryDeadline(s.opts.interval)
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	msg, ok := s.ntfy.WaitFor(waitCtx, tUp, s.matchEvent("UP"))
	cancel()
	if ok {
		s.rep.AddInfo("UP notification in ntfy", "UP notification (informational)",
			fmt.Sprintf("after %s", msg.At.Sub(tUp).Round(100*time.Millisecond)))
	} else {
		s.rep.AddInfo("UP notification in ntfy", "UP notification (informational)",
			fmt.Sprintf("not received within %s", limit))
	}

	var inc ucapi.Incident
	var lastErr error
	ok, _ = poll(ctx, limit, time.Second, func(c context.Context) (bool, error) {
		i, err := s.api.GetIncident(c, s.incidentID)
		if err != nil {
			lastErr = err
			return false, err
		}
		inc = i
		return i.ResolvedAt != nil, nil
	})
	expected := "incident resolved (resolvedAt set)"
	if !ok || inc.ResolvedAt == nil {
		measured := "not resolved"
		if lastErr != nil {
			measured += " (last error: " + lastErr.Error() + ")"
		}
		return s.fail("Incident resolved", expected, measured, errors.New("incident not resolved"))
	}
	s.rep.Add("Incident resolved", expected,
		fmt.Sprintf("resolved %s after recovery", inc.ResolvedAt.Sub(tUp).Round(time.Second)), true)
	return nil
}

// matchEvent matches a recorded ntfy message for this run's topic and
// monitor. Shoutrrr sends the Text() body, which starts with "DOWN: <name>"
// or "UP: <name>".
func (s *scenario) matchEvent(label string) func(fake.Message) bool {
	wantPath := "/" + s.topic
	needle := label + ": " + s.monitorNm
	return func(m fake.Message) bool {
		return m.Path == wantPath && strings.Contains(m.Body, needle)
	}
}

// poll calls fn every `every` until it reports done, the timeout elapses or
// ctx ends. Errors of fn are not fatal: the last one is returned for context.
func poll(ctx context.Context, timeout, every time.Duration, fn func(context.Context) (bool, error)) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var lastErr error
	for {
		done, err := fn(ctx)
		if err != nil {
			lastErr = err
		} else if done {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, lastErr
		case <-ticker.C:
		}
	}
}

// randomHex returns n random bytes encoded as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

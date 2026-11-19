// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Command as06 automates acceptance scenario AS-06 (docs/pflichtenheft.md,
// section 13.2) against a running UptimeCluster node that has two tenants
// signed in through OIDC.
//
// Tenant A creates a notification channel, an HTTP monitor, a maintenance
// window and an API token. Tenant B then tries to read and modify these
// objects by their IDs and must get 404 (the bulk action may also answer 422).
// Its lists, its audit log and its live stream must not show any of them.
// Afterwards tenant A checks that nothing changed, triggers events on its
// monitor and removes its objects again.
//
// Usage:
//
//	as06 -base http://127.0.0.1:8080 -token-a-file PATH -token-b-file PATH
//
// The token files hold one bearer access token each (tenant-admin of two
// different tenants). The tokens are read, never printed or logged.
//
// The result is printed as a Markdown table on stdout; the exit status is 1
// if any step failed. Secrets are scrubbed from everything the driver prints.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// Fixed parameters of the scenario. The interval is the minimum that keeps
// the monitor's checks rare; the target is a public host that the node must
// reach for the monitor to answer UP.
const (
	monitorURL      = "https://target.as06.invalid/"
	channelURL      = "https://target.as06.invalid/hook"
	monitorInterval = 60
	minEventWait    = 10
	maxEventWait    = 600
	// eventGrace lets a leak of tenant A's events reach tenant B's stream
	// after A has seen its own event.
	eventGrace = 5 * time.Second
	// cleanupTimeout bounds the removal of the objects of tenant A.
	cleanupTimeout = 2 * time.Minute
)

// options holds the parsed command line. The tokens are read from the files.
type options struct {
	base      string
	tokenA    string
	tokenB    string
	eventWait time.Duration
}

// parseOptions parses and validates the command line and reads the token files.
func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	var tokenAFile, tokenBFile string
	var eventWaitSeconds int
	fs := flag.NewFlagSet("as06", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&o.base, "base", "http://127.0.0.1:8080", "base URL of the node")
	fs.StringVar(&tokenAFile, "token-a-file", "", "file with the bearer token of the tenant-admin of tenant A")
	fs.StringVar(&tokenBFile, "token-b-file", "", "file with the bearer token of the tenant-admin of tenant B")
	fs.IntVar(&eventWaitSeconds, "event-wait", 90,
		fmt.Sprintf("seconds to wait for an event of A's monitor on A's stream (%d to %d)", minEventWait, maxEventWait))
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
	if tokenAFile == "" {
		return o, errors.New("-token-a-file is required")
	}
	if tokenBFile == "" {
		return o, errors.New("-token-b-file is required")
	}
	if eventWaitSeconds < minEventWait || eventWaitSeconds > maxEventWait {
		return o, fmt.Errorf("-event-wait must be between %d and %d seconds", minEventWait, maxEventWait)
	}
	o.eventWait = time.Duration(eventWaitSeconds) * time.Second
	if o.tokenA, err = readToken(tokenAFile); err != nil {
		return o, fmt.Errorf("-token-a-file: %w", err)
	}
	if o.tokenB, err = readToken(tokenBFile); err != nil {
		return o, fmt.Errorf("-token-b-file: %w", err)
	}
	return o, nil
}

// readToken returns the trimmed content of a token file.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("token file is empty")
	}
	return tok, nil
}

// realMain runs the driver and returns the process exit status.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "as06:", err)
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

// scenario holds the state of one AS-06 run.
type scenario struct {
	opts    options
	rep     *report
	log     *slog.Logger
	a       *ucapi.Client
	b       *ucapi.Client
	secrets []string

	suffix      string
	tenantA     string
	channelID   string
	channelName string
	monitorID   string
	monitorName string
	// monitorUpdatedAt is the updatedAt of the monitor after its creation.
	monitorUpdatedAt time.Time
	maintID          string
	maintName        string
	tokenID          string
	tokenName        string
	// bMonitorID and bMonitorName belong to tenant B and serve as the positive
	// control of B's stream; they are removed in cleanup.
	bMonitorID   string
	bMonitorName string

	streamA *streamClient
	streamB *streamClient
}

func newScenario(opts options, rep *report, log *slog.Logger) *scenario {
	a := ucapi.New(opts.base, "", "")
	a.Token = opts.tokenA
	b := ucapi.New(opts.base, "", "")
	b.Token = opts.tokenB
	return &scenario{
		opts:    opts,
		rep:     rep,
		log:     log,
		a:       a,
		b:       b,
		secrets: []string{opts.tokenA, opts.tokenB},
	}
}

// redact removes every known secret from a text before it is printed.
func (s *scenario) redact(msg string) string {
	return scrub(msg, s.secrets...)
}

// row adds one result row; the measured text is scrubbed.
func (s *scenario) row(name, expected, measured string, pass bool) {
	s.rep.Add(name, expected, s.redact(measured), pass)
}

// fail records a FAIL row and returns an error that stops the scenario.
func (s *scenario) fail(name, expected, measured string, err error) error {
	s.row(name, expected, measured, false)
	return fmt.Errorf("%s: %s", name, s.redact(err.Error()))
}

// run executes the scenario. Cleanup of tenant A's objects and the streams
// always run, even when a step stops the scenario early.
func (s *scenario) run(ctx context.Context) error {
	err := s.steps(ctx)
	cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	s.cleanup(cctx)
	s.closeStreams()
	return err
}

// steps runs the checks. An error stops the steps; the rows so far remain.
func (s *scenario) steps(ctx context.Context) error {
	if err := s.checkTenants(ctx); err != nil {
		return err
	}
	if err := s.openStreams(ctx); err != nil {
		return err
	}
	if err := s.createObjects(ctx); err != nil {
		return err
	}
	s.probeFromB(ctx)
	s.listsFromB(ctx)
	s.checkUnchangedForA(ctx)
	s.checkEvents(ctx)
	return nil
}

// ownTenant is the part of GET /tenant the driver reads.
type ownTenant struct {
	ID string `json:"id"`
}

// checkTenants reads the tenant of both tokens and aborts if they are not two tenants.
func (s *scenario) checkTenants(ctx context.Context) error {
	var ta, tb ownTenant
	_, _, errA := s.a.DoRaw(ctx, http.MethodGet, "/tenant", nil, nil, &ta)
	_, _, errB := s.b.DoRaw(ctx, http.MethodGet, "/tenant", nil, nil, &tb)
	if errA != nil || errB != nil {
		return s.fail("Tenants of A and B", "GET /tenant answers 200 for both",
			fmt.Sprintf("A: %s, B: %s", errText(errA), errText(errB)), errors.New("tenant lookup failed"))
	}
	if ta.ID == "" || tb.ID == "" || ta.ID == tb.ID {
		return s.fail("Tenants of A and B", "two different tenants",
			"the tokens belong to the same tenant or the tenant ID is empty",
			errors.New("setup wrong: the tokens do not belong to two tenants"))
	}
	s.tenantA = ta.ID
	s.row("Tenants of A and B", "GET /tenant answers 200 for both, IDs differ", "200 and 200, IDs differ", true)
	return nil
}

// openStreams opens the live streams of B and A. B is opened first, so that
// it sees the creation of A's objects.
func (s *scenario) openStreams(ctx context.Context) error {
	var err error
	if s.streamB, err = startStream(ctx, s.b, s.opts.base); err != nil {
		return s.fail("Stream of B opened", "ticket 201, WebSocket upgrade", errText(err), err)
	}
	s.row("Stream of B opened", "ticket 201, WebSocket upgrade", "connected", true)
	if s.streamA, err = startStream(ctx, s.a, s.opts.base); err != nil {
		return s.fail("Stream of A opened", "ticket 201, WebSocket upgrade", errText(err), err)
	}
	s.row("Stream of A opened", "ticket 201, WebSocket upgrade", "connected", true)
	return nil
}

// createObjects creates the channel, monitor, maintenance window and API token of A.
func (s *scenario) createObjects(ctx context.Context) error {
	suffix, err := randomHex(4)
	if err != nil {
		return err
	}
	s.suffix = suffix
	now := time.Now().UTC()

	s.channelName = "as06-hook-" + suffix
	var ch ucapi.NotificationChannel
	chIn := map[string]any{
		"name":      s.channelName,
		"kind":      "webhook",
		"webhook":   map[string]any{"url": channelURL, "method": "POST"},
		"events":    []string{"down", "up"},
		"isDefault": false,
	}
	if _, err := s.a.Do(ctx, http.MethodPost, "/notifications", nil, chIn, &ch); err != nil {
		return s.fail("Channel of A created", "201, kind webhook", errText(err), err)
	}
	s.channelID = ch.ID
	s.row("Channel of A created", "201, kind webhook", "channel created", true)

	s.monitorName = "as06-http-" + suffix
	mon, err := s.a.CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:                   s.monitorName,
		Type:                   "http",
		Config:                 map[string]any{"url": monitorURL},
		IntervalSeconds:        monitorInterval,
		Retries:                0,
		NotificationChannelIDs: []string{s.channelID},
	})
	if err != nil {
		return s.fail("Monitor of A created", "201, interval 60, retries 0", errText(err), err)
	}
	s.monitorID = mon.ID
	s.monitorUpdatedAt = mon.UpdatedAt
	s.row("Monitor of A created", "201, interval 60, retries 0", "monitor created", true)

	s.maintName = "as06-maint-" + suffix
	mw, err := s.a.CreateMaintenance(ctx, ucapi.MaintenanceWindowInput{
		Name:        s.maintName,
		Description: "AS-06 acceptance window, removed by the driver",
		MonitorIDs:  []string{s.monitorID},
		Schedule:    onceSchedule(now.Add(24*time.Hour), now.Add(25*time.Hour)),
		Active:      true,
	})
	if err != nil {
		return s.fail("Maintenance window of A created", "201", errText(err), err)
	}
	s.maintID = mw.ID
	s.row("Maintenance window of A created", "201, refers to the monitor", "window created", true)

	s.tokenName = "as06-token-" + suffix
	var tok struct {
		ID string `json:"id"`
	}
	// Only the ID is decoded; the secret of the answer is never kept.
	tokIn := map[string]any{
		"name":      s.tokenName,
		"role":      "tenant-viewer",
		"expiresAt": now.Add(2 * time.Hour).Format(time.RFC3339),
	}
	if _, err := s.a.Do(ctx, http.MethodPost, "/api-tokens", nil, tokIn, &tok); err != nil {
		return s.fail("API token of A created", "201", errText(err), err)
	}
	if tok.ID == "" {
		return s.fail("API token of A created", "201 with an ID", "answer without ID", errors.New("no token ID"))
	}
	s.tokenID = tok.ID
	s.row("API token of A created", "201, secret not kept", "token created, ID kept", true)
	s.log.Info("objects of tenant A created", "monitor", s.monitorID, "channel", s.channelID,
		"maintenance", s.maintID, "token", s.tokenID)
	return nil
}

// onceSchedule returns a one-time maintenance schedule.
func onceSchedule(start, end time.Time) map[string]any {
	return map[string]any{
		"kind":     "once",
		"startsAt": start.Format(time.RFC3339),
		"endsAt":   end.Format(time.RFC3339),
	}
}

// probe is one request of tenant B against an object of tenant A.
type probe struct {
	name   string
	method string
	path   string
	query  url.Values
	body   any
	// denied lists the statuses that mean "not visible to B".
	denied []int
	// emptyOK accepts a 200 whose list holds no items.
	emptyOK bool
}

// probes returns the requests of tenant B on the objects of tenant A.
func (s *scenario) probes() []probe {
	mon := "/monitors/" + url.PathEscape(s.monitorID)
	ch := "/notifications/" + url.PathEscape(s.channelID)
	mw := "/maintenance/" + url.PathEscape(s.maintID)
	tok := "/api-tokens/" + url.PathEscape(s.tokenID)
	hijack := "as06-hijack-" + s.suffix
	later := time.Now().UTC().Add(48 * time.Hour)
	return []probe{
		{name: "B reads A's monitor", method: http.MethodGet, path: mon, denied: []int{404}},
		{name: "B updates A's monitor", method: http.MethodPut, path: mon, denied: []int{404},
			body: ucapi.MonitorInput{
				Name:            hijack,
				Type:            "http",
				Config:          map[string]any{"url": "https://target.as06.invalid/pwned"},
				IntervalSeconds: monitorInterval,
			}},
		{name: "B deletes A's monitor", method: http.MethodDelete, path: mon, denied: []int{404}},
		{name: "B pauses A's monitor", method: http.MethodPost, path: mon + "/pause", denied: []int{404}},
		{name: "B resumes A's monitor", method: http.MethodPost, path: mon + "/resume", denied: []int{404}},
		{name: "B clones A's monitor", method: http.MethodPost, path: mon + "/clone", denied: []int{404}},
		{name: "B tests A's monitor", method: http.MethodPost, path: mon + "/test", denied: []int{404}},
		{name: "B reads A's heartbeats", method: http.MethodGet, path: mon + "/heartbeats", denied: []int{404}},
		{name: "B reads A's uptime", method: http.MethodGet, path: mon + "/uptime",
			query: url.Values{"range": {"24h"}}, denied: []int{404}},
		{name: "B lists incidents of A's monitor", method: http.MethodGet, path: "/incidents",
			query: url.Values{"monitorId": {s.monitorID}}, denied: []int{404}, emptyOK: true},
		{name: "B bulk-pauses A's monitor", method: http.MethodPost, path: "/monitors/bulk",
			body: map[string]any{"action": "pause", "ids": []string{s.monitorID}}, denied: []int{404, 422}},
		{name: "B reads A's channel", method: http.MethodGet, path: ch, denied: []int{404}},
		{name: "B updates A's channel", method: http.MethodPut, path: ch, denied: []int{404},
			body: map[string]any{
				"name":    hijack,
				"kind":    "webhook",
				"webhook": map[string]any{"url": "https://target.as06.invalid/pwned"},
				"events":  []string{"down"},
			}},
		{name: "B tests A's channel", method: http.MethodPost, path: ch + "/test", denied: []int{404}},
		{name: "B deletes A's channel", method: http.MethodDelete, path: ch, denied: []int{404}},
		{name: "B reads A's maintenance window", method: http.MethodGet, path: mw, denied: []int{404}},
		{name: "B updates A's maintenance window", method: http.MethodPut, path: mw, denied: []int{404},
			body: map[string]any{
				"name":       hijack,
				"monitorIds": []string{},
				"schedule":   onceSchedule(later, later.Add(time.Hour)),
				"active":     false,
			}},
		{name: "B deletes A's maintenance window", method: http.MethodDelete, path: mw, denied: []int{404}},
		{name: "B revokes A's API token", method: http.MethodDelete, path: tok, denied: []int{404}},
	}
}

// probeFromB sends every probe as tenant B and judges the answers.
func (s *scenario) probeFromB(ctx context.Context) {
	needles := s.aNeedles(false)
	for _, p := range s.probes() {
		status, raw, err := s.b.DoRaw(ctx, p.method, p.path, p.query, p.body, nil)
		s.judgeProbe(p, status, raw, err, needles)
	}
}

// judgeProbe adds the row of one probe. It passes when B gets a denial and
// the answer contains none of A's names or the tenant ID.
func (s *scenario) judgeProbe(p probe, status int, raw []byte, err error, needles []needle) {
	measured := fmt.Sprintf("HTTP %d", status)
	pass := false
	switch {
	case status == 0:
		measured = "no answer: " + errText(err)
	case slices.Contains(p.denied, status):
		pass = true
	case p.emptyOK && status == http.StatusOK && emptyItems(raw):
		pass = true
		measured = "HTTP 200, no items"
	case status >= 200 && status <= 299:
		measured = fmt.Sprintf("HTTP %d, request accepted", status)
	default:
		measured = fmt.Sprintf("HTTP %d, not a denial", status)
	}
	if leaks := findLeaks(string(raw), needles); len(leaks) > 0 {
		pass = false
		measured += ", leaks " + strings.Join(leaks, ", ")
	}
	s.row(p.name, expectedDenial(p), measured, pass)
}

// expectedDenial describes the accepted answers of a probe.
func expectedDenial(p probe) string {
	parts := make([]string, 0, len(p.denied)+1)
	for _, st := range p.denied {
		parts = append(parts, fmt.Sprint(st))
	}
	text := strings.Join(parts, " or ")
	if p.emptyOK {
		text += " or 200 without items"
	}
	return text + ", no A data in the answer"
}

// emptyItems reports whether a page answer holds no items.
func emptyItems(raw []byte) bool {
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return false
	}
	return len(page.Items) == 0
}

// listsFromB reads the lists of tenant B and checks that none shows an ID or name of A.
func (s *scenario) listsFromB(ctx context.Context) {
	lists := []struct {
		name  string
		path  string
		query url.Values
	}{
		{"List of B: monitors", "/monitors", url.Values{"limit": {"500"}}},
		{"List of B: channels", "/notifications", nil},
		{"List of B: maintenance windows", "/maintenance", nil},
		{"List of B: incidents", "/incidents", url.Values{"limit": {"500"}}},
		{"List of B: API tokens", "/api-tokens", nil},
		{"List of B: audit log", "/audit", nil},
	}
	needles := s.aNeedles(true)
	for _, l := range lists {
		status, raw, err := s.b.DoRaw(ctx, http.MethodGet, l.path, l.query, nil, nil)
		measured := fmt.Sprintf("HTTP %d", status)
		pass := status == http.StatusOK
		if err != nil {
			measured = "error: " + errText(err)
			pass = false
		}
		if leaks := findLeaks(string(raw), needles); len(leaks) > 0 {
			pass = false
			measured += ", leaks " + strings.Join(leaks, ", ")
		}
		if pass {
			measured += ", no ID or name of A"
		}
		s.row(l.name, "200, no ID, name or tenant ID of A", measured, pass)
	}
}

// aNeedles returns the values of A that must not show up in B's answers.
// Without all, only names and the tenant ID are returned (single-object answers).
func (s *scenario) aNeedles(all bool) []needle {
	n := []needle{
		{"monitor name", s.monitorName},
		{"channel name", s.channelName},
		{"maintenance name", s.maintName},
		{"token name", s.tokenName},
		{"tenant ID", s.tenantA},
	}
	if all {
		n = append(n,
			needle{"monitor ID", s.monitorID},
			needle{"channel ID", s.channelID},
			needle{"maintenance ID", s.maintID},
			needle{"token ID", s.tokenID},
		)
	}
	return n
}

// checkUnchangedForA reads A's objects and checks that B's attempts changed nothing.
func (s *scenario) checkUnchangedForA(ctx context.Context) {
	m, err := s.a.GetMonitor(ctx, s.monitorID)
	switch {
	case err != nil:
		s.row("A's monitor unchanged", "200, same name, not paused, same updatedAt", errText(err), false)
	case m.Name != s.monitorName || m.Paused || !m.UpdatedAt.Equal(s.monitorUpdatedAt):
		s.row("A's monitor unchanged", "200, same name, not paused, same updatedAt",
			fmt.Sprintf("name same %t, paused %t, updatedAt same %t",
				m.Name == s.monitorName, m.Paused, m.UpdatedAt.Equal(s.monitorUpdatedAt)), false)
	default:
		s.row("A's monitor unchanged", "200, same name, not paused, same updatedAt",
			"200, same name, not paused, same updatedAt", true)
	}

	var ch ucapi.NotificationChannel
	if _, err := s.a.Do(ctx, http.MethodGet, "/notifications/"+url.PathEscape(s.channelID), nil, nil, &ch); err != nil {
		s.row("A's channel unchanged", "200, same name", errText(err), false)
	} else {
		s.row("A's channel unchanged", "200, same name", fmt.Sprintf("200, name same %t", ch.Name == s.channelName),
			ch.Name == s.channelName)
	}

	var mw ucapi.MaintenanceWindow
	if _, err := s.a.Do(ctx, http.MethodGet, "/maintenance/"+url.PathEscape(s.maintID), nil, nil, &mw); err != nil {
		s.row("A's maintenance window unchanged", "200, same name, active", errText(err), false)
	} else {
		ok := mw.Name == s.maintName && mw.Active
		s.row("A's maintenance window unchanged", "200, same name, active",
			fmt.Sprintf("200, name same %t, active %t", mw.Name == s.maintName, mw.Active), ok)
	}

	s.checkTokenListed(ctx)
}

// checkTokenListed checks that A's API token is listed as not expired and not revoked.
func (s *scenario) checkTokenListed(ctx context.Context) {
	var tokens []struct {
		ID      string `json:"id"`
		Expired bool   `json:"expired"`
	}
	if _, err := s.a.Do(ctx, http.MethodGet, "/api-tokens", nil, nil, &tokens); err != nil {
		s.row("A's API token not revoked", "listed, not expired", errText(err), false)
		return
	}
	found, expired := false, false
	for _, t := range tokens {
		if t.ID == s.tokenID {
			found = true
			expired = t.Expired
		}
	}
	s.row("A's API token not revoked", "listed, not expired",
		fmt.Sprintf("listed %t, expired %t", found, expired), found && !expired)
}

// checkEvents pauses and resumes A's monitor. A's stream must show an event of
// the monitor, and B's stream must show nothing of A.
func (s *scenario) checkEvents(ctx context.Context) {
	monPath := "/monitors/" + url.PathEscape(s.monitorID)
	from := s.streamA.count()
	for _, action := range []string{"pause", "resume"} {
		var m ucapi.Monitor
		if _, err := s.a.Do(ctx, http.MethodPost, monPath+"/"+action, nil, nil, &m); err != nil {
			s.row("A's monitor "+action+"d", "200", errText(err), false)
			continue
		}
		s.row("A's monitor "+action+"d", "200", "200", true)
	}

	monitorID := s.monitorID
	elapsed, ok := s.streamA.waitFor(ctx, from, s.opts.eventWait, func(msg string) bool {
		return strings.Contains(msg, monitorID)
	})
	expected := fmt.Sprintf("A's stream shows an event of the monitor within %s", s.opts.eventWait)
	if ok {
		s.row("A's stream shows event of its monitor", expected,
			fmt.Sprintf("event after %s", elapsed.Round(100*time.Millisecond)), true)
	} else {
		s.row("A's stream shows event of its monitor", expected,
			fmt.Sprintf("no event within %s", s.opts.eventWait), false)
	}

	s.checkBControl(ctx)

	if !sleepCtx(ctx, eventGrace) {
		s.row("B's stream carries no data of A", "no message with A's IDs, names or tenant ID",
			"interrupted", false)
		return
	}
	msgs := s.streamB.messages()
	needles := s.aNeedles(true)
	leaking := 0
	var labels []string
	for _, msg := range msgs {
		if l := findLeaks(msg, needles); len(l) > 0 {
			leaking++
			labels = append(labels, l...)
		}
	}
	measured := fmt.Sprintf("%d messages, none mentions A", len(msgs))
	if leaking > 0 {
		slices.Sort(labels)
		measured = fmt.Sprintf("%d of %d messages mention A (%s)", leaking, len(msgs),
			strings.Join(slices.Compact(labels), ", "))
	}
	s.row("B's stream carries no data of A", "no message with A's IDs, names or tenant ID",
		measured, leaking == 0)
}

// checkBControl is the positive control of B's stream. B creates its own
// monitor, pauses and resumes it, and its stream must show an event of that
// monitor. Without this control an empty stream of B would pass the no-data check.
func (s *scenario) checkBControl(ctx context.Context) {
	suffix, err := randomHex(4)
	if err != nil {
		s.row("B's own monitor created", "201, interval 60, retries 0", errText(err), false)
		return
	}
	s.bMonitorName = "as06-bmon-" + suffix
	mon, err := s.b.CreateMonitorInput(ctx, ucapi.MonitorInput{
		Name:                   s.bMonitorName,
		Type:                   "http",
		Config:                 map[string]any{"url": monitorURL},
		IntervalSeconds:        monitorInterval,
		Retries:                0,
		NotificationChannelIDs: []string{},
	})
	if err != nil {
		s.row("B's own monitor created", "201, interval 60, retries 0", errText(err), false)
		return
	}
	s.bMonitorID = mon.ID
	s.row("B's own monitor created", "201, interval 60, retries 0", "monitor created", true)

	bPath := "/monitors/" + url.PathEscape(s.bMonitorID)
	from := s.streamB.count()
	for _, action := range []string{"pause", "resume"} {
		var m ucapi.Monitor
		if _, err := s.b.Do(ctx, http.MethodPost, bPath+"/"+action, nil, nil, &m); err != nil {
			s.row("B's own monitor "+action+"d", "200", errText(err), false)
			continue
		}
		s.row("B's own monitor "+action+"d", "200", "200", true)
	}

	monitorID := s.bMonitorID
	elapsed, ok := s.streamB.waitFor(ctx, from, s.opts.eventWait, func(msg string) bool {
		return strings.Contains(msg, monitorID)
	})
	expected := fmt.Sprintf("B's stream shows an event of its own monitor within %s", s.opts.eventWait)
	if ok {
		s.row("B's stream shows event of its own monitor", expected,
			fmt.Sprintf("event after %s", elapsed.Round(100*time.Millisecond)), true)
	} else {
		s.row("B's stream shows event of its own monitor", expected,
			fmt.Sprintf("no event within %s", s.opts.eventWait), false)
	}
}

// cleanup removes B's and A's objects in dependency order. Failures are rows
// too, and do not stop the cleanup of the other objects.
func (s *scenario) cleanup(ctx context.Context) {
	if s.bMonitorID != "" {
		_, err := s.b.Do(ctx, http.MethodDelete, "/monitors/"+url.PathEscape(s.bMonitorID), nil, nil, nil)
		if err != nil {
			s.row("Cleanup: monitor of B deleted", "204", errText(err), false)
		} else {
			s.row("Cleanup: monitor of B deleted", "204", "204", true)
		}
	}
	del := func(name, path string, created bool) {
		if !created {
			return
		}
		_, err := s.a.Do(ctx, http.MethodDelete, path, nil, nil, nil)
		if err != nil {
			s.row("Cleanup: "+name+" deleted", "204", errText(err), false)
			return
		}
		s.row("Cleanup: "+name+" deleted", "204", "204", true)
	}
	del("maintenance window", "/maintenance/"+url.PathEscape(s.maintID), s.maintID != "")
	del("monitor", "/monitors/"+url.PathEscape(s.monitorID), s.monitorID != "")
	del("channel", "/notifications/"+url.PathEscape(s.channelID), s.channelID != "")
	del("API token", "/api-tokens/"+url.PathEscape(s.tokenID), s.tokenID != "")
}

// closeStreams closes the live streams that were opened.
func (s *scenario) closeStreams() {
	for _, sc := range []*streamClient{s.streamA, s.streamB} {
		if sc != nil {
			sc.close()
		}
	}
}

// errText returns the error text, or "ok" for nil.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// sleepCtx waits for d and reports false if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
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

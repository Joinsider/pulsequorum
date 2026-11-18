// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package alert

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicholas-fedor/shoutrrr/pkg/services"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

// ssrfText is the error text of a connection the address policy denies.
const ssrfText = "blocked by SSRF protection"

// shoutrrrCase describes how one Shoutrrr service is tested for ADR 0030
// section 4: a real connection attempt must go through our dialer and the
// SSRF policy must stop it.
type shoutrrrCase struct {
	scheme string
	// url is a Shoutrrr URL. "%s" is replaced with the listener address
	// (127.0.0.1:<port>) for services whose host comes from the URL.
	url string
	// fixedHost is the public host a service with a fixed host must request.
	// Such cases never reach the network: the test records the request host
	// in a replaced transport of the sender's HTTP client.
	fixedHost string
	// rejectAtSave is true if Sender.Validate must reject the URL.
	rejectAtSave bool
	// errorTextMayLack allows a blocked delivery without the SSRF error text.
	// Only for services that retry the connection until the context ends, so
	// the dial error is lost; the positive control still proves the connect.
	errorTextMayLack bool
}

// shoutrrrCases lists every scheme that services.SupportedSchemas returns.
// TestShoutrrrServicesUseDialer fails if a scheme is missing here.
var shoutrrrCases = []shoutrrrCase{
	{scheme: "bark", url: "bark://:devicekey@%s"},
	{scheme: "discord", url: "discord://token@123456789012345678", fixedHost: "discord.com"},
	{scheme: "generic", url: "generic://%s/webhook"},
	{scheme: "gotify", url: "gotify://%s/Aaa.bbb.ccc.ddd"},
	{scheme: "googlechat", url: "googlechat://%s/v1/spaces/SPACE/messages?key=KEY&token=TOKEN"},
	{scheme: "hangouts", url: "hangouts://%s/v1/spaces/SPACE/messages?key=KEY&token=TOKEN"},
	{scheme: "homeassistant", url: "homeassistant://s3cret@%s"},
	{scheme: "ifttt", url: "ifttt://dummyID/?events=event1", fixedHost: "maker.ifttt.com"},
	{scheme: "join", url: "join://:apikey@join?devices=device1", fixedHost: "joinjoaomgcd.appspot.com"},
	{scheme: "lark", url: "lark://open.larksuite.com/token", fixedHost: "open.larksuite.com"},
	{scheme: "logger", url: "logger://", rejectAtSave: true},
	{scheme: "matrix", url: "matrix://user:password@%s"},
	{scheme: "mattermost", url: "mattermost://%s/token"},
	{scheme: "mqtt", url: "mqtt://%s/topic", errorTextMayLack: true},
	{scheme: "mqtts", url: "mqtts://%s/topic", errorTextMayLack: true},
	{scheme: "notifiarr", url: "notifiarr://API_KEY", fixedHost: "notifiarr.com"},
	{scheme: "ntfy", url: "ntfy://%s/topic"},
	{scheme: "opsgenie", url: "opsgenie://%s/API_KEY"},
	{scheme: "pagerduty", url: "pagerduty://%s/0123456789abcdef0123456789abcdef"},
	{scheme: "pushbullet", url: "pushbullet://tokentokentokentokentokentokentoke", fixedHost: "api.pushbullet.com"},
	{scheme: "pushover", url: "pushover://userKey:apiToken@host", fixedHost: "api.pushover.net"}, //nolint:gosec // G101: placeholder credentials, never sent
	{scheme: "rocketchat", url: "rocketchat://%s/tokenA/tokenB"},
	{scheme: "signal", url: "signal://%s/+15551234567/+15557654321"},
	{scheme: "signalgrid", url: "signalgrid://key@channel", fixedHost: "api.signalgrid.co"},
	{scheme: "slack", url: "slack://xoxb:123456789012-1234567890123-4mt0t4l1YL3g1T5L4cK70k3N@C0123456789", fixedHost: "slack.com"}, //nolint:gosec // G101: placeholder token, never sent
	{scheme: "smtp", url: "smtp://user:password@%s/?fromAddress=sender@example.com&toAddresses=recipient@example.com"},
	{scheme: "teams", url: "teams://?host=https%3A%2F%2Fdefault0123.environment.api.powerplatform.com%3A443%2Fpowerautomate%2Fautomations%2Fdirect%2Fworkflows%2Fabc123%2Ftriggers%2Fmanual%2Fpaths%2Finvoke%3Fapi-version%3D1%26sp%3D%2Ftriggers%2Fmanual%2Frun%26sv%3D1.0%26sig%3DXXXXXXXX", fixedHost: "default0123.environment.api.powerplatform.com"},
	{scheme: "telegram", url: "telegram://123:token@telegram/?chats=@channel&apiurl=http://%s"},
	{scheme: "twilio", url: "twilio://AC0123456789abcdef0123456789ABCDEF:authToken@+15551234567/+15557654321", fixedHost: "api.twilio.com"},
	{scheme: "wecom", url: "wecom://693axxx6-7aoc-4bc4-97a0-0ec2sifa5aaa", fixedHost: "qyapi.weixin.qq.com"},
	{scheme: "xmpp", url: "xmpp://user:pass@%s/?to=a@ex.com"},
	{scheme: "xmpps", url: "xmpps://user:pass@%s/?to=a@ex.com"},
	{scheme: "zulip", url: "zulip://botmail:botkey@%s?stream=general&topic=alerts"},
}

// hostRecorder is an http.RoundTripper that records the host of each request
// and fails it. It performs no network I/O and no DNS lookup.
type hostRecorder struct {
	mu    sync.Mutex
	hosts []string
}

// RoundTrip implements http.RoundTripper.
func (r *hostRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.hosts = append(r.hosts, req.URL.Hostname())
	r.mu.Unlock()
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, errors.New("hostRecorder: request recorded, no network I/O")
}

// countingListener listens on 127.0.0.1 and counts accepted connections. Each
// connection is closed at once.
func countingListener(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var accepted atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()
	return ln.Addr().String(), &accepted
}

// waitAccepted waits up to wait for the counter to exceed before.
func waitAccepted(c *atomic.Int64, before int64, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for c.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return c.Load() > before
}

// TestShoutrrrServicesUseDialer sends one alert through every Shoutrrr service
// with SSRF protection on. URL-host services are sent to a local listener: a
// positive control without policy must connect, and the guarded run must not.
// Fixed-host services must request their public host through the sender's
// HTTP client. TestSenderTransportUsesGuardedDialer covers that client.
func TestShoutrrrServicesUseDialer(t *testing.T) {
	addr, accepted := countingListener(t)

	covered := make(map[string]bool, len(shoutrrrCases))
	for _, c := range shoutrrrCases {
		covered[c.scheme] = true
	}
	for _, s := range services.SupportedSchemas() {
		if !covered[s] {
			t.Errorf("Shoutrrr scheme %q has no case in shoutrrrCases", s)
		}
	}

	payload := cluster.AlertPayload{
		AlertID:     "shoutrrr-ssrf-test",
		MonitorName: "ssrf-test-monitor",
		Event:       store.EventDown,
		Status:      "DOWN",
	}

	for _, tc := range shoutrrrCases {
		t.Run(tc.scheme, func(t *testing.T) {
			if tc.fixedHost != "" {
				testFixedHostService(t, tc, payload)
				return
			}
			testURLHostService(t, tc, addr, accepted, payload)
		})
	}
}

// testFixedHostService checks that the service requests its fixed public host
// through the sender's HTTP client. The client's transport is replaced by a
// recorder, so no request leaves the test.
func testFixedHostService(t *testing.T, tc shoutrrrCase, payload cluster.AlertPayload) {
	sender := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())
	rec := &hostRecorder{}
	sender.client.Transport = rec
	target := Target{Kind: store.ChannelShoutrrr, URL: tc.url}

	if err := sender.Validate(target); err != nil {
		t.Fatalf("Validate rejected the URL, but the case expects a delivery: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sender.Send(ctx, target, payload)

	if err == nil {
		t.Error("Send succeeded through a recording transport, want an error")
	}
	if len(rec.hosts) == 0 {
		t.Fatalf("service did not use the sender's HTTP client (send error: %v)", err)
	}
	for _, h := range rec.hosts {
		if h != tc.fixedHost {
			t.Errorf("service requested host %q, want %q", h, tc.fixedHost)
		}
	}
}

// testURLHostService checks a service whose host comes from the URL.
func testURLHostService(t *testing.T, tc shoutrrrCase, addr string, accepted *atomic.Int64, payload cluster.AlertPayload) {
	rawURL := strings.ReplaceAll(tc.url, "%s", addr)
	target := Target{Kind: store.ChannelShoutrrr, URL: rawURL}
	guarded := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())

	err := guarded.Validate(target)
	if tc.rejectAtSave {
		if err == nil {
			t.Fatal("Validate accepted the URL, but the case expects a rejection at save")
		}
		t.Logf("rejected at save: %v", err)
		return
	}
	// The URL names a blocked IP literal, so the save-time rules of ADR 0030
	// reject it already. A name resolving there would pass Validate, so the
	// dial-time block is checked below regardless.
	if err == nil || !strings.Contains(err.Error(), ssrfText) {
		t.Fatalf("Validate = %v, want a rejection with %q at save", err, ssrfText)
	}

	// Positive control: without a policy the service must connect to the
	// listener. Otherwise a zero count below would not prove a block.
	open := NewSender(&checker.Dialer{}, slog.Default())
	before := accepted.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = open.Send(ctx, target, payload)
	cancel()
	if !waitAccepted(accepted, before, time.Second) {
		t.Fatal("positive control: service did not connect to the listener without a policy")
	}

	before = accepted.Load()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	err = guarded.Send(ctx, target, payload)
	cancel()

	if n := accepted.Load() - before; n != 0 {
		t.Errorf("listener accepted %d connection(s) despite SSRF protection (BYPASS)", n)
	}
	if err == nil {
		t.Fatal("Send succeeded, want an SSRF block")
	}
	// DeliveryError keeps no error chain, so the message is checked.
	if !tc.errorTextMayLack && !strings.Contains(err.Error(), ssrfText) {
		t.Errorf("Send failed without the SSRF error: %v", err)
	}
}

// TestSenderTransportUsesGuardedDialer checks that the HTTP client of a sender
// dials through its checker.Dialer. The same request must connect without a
// policy and fail with the SSRF error with BlockPrivate.
func TestSenderTransportUsesGuardedDialer(t *testing.T) {
	addr, accepted := countingListener(t)
	url := "http://" + addr + "/"

	request := func(s *Sender) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp, err := s.client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}

	// Sanity check: without a policy the request reaches the listener.
	open := NewSender(&checker.Dialer{}, slog.Default())
	before := accepted.Load()
	_ = request(open)
	if !waitAccepted(accepted, before, time.Second) {
		t.Fatal("sanity check: request did not reach the listener without a policy")
	}

	guarded := NewSender(&checker.Dialer{Policy: checker.BlockPrivate{}}, slog.Default())
	before = accepted.Load()
	err := request(guarded)
	if err == nil {
		t.Fatal("request to loopback through the guarded client succeeded, want an SSRF block")
	}
	if !strings.Contains(err.Error(), ssrfText) {
		t.Errorf("request failed without the SSRF error: %v", err)
	}
	if n := accepted.Load() - before; n != 0 {
		t.Errorf("listener accepted %d connection(s) through the guarded client", n)
	}
}

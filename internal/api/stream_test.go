// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/stream"
)

// expiringAuth is headerAuth with a credential expiry.
type expiringAuth struct{ expires time.Time }

func (a expiringAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	p, err := headerAuth{}.Authenticate(r)
	p.ExpiresAt = a.expires
	return p, err
}

type streamEnv struct {
	t       *testing.T
	srv     *httptest.Server
	hub     *stream.Hub
	closing chan struct{}
}

func newStreamEnv(t *testing.T, expires time.Time) *streamEnv {
	t.Helper()
	e := &streamEnv{t: t, hub: stream.NewHub(16), closing: make(chan struct{})}
	mux := http.NewServeMux()
	cfg := config.Config{NodeName: "n1", Role: config.RoleVoter, CORSOrigins: []string{"https://ui.example.com"}}
	Register(mux, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{
		Authenticator: expiringAuth{expires: expires},
		Stream:        e.hub, Tickets: auth.NewTickets(), StreamClosing: e.closing,
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *streamEnv) ticket(principal string) string {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/stream/ticket", nil)
	req.Header.Set("X-Test-Principal", principal)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("ticket: status %d", res.StatusCode)
	}
	var tk gen.StreamTicket
	if err := json.NewDecoder(res.Body).Decode(&tk); err != nil {
		e.t.Fatal(err)
	}
	return tk.Ticket
}

func (e *streamEnv) dial(query, origin string) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if origin != "" {
		opts.HTTPHeader.Set("Origin", origin)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/api/v1/stream?"+query, opts)
}

// dialStatus dials and returns only the HTTP status of a failed handshake
// (0 if there was no response). The response body is closed here.
func (e *streamEnv) dialStatus(query, origin string) (int, error) {
	_, res, err := e.dial(query, origin)
	if res == nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode, err
}

func (e *streamEnv) connect(principal, extra string) *websocket.Conn {
	e.t.Helper()
	conn, res, err := e.dial("ticket="+e.ticket(principal)+extra, "")
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.CloseNow() })
	// The subscription exists once the handler runs; wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for e.hub.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return conn
}

func read(t *testing.T, conn *websocket.Conn, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	return string(data), err
}

func waitSubscribers(t *testing.T, hub *stream.Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for hub.Subscribers() != n {
		if time.Now().After(deadline) {
			t.Fatalf("subscribers = %d, want %d", hub.Subscribers(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// FA-TEN-05: each connection receives only the events of its ticket's tenant.
func TestStreamTenantIsolation(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Time{})
	a := e.connect(viewer, "")
	waitSubscribers(t, e.hub, 1)
	b := e.connect(other, "")
	waitSubscribers(t, e.hub, 2)

	e.hub.Publish(stream.Event{TenantID: "tenant-b", MonitorID: "m-b", Data: []byte(`"b"`)})
	e.hub.Publish(stream.Event{TenantID: "default", MonitorID: "m-a", Data: []byte(`"a"`)})

	if got, err := read(t, a, 2*time.Second); err != nil || got != `"a"` {
		t.Errorf("tenant default got %q, %v", got, err)
	}
	if got, err := read(t, b, 2*time.Second); err != nil || got != `"b"` {
		t.Errorf("tenant-b got %q, %v", got, err)
	}
	if got, err := read(t, a, 100*time.Millisecond); err == nil {
		t.Errorf("tenant default got a second message %q", got)
	}
}

func TestStreamMonitorFilter(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Time{})
	m1, m2 := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	conn := e.connect(viewer, "&monitorId="+m1)
	waitSubscribers(t, e.hub, 1)
	e.hub.Publish(stream.Event{TenantID: "default", MonitorID: m2, Data: []byte(`2`)})
	e.hub.Publish(stream.Event{TenantID: "default", MonitorID: m1, Data: []byte(`1`)})
	if got, err := read(t, conn, 2*time.Second); err != nil || got != "1" {
		t.Fatalf("got %q, %v", got, err)
	}

	// The client replaces the filter with a message.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"monitorIds":["`+m2+`"]}`)); err != nil {
		t.Fatal(err)
	}
	// The filter message is applied asynchronously; publish until it arrives.
	// (A read timeout would close the connection, so a single read is used.)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				e.hub.Publish(stream.Event{TenantID: "default", MonitorID: m1, Data: []byte(`1`)})
				e.hub.Publish(stream.Event{TenantID: "default", MonitorID: m2, Data: []byte(`2`)})
			}
		}
	}()
	// Events of m1 may arrive until the change applies; then only m2.
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	for {
		_, data, err := conn.Read(rctx)
		if err != nil {
			t.Fatalf("filter change not applied: %v", err)
		}
		if string(data) == "2" {
			break
		}
	}
	for range 3 {
		if _, data, err := conn.Read(rctx); err != nil || string(data) != "2" {
			t.Fatalf("after filter change got %q, %v", data, err)
		}
	}
	close(stop)

	// An invalid filter ends the stream.
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"monitorIds":["../x"]}`)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := read(t, conn, 2*time.Second); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("close status = %v (%v)", websocket.CloseStatus(err), err)
			}
			break
		}
	}
}

// FA-AUTH-11: tickets are required, single use, and checked against Origin.
func TestStreamTicketRules(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Time{})

	if status, err := e.dialStatus("", ""); err == nil || status != http.StatusUnauthorized {
		t.Errorf("without ticket: %v", err)
	}
	if status, err := e.dialStatus("ticket=forged", ""); err == nil || status != http.StatusUnauthorized {
		t.Errorf("forged ticket: %v", err)
	}

	tk := e.ticket(viewer)
	if status, err := e.dialStatus("ticket="+tk, "https://evil.example.net"); err == nil || status != http.StatusForbidden {
		t.Errorf("foreign origin: %v", err)
	}
	conn, res, err := e.dial("ticket="+tk, "https://ui.example.com")
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		t.Fatalf("allowed origin: %v", err)
	}
	_ = conn.CloseNow()
	if status, err := e.dialStatus("ticket="+tk, ""); err == nil || status != http.StatusUnauthorized {
		t.Errorf("reused ticket: %v", err)
	}

	tk = e.ticket(viewer)
	if status, err := e.dialStatus("ticket="+tk+"&monitorId=nope", ""); err == nil || status != http.StatusBadRequest {
		t.Errorf("invalid monitor filter: %v", err)
	}

	// Anonymous callers get no ticket.
	res, err = http.Post(e.srv.URL+"/api/v1/stream/ticket", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous ticket: status %d", res.StatusCode)
	}
}

// FA-AUTH-11: the stream ends when the session the ticket was issued for ends.
func TestStreamClosesAtSessionEnd(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Now().Add(1500*time.Millisecond))
	conn := e.connect(viewer, "")
	_, err := read(t, conn, 5*time.Second)
	if websocket.CloseStatus(err) != closeSessionEnded {
		t.Fatalf("close = %v (%v), want %d", websocket.CloseStatus(err), err, closeSessionEnded)
	}
	waitSubscribers(t, e.hub, 0)
}

func TestStreamClosesOnShutdown(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Time{})
	conn := e.connect(viewer, "")
	waitSubscribers(t, e.hub, 1)
	close(e.closing)
	_, err := read(t, conn, 5*time.Second)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close = %v (%v), want going away", websocket.CloseStatus(err), err)
	}
}

func TestStreamClosesSlowClient(t *testing.T) {
	t.Parallel()
	e := newStreamEnv(t, time.Time{})
	conn := e.connect(viewer, "")
	waitSubscribers(t, e.hub, 1)
	// The client does not read; the hub buffer (16) overflows long before the
	// TCP buffers block the writer, or the writer times out.
	big := []byte(`"` + strings.Repeat("x", 64<<10) + `"`)
	for range 400 {
		e.hub.Publish(stream.Event{TenantID: "default", Data: big})
	}
	var err error
	for err == nil {
		_, err = read(t, conn, 15*time.Second)
	}
	if code := websocket.CloseStatus(err); code != closeTooSlow && !errors.Is(err, io.EOF) && code != -1 {
		t.Fatalf("close = %v (%v)", code, err)
	}
	waitSubscribers(t, e.hub, 0)
}

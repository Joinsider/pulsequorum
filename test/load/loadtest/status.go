// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	errNoEvent             = errors.New("no state event")
	errLostDuringReconnect = errors.New("state event lost during reconnect")
)

// stateEvent is a state message of the live stream with its local arrival time.
type stateEvent struct {
	MonitorID string
	Status    string
	ts        time.Time // event.ts, set by the voter when the change is committed
	received  time.Time // local clock when the message was read
}

// timeSpan is a period without a stream connection. to is zero while it lasts.
type timeSpan struct {
	from, to time.Time
}

// stateWaiter is registered before a change is triggered. It matches events
// of its monitor and status from index from of the event log onwards.
type stateWaiter struct {
	monitorID string
	status    string
	from      int
	since     time.Time
}

// streamListener keeps one WebSocket on /api/v1/stream open and reconnects
// after a failure. Its single reader stamps every state event on arrival and
// appends it to an event log; it never waits for a waiter. Waiters scan the
// log from their registration point, so an event cannot be lost because the
// waiter was not registered yet, and a late waiter cannot delay the reader.
type streamListener struct {
	client *apiClient
	log    *slog.Logger

	ready     chan struct{}
	readyOnce sync.Once

	mu          sync.Mutex
	events      []stateEvent
	notify      chan struct{} // closed and replaced whenever events grows
	gaps        []timeSpan    // periods without a connection, starting with the initial one
	connected   bool
	stale       bool // the last connection ended, so the next one logs in again
	connections int  // successful dials
}

func newStreamListener(client *apiClient, log *slog.Logger) *streamListener {
	return &streamListener{
		client: client,
		log:    log,
		ready:  make(chan struct{}),
		notify: make(chan struct{}),
		gaps:   []timeSpan{{from: time.Now()}},
	}
}

// register must be called before the change is triggered, so no event can be missed.
func (s *streamListener) register(monitorID, status string) *stateWaiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &stateWaiter{monitorID: monitorID, status: status, from: len(s.events), since: time.Now()}
}

// await blocks until an event matching w arrives, the timeout passes or ctx
// ends. A timeout during which the stream was down returns
// errLostDuringReconnect, because the event may have been sent while the
// stream was closed.
func (s *streamListener) await(ctx context.Context, w *stateWaiter, timeout time.Duration) (stateEvent, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	from := w.from
	for {
		s.mu.Lock()
		if ev, ok := s.match(w, from); ok {
			s.mu.Unlock()
			return ev, nil
		}
		from = len(s.events)
		notify := s.notify
		s.mu.Unlock()

		select {
		case <-notify:
		case <-t.C:
			s.mu.Lock()
			ev, ok := s.match(w, w.from)
			lost := s.disconnectedSince(w.since)
			s.mu.Unlock()
			switch {
			case ok:
				return ev, nil
			case lost:
				return stateEvent{}, errLostDuringReconnect
			default:
				return stateEvent{}, errNoEvent
			}
		case <-ctx.Done():
			return stateEvent{}, ctx.Err()
		}
	}
}

// match returns the first event at or after index from that matches w.
// The caller must hold s.mu.
func (s *streamListener) match(w *stateWaiter, from int) (stateEvent, bool) {
	for _, ev := range s.events[from:] {
		if ev.MonitorID == w.monitorID && ev.Status == w.status {
			return ev, true
		}
	}
	return stateEvent{}, false
}

// disconnectedSince reports whether the stream was down at any time after t.
// The caller must hold s.mu.
func (s *streamListener) disconnectedSince(t time.Time) bool {
	for _, g := range s.gaps {
		if g.to.IsZero() || g.to.After(t) {
			return true
		}
	}
	return false
}

// markUp records a successful dial and closes the open gap.
func (s *streamListener) markUp(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected {
		return
	}
	s.connected = true
	s.connections++
	if n := len(s.gaps); n > 0 && s.gaps[n-1].to.IsZero() {
		s.gaps[n-1].to = at
	}
}

// markDown records that the connection ended and opens a gap.
func (s *streamListener) markDown(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return
	}
	s.connected = false
	s.gaps = append(s.gaps, timeSpan{from: at})
}

// reconnectCount returns how often the stream was re-established.
func (s *streamListener) reconnectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return max(s.connections-1, 0)
}

// connectionCount returns the number of successful dials.
func (s *streamListener) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

// snapshot returns a copy of all state events received so far.
func (s *streamListener) snapshot() []stateEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stateEvent, len(s.events))
	copy(out, s.events)
	return out
}

// waitReady blocks until the first connection is established.
func (s *streamListener) waitReady(ctx context.Context, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.ready:
		return nil
	case <-t.C:
		return errors.New("stream not connected in time")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// messageSource is the read side of a WebSocket connection.
type messageSource interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
}

// consume is the only reader of a connection. It returns when a read fails.
func (s *streamListener) consume(ctx context.Context, src messageSource) error {
	for {
		_, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		s.record(data, time.Now())
	}
}

type streamEvent struct {
	Type      string    `json:"type"`
	Ts        time.Time `json:"ts"`
	MonitorID string    `json:"monitorId"`
	Status    string    `json:"status"`
}

// record decodes one message and appends it to the log when it is a state event.
func (s *streamListener) record(data []byte, received time.Time) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil || ev.Type != "state" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, stateEvent{MonitorID: ev.MonitorID, Status: ev.Status, ts: ev.Ts, received: received})
	close(s.notify)
	s.notify = make(chan struct{})
}

// run keeps the stream open until ctx ends. After every disconnect the next
// connection logs in again and requests a new ticket, because the voter
// closes a stream with 4001 "session expired" when the session ends.
func (s *streamListener) run(ctx context.Context) {
	for {
		err := s.connect(ctx)
		s.markDown(time.Now())
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.stale = true
		s.mu.Unlock()
		s.log.Warn("stream disconnected, reconnecting", "err", err, "closeCode", int(websocket.CloseStatus(err)))
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// loginIfStale logs in again when the previous connection ended.
func (s *streamListener) loginIfStale(ctx context.Context) error {
	s.mu.Lock()
	stale := s.stale
	s.mu.Unlock()
	if !stale {
		return nil
	}
	if err := s.client.reLogin(ctx); err != nil {
		return fmt.Errorf("re-login after stream loss: %w", err)
	}
	s.mu.Lock()
	s.stale = false
	s.mu.Unlock()
	return nil
}

// connect logs in if needed, obtains a ticket, dials the WebSocket and reads
// until it fails. No Origin header is sent; non-browser clients are allowed.
func (s *streamListener) connect(ctx context.Context) error {
	if err := s.loginIfStale(ctx); err != nil {
		return err
	}
	ticket, err := s.client.streamTicket(ctx)
	if err != nil {
		return fmt.Errorf("stream ticket: %w", err)
	}
	wsBase, err := websocketBase(s.client.base)
	if err != nil {
		return err
	}
	dialURL := wsBase + apiPrefix + "/stream?ticket=" + url.QueryEscape(ticket)
	conn, resp, err := websocket.Dial(ctx, dialURL, nil)
	// websocket.Dial clears resp.Body after a successful upgrade; on failure the
	// body is already drained and closed, so closing it here is a no-op.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// Do not leak the one-time ticket into logs.
		return fmt.Errorf("dial stream: %s", strings.ReplaceAll(err.Error(), ticket, "***"))
	}
	defer func() { _ = conn.CloseNow() }()
	s.markUp(time.Now())
	s.readyOnce.Do(func() { close(s.ready) })
	return fmt.Errorf("read stream: %w", s.consume(ctx, conn))
}

// websocketBase maps the HTTP base URL to ws:// or wss://.
func websocketBase(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// unexpectedEvent is a DOWN or PENDING state event of a monitor the tool did not flip.
type unexpectedEvent struct {
	MonitorID  string    `json:"monitorId"`
	Status     string    `json:"status"`
	EventTS    time.Time `json:"eventTs"`
	ReceivedAt time.Time `json:"receivedAt"`
}

// unexpectedSummary counts state changes the tool did not cause. They are
// false alarms of the environment or the product.
type unexpectedSummary struct {
	Down     int               `json:"down"`
	Pending  int               `json:"pending"`
	Monitors int               `json:"monitors"`
	First    []unexpectedEvent `json:"first,omitempty"`
}

// unexpectedExamples is the number of events listed in the report.
const unexpectedExamples = 5

// unexpectedChanges counts DOWN and PENDING events of monitors that are not
// in flipped, in arrival order.
func unexpectedChanges(events []stateEvent, flipped map[string]bool) unexpectedSummary {
	var s unexpectedSummary
	seen := make(map[string]bool)
	for _, ev := range events {
		if flipped[ev.MonitorID] {
			continue
		}
		switch ev.Status {
		case "DOWN":
			s.Down++
		case "PENDING":
			s.Pending++
		default:
			continue
		}
		seen[ev.MonitorID] = true
		if len(s.First) < unexpectedExamples {
			s.First = append(s.First, unexpectedEvent{
				MonitorID: ev.MonitorID, Status: ev.Status, EventTS: ev.ts, ReceivedAt: ev.received,
			})
		}
	}
	s.Monitors = len(seen)
	return s
}

// statusProbe flips one target down and up and measures how long the voter
// needs until the state event reaches the stream.
type statusProbe struct {
	client   *apiClient
	stream   *streamListener
	control  string // target farm base URL
	httpc    *http.Client
	timeout  time.Duration
	monitors []monitorRef
	rec      *recorder
	log      *slog.Logger

	mu   sync.Mutex
	busy map[string]bool
}

func newStatusProbe(client *apiClient, stream *streamListener, control string, timeout time.Duration,
	monitors []monitorRef, rec *recorder, log *slog.Logger) *statusProbe {
	return &statusProbe{
		client:   client,
		stream:   stream,
		control:  strings.TrimRight(control, "/"),
		httpc:    &http.Client{Timeout: 10 * time.Second},
		timeout:  timeout,
		monitors: monitors,
		rec:      rec,
		log:      log,
		busy:     make(map[string]bool),
	}
}

// schedule starts one cycle now and then every interval. Cycles of different
// monitors may overlap; a monitor is only used by one cycle at a time. After
// ctx ends, running cycles get grace to finish before they are cancelled
// (immediately if interrupt ends).
func (p *statusProbe) schedule(ctx, interrupt context.Context, interval, grace time.Duration) {
	cycleCtx, cancelCycles := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelCycles()
	var wg sync.WaitGroup
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		p.startCycle(cycleCtx, &wg)
		select {
		case <-ctx.Done():
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(grace):
				p.log.Warn("cycles still running, cancelling", "grace", grace)
				cancelCycles()
				<-done
			case <-interrupt.Done():
				cancelCycles()
				<-done
			}
			return
		case <-ticker.C:
		}
	}
}

func (p *statusProbe) startCycle(ctx context.Context, wg *sync.WaitGroup) {
	p.mu.Lock()
	free := make([]monitorRef, 0, len(p.monitors))
	for _, m := range p.monitors {
		if !p.busy[m.ID] {
			free = append(free, m)
		}
	}
	if len(free) == 0 {
		p.mu.Unlock()
		p.log.Warn("no free monitor for status cycle")
		return
	}
	m := free[rand.IntN(len(free))]
	p.busy[m.ID] = true
	p.mu.Unlock()
	p.rec.markFlipped(m.ID)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.busy, m.ID)
			p.mu.Unlock()
		}()
		p.cycle(ctx, m)
	}()
}

// cycle switches the target of m down, waits for the DOWN state event, then
// up, and waits for the UP event. The target is restored on every exit path.
func (p *statusProbe) cycle(ctx context.Context, m monitorRef) {
	down := false
	defer func() {
		if down {
			rc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := p.flipTarget(rc, m.TargetIndex, "up"); err != nil {
				p.log.Error("could not restore target", "monitor", m.ID, "err", err)
			}
		}
	}()

	down = true
	_ = p.flip(ctx, m, "down")
	if p.flip(ctx, m, "up") == nil {
		down = false
	}
}

// flip changes the target state and records the time until the matching
// state event of the monitor arrives. The waiter is registered before the
// target changes.
func (p *statusProbe) flip(ctx context.Context, m monitorRef, direction string) error {
	status := "UP"
	if direction == "down" {
		status = "DOWN"
	}
	w := p.stream.register(m.ID, status)
	flippedAt := time.Now()
	sample := statusSample{MonitorID: m.ID, TargetIndex: m.TargetIndex, Direction: direction, FlippedAt: flippedAt}

	if err := p.flipTarget(ctx, m.TargetIndex, direction); err != nil {
		sample.Error = "flip target: " + err.Error()
		p.rec.addStatus(sample)
		return err
	}
	ev, err := p.stream.await(ctx, w, p.timeout)
	switch {
	case err == nil:
	case errors.Is(err, errNoEvent):
		sample.Timeout = true
		sample.Error = fmt.Sprintf("no %s event within %s", status, p.timeout)
	case errors.Is(err, errLostDuringReconnect):
		sample.LostDuringReconnect = true
		sample.Error = fmt.Sprintf("%s event lost during stream reconnect", status)
	default:
		sample.Error = err.Error()
	}
	if err != nil {
		p.rec.addStatus(sample)
		return err
	}
	sample.EventTS = ev.ts
	sample.ReceivedAt = ev.received
	sample.LatencyMs = ms(ev.received.Sub(ev.ts))
	sample.FlipToEventMs = ms(ev.received.Sub(flippedAt))
	p.rec.addStatus(sample)
	return nil
}

func (p *statusProbe) flipTarget(ctx context.Context, index int, direction string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/control/%d/%s", p.control, index, direction), nil)
	if err != nil {
		return err
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("control %s: HTTP %d", direction, resp.StatusCode)
	}
	return nil
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

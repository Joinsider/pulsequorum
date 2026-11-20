// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeSource delivers messages without waiting and records when each one was
// handed to the reader. After the messages it blocks until ctx ends.
type fakeSource struct {
	msgs      [][]byte
	delivered []time.Time
	next      int
}

func newFakeSource(msgs ...string) *fakeSource {
	f := &fakeSource{delivered: make([]time.Time, len(msgs))}
	for _, m := range msgs {
		f.msgs = append(f.msgs, []byte(m))
	}
	return f
}

func (f *fakeSource) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	if f.next < len(f.msgs) {
		i := f.next
		f.next++
		f.delivered[i] = time.Now()
		return websocket.MessageText, f.msgs[i], nil
	}
	<-ctx.Done()
	return 0, nil, ctx.Err()
}

func stateMsg(monitorID, status string) string {
	return `{"type":"state","ts":"2026-10-22T12:00:00Z","monitorId":"` + monitorID + `","status":"` + status + `"}`
}

// testListener returns a listener whose stream is connected, so that a
// timeout is a plain timeout unless the test disconnects it.
func testListener(t *testing.T) *streamListener {
	t.Helper()
	s := newStreamListener(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.markUp(time.Now())
	return s
}

// The reader must stamp receivedAt as soon as a message arrives, even when
// no waiter is registered. The old design delayed this until a waiter came.
func TestConsumeStampsReceivedWithoutWaiter(t *testing.T) {
	s := testListener(t)
	src := newFakeSource(stateMsg("m1", "DOWN"), stateMsg("m2", "UP"), `{"type":"other"}`, stateMsg("m3", "DOWN"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.consume(ctx, src) }()

	deadline := time.Now().Add(2 * time.Second)
	for len(s.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	events := s.snapshot()
	if len(events) != 3 {
		t.Fatalf("got %d state events, want 3 (no waiter was registered)", len(events))
	}
	want := []string{"m1", "m2", "m3"}
	for i, ev := range events {
		if ev.MonitorID != want[i] {
			t.Errorf("event %d monitor = %s, want %s", i, ev.MonitorID, want[i])
		}
		if d := ev.received.Sub(src.delivered[[]int{0, 1, 3}[i]]); d < 0 || d > 250*time.Millisecond {
			t.Errorf("event %d received %v after arrival, want immediate stamp", i, d)
		}
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("consume returned %v, want context canceled", err)
	}
}

func TestAwaitFindsEventAfterRegistration(t *testing.T) {
	tests := []struct {
		name      string
		beforeReg []string // events that arrive before registration
		afterReg  []string // events that arrive after registration, possibly from another goroutine
		want      string   // monitor ID of the event that must be returned, or "" for timeout
	}{
		{"event after registration", nil, []string{stateMsg("m1", "DOWN")}, "m1"},
		{"other monitor and status are ignored", nil, []string{stateMsg("m2", "DOWN"), stateMsg("m1", "UP"), stateMsg("m1", "DOWN")}, "m1"},
		{"event before registration is not matched", []string{stateMsg("m1", "DOWN")}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testListener(t)
			for _, m := range tt.beforeReg {
				s.record([]byte(m), time.Now())
			}
			w := s.register("m1", "DOWN")
			go func() {
				time.Sleep(10 * time.Millisecond)
				for _, m := range tt.afterReg {
					s.record([]byte(m), time.Now())
				}
			}()
			ev, err := s.await(context.Background(), w, 300*time.Millisecond)
			if tt.want == "" {
				if !errors.Is(err, errNoEvent) {
					t.Fatalf("err = %v, want errNoEvent", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("await: %v", err)
			}
			if ev.MonitorID != tt.want || ev.Status != "DOWN" {
				t.Errorf("got %+v, want monitor %s DOWN", ev, tt.want)
			}
		})
	}
}

func TestAwaitTimeoutDuringReconnectIsLost(t *testing.T) {
	s := testListener(t)

	// Timeout without any disconnect is a plain timeout.
	w := s.register("m1", "UP")
	if _, err := s.await(context.Background(), w, 20*time.Millisecond); !errors.Is(err, errNoEvent) {
		t.Fatalf("err = %v, want errNoEvent", err)
	}

	// The stream drops while waiting: the wait is lost, not a timeout.
	w = s.register("m1", "UP")
	s.markDown(time.Now())
	if _, err := s.await(context.Background(), w, 20*time.Millisecond); !errors.Is(err, errLostDuringReconnect) {
		t.Fatalf("err = %v, want errLostDuringReconnect", err)
	}

	// After the reconnect a new wait is a plain timeout again.
	s.markUp(time.Now())
	w = s.register("m1", "UP")
	if _, err := s.await(context.Background(), w, 20*time.Millisecond); !errors.Is(err, errNoEvent) {
		t.Fatalf("err = %v, want errNoEvent after reconnect", err)
	}
}

func TestAwaitEventDeliveredDuringReconnectStillCounts(t *testing.T) {
	s := testListener(t)
	w := s.register("m1", "DOWN")
	s.markDown(time.Now())
	s.markUp(time.Now())
	s.record([]byte(stateMsg("m1", "DOWN")), time.Now())
	if _, err := s.await(context.Background(), w, time.Second); err != nil {
		t.Fatalf("await: %v, want the event that arrived after reconnect", err)
	}
}

func TestReconnectCounting(t *testing.T) {
	s := newStreamListener(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s.connectionCount() != 0 || s.reconnectCount() != 0 {
		t.Fatal("new listener must have no connections")
	}
	s.markUp(time.Now())
	s.markUp(time.Now()) // second call while connected changes nothing
	if s.connectionCount() != 1 || s.reconnectCount() != 0 {
		t.Fatalf("after first dial: connections %d, reconnects %d", s.connectionCount(), s.reconnectCount())
	}
	s.markDown(time.Now())
	s.markDown(time.Now())
	s.markUp(time.Now())
	s.markDown(time.Now())
	s.markUp(time.Now())
	if s.connectionCount() != 3 || s.reconnectCount() != 2 {
		t.Errorf("connections %d, reconnects %d; want 3 and 2", s.connectionCount(), s.reconnectCount())
	}
}

func TestLoginIfStale(t *testing.T) {
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			logins.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accessToken":"tok","expiresAt":"2030-01-01T00:00:00Z"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client, err := newAPIClient(srv.URL, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	s := newStreamListener(client, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := s.loginIfStale(context.Background()); err != nil || logins.Load() != 0 {
		t.Fatalf("not stale: err %v, logins %d; want no login", err, logins.Load())
	}
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
	if err := s.loginIfStale(context.Background()); err != nil {
		t.Fatalf("stale: %v", err)
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want 1 after a disconnect", logins.Load())
	}
	if s.stale {
		t.Error("stale flag must be cleared after a successful login")
	}
	if tok, _, _ := client.currentToken(context.Background()); tok != "tok" {
		t.Errorf("token = %q, want the new token", tok)
	}
}

func TestUnexpectedChanges(t *testing.T) {
	events := []stateEvent{
		{MonitorID: "flipped", Status: "DOWN"},
		{MonitorID: "a", Status: "UP"},
		{MonitorID: "a", Status: "DOWN"},
		{MonitorID: "b", Status: "PENDING"},
		{MonitorID: "a", Status: "DOWN"},
		{MonitorID: "flipped", Status: "PENDING"},
	}
	flipped := map[string]bool{"flipped": true}
	got := unexpectedChanges(events, flipped)
	if got.Down != 2 || got.Pending != 1 || got.Monitors != 2 {
		t.Fatalf("got down %d pending %d monitors %d, want 2 1 2", got.Down, got.Pending, got.Monitors)
	}
	if len(got.First) != 3 || got.First[0].MonitorID != "a" || got.First[0].Status != "DOWN" || got.First[1].Status != "PENDING" {
		t.Errorf("first events = %+v", got.First)
	}

	many := make([]stateEvent, 0, 10)
	for range 10 {
		many = append(many, stateEvent{MonitorID: "x", Status: "DOWN"})
	}
	if got := unexpectedChanges(many, nil); len(got.First) != unexpectedExamples || got.Down != 10 {
		t.Errorf("examples = %d, down = %d; want %d and 10", len(got.First), got.Down, unexpectedExamples)
	}
	if got := unexpectedChanges(nil, nil); got.Down != 0 || got.Monitors != 0 {
		t.Errorf("empty input: %+v", got)
	}
}

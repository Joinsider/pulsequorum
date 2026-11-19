// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

// streamPollInterval is how often waitFor looks at the collected messages.
const streamPollInterval = 500 * time.Millisecond

// streamClient is one live stream (GET /api/v1/stream) that collects every
// text message it receives until it is closed.
type streamClient struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	msgs []string
}

// startStream issues a one-time ticket with the token of c and opens the
// WebSocket with it. The ticket is scrubbed from every error text.
func startStream(ctx context.Context, c *ucapi.Client, base string) (*streamClient, error) {
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if _, _, err := c.DoRaw(ctx, http.MethodPost, "/stream/ticket", nil, nil, &tk); err != nil {
		return nil, fmt.Errorf("stream ticket: %w", err)
	}
	if tk.Ticket == "" {
		return nil, errors.New("stream ticket: answer without ticket")
	}
	wsURL, err := streamURL(base, tk.Ticket)
	if err != nil {
		return nil, fmt.Errorf("stream URL: %w", err)
	}
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	// websocket.Dial clears resp.Body after a successful upgrade; on failure the
	// body is already drained and closed, so closing it here is a no-op.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// The dial error may quote the URL, which carries the ticket.
		msg := scrub(err.Error(), tk.Ticket, url.QueryEscape(tk.Ticket))
		return nil, fmt.Errorf("dial stream: %s", msg)
	}
	// The reader lives until close() cancels it; it keeps its own lifetime and
	// does not end with the cancellation of ctx, which only bounds the dial.
	readCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sc := &streamClient{conn: conn, cancel: cancel, done: make(chan struct{})}
	go sc.read(readCtx)
	return sc, nil
}

// streamURL returns the WebSocket URL of the stream for a ticket.
func streamURL(base, ticket string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse base: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + ucapi.APIBasePath + "/stream"
	u.RawQuery = url.Values{"ticket": {ticket}}.Encode()
	return u.String(), nil
}

// read collects the messages until the connection ends.
func (s *streamClient) read(ctx context.Context) {
	defer close(s.done)
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, string(data))
		s.mu.Unlock()
	}
}

// count returns the number of messages received so far.
func (s *streamClient) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

// messages returns a copy of all messages received so far.
func (s *streamClient) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.msgs)
}

// waitFor waits until a message at index from or later matches, or the
// timeout elapses. It returns the time waited and whether a match was found.
func (s *streamClient) waitFor(ctx context.Context, from int, timeout time.Duration, match func(string) bool) (time.Duration, bool) {
	start := time.Now()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(streamPollInterval)
	defer tick.Stop()
	for {
		msgs := s.messages()
		for _, m := range msgs[min(from, len(msgs)):] {
			if match(m) {
				return time.Since(start), true
			}
		}
		select {
		case <-ctx.Done():
			return time.Since(start), false
		case <-deadline.C:
			return time.Since(start), false
		case <-tick.C:
		}
	}
}

// close ends the stream and waits for the reader to stop.
func (s *streamClient) close() {
	s.cancel()
	_ = s.conn.CloseNow()
	<-s.done
}

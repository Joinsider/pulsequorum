// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/auth"
	"github.com/joinsider/pulsequorum/internal/store"
	"github.com/joinsider/pulsequorum/internal/stream"
)

// Live stream (FA-API-04, FA-AUTH-11, FA-TEN-05). A browser fetches a
// one-time ticket with its bearer token and opens GET /api/v1/stream?ticket=…
// on the same node. The tenant of the subscription always comes from the
// ticket's principal; clients can only narrow the stream to monitor IDs.

const (
	streamPath = BaseURL + "/stream"
	// streamRevalidate is how often the user behind a stream is checked again
	// (disabled, deleted or without read access → closed).
	streamRevalidate = time.Minute
	streamPing       = 30 * time.Second
	streamWrite      = 10 * time.Second
	streamReadLimit  = 64 << 10
	// streamMaxMonitors bounds the monitor filter of one connection.
	streamMaxMonitors = 1000
)

// Close codes in the application range (RFC 6455 7.4.2).
const (
	closeSessionEnded websocket.StatusCode = 4001 // fetch a new token and ticket
	closeForbidden    websocket.StatusCode = 4003 // the user lost access
	closeTooSlow      websocket.StatusCode = 4008 // reconnect and reload
)

// CreateStreamTicket issues a ticket for the caller (FA-AUTH-11).
func (h *Handlers) CreateStreamTicket(ctx context.Context, _ gen.CreateStreamTicketRequestObject) (gen.CreateStreamTicketResponseObject, error) {
	p, err := principal(ctx, readAccess)
	if err != nil {
		return nil, err
	}
	if h.deps.Tickets == nil || h.deps.Stream == nil {
		return nil, &Problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable", Detail: "live stream is not available on this node"}
	}
	token, expires, err := h.deps.Tickets.Issue(p, p.ExpiresAt)
	if errors.Is(err, auth.ErrTooManyTickets) {
		return nil, &Problem{Status: http.StatusTooManyRequests, Title: "Too Many Requests", Detail: err.Error(), RetryAfter: int(auth.TicketTTL.Seconds())}
	}
	if err != nil {
		return nil, err
	}
	return gen.CreateStreamTicket201JSONResponse{Ticket: token, ExpiresAt: expires.UTC()}, nil
}

// streamFilter is the only message a client sends: it replaces the monitor
// filter (empty = all monitors of the tenant).
type streamFilter struct {
	MonitorIDs []string `json:"monitorIds"`
}

func parseMonitorIDs(ids []string) ([]string, error) {
	if len(ids) > streamMaxMonitors {
		return nil, errors.New("too many monitor IDs")
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, errors.New("monitor IDs must be UUIDs")
		}
		out = append(out, u.String())
	}
	return out, nil
}

// allowedOrigin accepts same-host requests, configured frontend origins and
// non-browser clients without Origin. Comparison is exact, not a pattern.
func (h *Handlers) allowedOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	return slices.Contains(h.cfg.CORSOrigins, strings.ToLower(origin))
}

// revalidate checks that the tenant still exists and is not suspended, in
// every auth mode (ADR 0027, 0029). With local users it also checks that the
// user still exists, is enabled and may read. With OIDC there is no local
// user: the roles come from the token and hold until it expires, when the
// stream is closed.
func (h *Handlers) revalidate(ctx context.Context, p auth.Principal) bool {
	if h.deps.State == nil {
		return true
	}
	t, err := h.deps.State.GetTenant(ctx, p.TenantID)
	if err != nil || t.Suspended || t.ID == store.SystemTenantID {
		return false
	}
	if h.deps.Local == nil {
		return true
	}
	u, err := h.deps.State.GetUser(ctx, p.TenantID, p.UserID)
	if err != nil || u.Disabled {
		return false
	}
	return auth.Principal{TenantID: u.TenantID, Roles: []auth.Role{auth.Role(u.Role)}}.CanRead()
}

// serveStream upgrades to a WebSocket and forwards the tenant's events.
func (h *Handlers) serveStream(w http.ResponseWriter, r *http.Request) {
	conn, ticket, filter, ok := h.acceptStream(w, r)
	if !ok {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	sub := h.deps.Stream.Subscribe(ticket.Principal.TenantID)
	defer sub.Close()
	sub.SetMonitors(filter)

	// The request context is not cancelled by http.Server.Shutdown for
	// hijacked connections; StreamClosing is.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	readErr := readStreamFilters(ctx, conn, sub)
	h.runStream(ctx, conn, sub, ticket, readErr)
}

func (h *Handlers) acceptStream(w http.ResponseWriter, r *http.Request) (*websocket.Conn, auth.Ticket, []string, bool) {
	if h.deps.Tickets == nil || h.deps.Stream == nil {
		WriteProblem(w, http.StatusServiceUnavailable, problemTitleServiceUnavailable, "live stream is not available on this node")
		return nil, auth.Ticket{}, nil, false
	}
	if !h.allowedOrigin(r) {
		WriteProblem(w, http.StatusForbidden, "Forbidden", "origin is not allowed")
		return nil, auth.Ticket{}, nil, false
	}
	ticket, err := h.deps.Tickets.Redeem(r.URL.Query().Get("ticket"))
	if err != nil {
		WriteProblem(w, http.StatusUnauthorized, "Unauthorized", err.Error())
		return nil, auth.Ticket{}, nil, false
	}
	filter, err := parseMonitorIDs(r.URL.Query()["monitorId"])
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, problemTitleBadRequest, err.Error())
		return nil, auth.Ticket{}, nil, false
	}

	// The server's read and write timeouts must not end a long-lived stream.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// The origin was checked above, exactly instead of by host pattern.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, auth.Ticket{}, nil, false
	}
	conn.SetReadLimit(streamReadLimit)
	return conn, ticket, filter, true
}

func readStreamFilters(ctx context.Context, conn *websocket.Conn, sub *stream.Subscription) <-chan error {
	// Reader: filter updates; any read error ends the stream.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if err := handleStreamFilterMessage(data, sub); err != nil {
				readErr <- err
				return
			}
		}
	}()
	return readErr
}

func handleStreamFilterMessage(data []byte, sub *stream.Subscription) error {
	var f streamFilter
	if err := json.Unmarshal(data, &f); err != nil {
		return errors.New("invalid filter message")
	}
	ids, err := parseMonitorIDs(f.MonitorIDs)
	if err != nil {
		return err
	}
	sub.SetMonitors(ids)
	return nil
}

func (h *Handlers) runStream(ctx context.Context, conn *websocket.Conn, sub *stream.Subscription, ticket auth.Ticket, readErr <-chan error) {
	sessionEnd, stopSession := sessionEndTimer(ticket.SessionEnd, h.deps.Now)
	defer stopSession()

	revalidate := time.NewTicker(streamRevalidate)
	defer revalidate.Stop()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()

	for {
		select {
		case data, ok := <-sub.Events():
			if !forwardStreamEvent(ctx, conn, sub, data, ok) {
				return
			}
		case err := <-readErr:
			closeOnStreamReadError(ctx, conn, err)
			return
		case <-ping.C:
			if !pingStream(ctx, conn) {
				return
			}
		case <-revalidate.C:
			if !h.revalidate(ctx, ticket.Principal) {
				_ = conn.Close(closeForbidden, "access revoked")
				return
			}
		case <-sessionEnd:
			_ = conn.Close(closeSessionEnded, "session expired")
			return
		case <-h.deps.StreamClosing:
			_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
			return
		case <-ctx.Done():
			return
		}
	}
}

func sessionEndTimer(sessionEnd time.Time, now func() time.Time) (<-chan time.Time, func()) {
	if sessionEnd.IsZero() {
		return nil, func() { /* no timer to stop when session has no end time */ }
	}
	t := time.NewTimer(sessionEnd.Sub(now()))
	return t.C, func() { t.Stop() }
}

func forwardStreamEvent(ctx context.Context, conn *websocket.Conn, sub *stream.Subscription, data []byte, ok bool) bool {
	if !ok {
		if sub.Overflowed() {
			_ = conn.Close(closeTooSlow, "client too slow, reconnect")
		}
		return false
	}
	wctx, wcancel := context.WithTimeout(ctx, streamWrite)
	err := conn.Write(wctx, websocket.MessageText, data)
	wcancel()
	return err == nil
}

func pingStream(ctx context.Context, conn *websocket.Conn) bool {
	pctx, pcancel := context.WithTimeout(ctx, streamWrite)
	err := conn.Ping(pctx)
	pcancel()
	return err == nil
}

func closeOnStreamReadError(ctx context.Context, conn *websocket.Conn, err error) {
	if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid message")
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

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

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
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

// revalidate checks that the user still exists, is enabled and may read.
func (h *Handlers) revalidate(ctx context.Context, p auth.Principal) bool {
	if h.deps.State == nil || h.deps.Local == nil {
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
	if h.deps.Tickets == nil || h.deps.Stream == nil {
		WriteProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "live stream is not available on this node")
		return
	}
	if !h.allowedOrigin(r) {
		WriteProblem(w, http.StatusForbidden, "Forbidden", "origin is not allowed")
		return
	}
	ticket, err := h.deps.Tickets.Redeem(r.URL.Query().Get("ticket"))
	if err != nil {
		WriteProblem(w, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	p := ticket.Principal
	filter, err := parseMonitorIDs(r.URL.Query()["monitorId"])
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}

	// The server's read and write timeouts must not end a long-lived stream.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// The origin was checked above, exactly instead of by host pattern.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(streamReadLimit)

	sub := h.deps.Stream.Subscribe(p.TenantID)
	defer sub.Close()
	sub.SetMonitors(filter)

	// The request context is not cancelled by http.Server.Shutdown for
	// hijacked connections; StreamClosing is.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reader: filter updates; any read error ends the stream.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var f streamFilter
			if err := json.Unmarshal(data, &f); err != nil {
				readErr <- errors.New("invalid filter message")
				return
			}
			ids, err := parseMonitorIDs(f.MonitorIDs)
			if err != nil {
				readErr <- err
				return
			}
			sub.SetMonitors(ids)
		}
	}()

	var sessionEnd <-chan time.Time
	if !ticket.SessionEnd.IsZero() {
		t := time.NewTimer(time.Until(ticket.SessionEnd))
		defer t.Stop()
		sessionEnd = t.C
	}
	revalidate := time.NewTicker(streamRevalidate)
	defer revalidate.Stop()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()

	for {
		select {
		case data, ok := <-sub.Events():
			if !ok {
				if sub.Overflowed() {
					_ = conn.Close(closeTooSlow, "client too slow, reconnect")
				}
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, streamWrite)
			err := conn.Write(wctx, websocket.MessageText, data)
			wcancel()
			if err != nil {
				return
			}
		case err := <-readErr:
			if websocket.CloseStatus(err) == -1 && ctx.Err() == nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "invalid message")
			}
			return
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, streamWrite)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case <-revalidate.C:
			if !h.revalidate(ctx, p) {
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

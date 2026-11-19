// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package ucapi is a minimal client for the UptimeCluster REST API
// (api/openapi.yaml). It covers the operations the acceptance and chaos
// drivers need. Secrets (access token, password, join tokens) are never
// included in errors.
package ucapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIBasePath is the REST prefix of a node (api/openapi.yaml, servers.url).
const APIBasePath = "/api/v1"

// maxResponseBody bounds how much of one response is read.
const maxResponseBody = 4 << 20

// Client talks to one node. Token is the access token; when it is empty and a
// password is known, the client logs in before the first authenticated call
// and again after a 401 (the access token is short-lived).
type Client struct {
	// Base is the node base URL without trailing slash, e.g. http://127.0.0.1:8080.
	Base     string
	HTTP     *http.Client
	Token    string
	User     string
	Password string
}

// APIError is a non-2xx answer. Detail is the problem detail, truncated.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
}

// New returns a client for the node at base. The HTTP timeout is 15 s.
func New(base, user, password string) *Client {
	return &Client{
		Base:     strings.TrimRight(base, "/"),
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		User:     user,
		Password: password,
	}
}

// Wire types. Only the fields the drivers read or compare are declared.

// TokenResponse is the answer of setup and login.
type TokenResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Health is the answer of GET /health.
type Health struct {
	Status   string `json:"status"`
	Role     string `json:"role"`
	NodeName string `json:"nodeName"`
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	// Degraded is true while the voter has known no leader for DEGRADED_AFTER (ADR 0024).
	Degraded bool `json:"degraded"`
	// Partition holds the node IDs of this voter and the reachable voters without a leader.
	Partition []string `json:"partition"`
}

// User is a local user of the tenant.
type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

// NotificationChannel is a notification channel (secrets masked by the node).
type NotificationChannel struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	MaskedURL string    `json:"maskedUrl"`
	Webhook   any       `json:"webhook,omitempty"`
	Events    []string  `json:"events"`
	IsDefault bool      `json:"isDefault"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
}

// DeliveryResult is the answer of a synchronous channel test.
type DeliveryResult struct {
	Delivered  bool   `json:"delivered"`
	Error      string `json:"error"`
	DurationMs int    `json:"durationMs"`
}

// Assignment decides which nodes run a monitor's checks (api/openapi.yaml).
type Assignment struct {
	Mode  string   `json:"mode"`
	Count int      `json:"count,omitempty"`
	Nodes []string `json:"nodes,omitempty"`
}

// MonitorInput is the writable part of a monitor (MonitorInput in the API).
type MonitorInput struct {
	Name                   string         `json:"name"`
	Description            string         `json:"description,omitempty"`
	Type                   string         `json:"type"`
	Config                 map[string]any `json:"config"`
	IntervalSeconds        int            `json:"intervalSeconds"`
	TimeoutSeconds         int            `json:"timeoutSeconds,omitempty"`
	Retries                int            `json:"retries"`
	RecoveryThreshold      int            `json:"recoveryThreshold,omitempty"`
	ResendIntervalMinutes  int            `json:"resendIntervalMinutes"`
	Assignment             *Assignment    `json:"assignment,omitempty"`
	ConsensusPolicy        string         `json:"consensusPolicy,omitempty"`
	Tags                   []string       `json:"tags,omitempty"`
	NotificationChannelIDs []string       `json:"notificationChannelIds"`
	Paused                 bool           `json:"paused"`
}

// Monitor is a monitor including its consensus status.
type Monitor struct {
	MonitorInput
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	StatusSince time.Time  `json:"statusSince"`
	LastCheckAt *time.Time `json:"lastCheckAt"`
	LatencyMs   *int       `json:"lastLatencyMs"`
	Uptime24h   *float64   `json:"uptime24h"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// MonitorPage is one page of monitors.
type MonitorPage struct {
	Items      []Monitor `json:"items"`
	NextCursor *string   `json:"nextCursor"`
}

// Heartbeat is the result of one check on one node.
type Heartbeat struct {
	NodeID     string    `json:"nodeId"`
	TS         time.Time `json:"ts"`
	Status     string    `json:"status"`
	LatencyMs  int       `json:"latencyMs"`
	ErrorClass string    `json:"errorClass"`
	Message    string    `json:"message"`
}

// HeartbeatPage is one page of heartbeats.
type HeartbeatPage struct {
	Items      []Heartbeat `json:"items"`
	NextCursor *string     `json:"nextCursor"`
}

// HeartbeatQuery selects heartbeats of one monitor. A zero From or To leaves
// that side open; an empty Node means the consensus view.
type HeartbeatQuery struct {
	From time.Time
	To   time.Time
	Node string
}

// Incident is an outage of one monitor.
type Incident struct {
	ID          string     `json:"id"`
	MonitorID   string     `json:"monitorId"`
	MonitorName string     `json:"monitorName"`
	StartedAt   time.Time  `json:"startedAt"`
	ResolvedAt  *time.Time `json:"resolvedAt"`
	Cause       string     `json:"cause"`
	ErrorClass  string     `json:"errorClass"`
	Confirmed   bool       `json:"confirmed"`
}

// IncidentPage is one page of incidents.
type IncidentPage struct {
	Items      []Incident `json:"items"`
	NextCursor *string    `json:"nextCursor"`
}

// UptimeStats is the answer of GET /monitors/{id}/uptime.
type UptimeStats struct {
	UptimePercent *float64 `json:"uptimePercent"`
}

// MaintenanceWindow is a maintenance window (MaintenanceWindow in the API).
type MaintenanceWindow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	MonitorIDs  []string  `json:"monitorIds"`
	Schedule    any       `json:"schedule"`
	Active      bool      `json:"active"`
	NextStart   any       `json:"nextStart,omitempty"`
	InProgress  bool      `json:"inProgress,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// JoinToken is the one-time token of a new node. Token is a secret.
type JoinToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"role"`
	TenantID  string    `json:"tenantId"`
}

// ClusterNode is one member of the Raft configuration as seen by the node
// that answered the request.
type ClusterNode struct {
	ID                string   `json:"id"`
	Address           string   `json:"address"`
	Voter             bool     `json:"voter"`
	Leader            bool     `json:"leader"`
	Self              bool     `json:"self"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Capabilities      []string `json:"capabilities"`
	Healthy           *bool    `json:"healthy"`
	ReplicationLagMs  *int64   `json:"replicationLagMs"`
	ReplicationError  string   `json:"replicationError"`
	MaxCommandVersion int      `json:"maxCommandVersion"`
	// ConnectivityImpaired is the leader's decision on the node's connectivity (ADR 0022).
	ConnectivityImpaired bool   `json:"connectivityImpaired"`
	ConnectivityReason   string `json:"connectivityReason"`
}

// SystemAlert is one system alert (ADR 0025). Details hold measured values and node IDs only.
type SystemAlert struct {
	Kind    string         `json:"kind"`
	Subject string         `json:"subject"`
	Active  bool           `json:"active"`
	Level   int            `json:"level"`
	Since   time.Time      `json:"since"`
	Updated time.Time      `json:"updated"`
	Details map[string]any `json:"details"`
}

// ClusterNodeList is the answer of GET /admin/nodes.
type ClusterNodeList struct {
	Nodes          []ClusterNode `json:"nodes"`
	LeaderID       string        `json:"leaderId"`
	Warnings       []string      `json:"warnings"`
	CommandLevel   int           `json:"commandLevel"`
	UpgradePending bool          `json:"upgradePending"`
}

// NodeRemoval is the answer of DELETE /admin/nodes/{id}.
type NodeRemoval struct {
	VotersLeft int      `json:"votersLeft"`
	Warnings   []string `json:"warnings"`
}

// Do sends one request. method and path are relative to APIBasePath. On 401
// it logs in again once when a password is known and retries. out may be nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	status, _, err := c.DoRaw(ctx, method, path, query, body, out)
	return status, err
}

// DoRaw is Do that also returns the raw body of the answer, error answers
// included. The body is nil when it could not be read. It can hold secrets
// (for example a token creation answer), so callers may inspect it but must
// never print or log it.
func (c *Client) DoRaw(ctx context.Context, method, path string, query url.Values, body, out any) (int, []byte, error) {
	if c.Token == "" && c.Password != "" && needsAuth(path) {
		if err := c.Login(ctx); err != nil {
			return 0, nil, fmt.Errorf("login before %s %s: %w", method, path, err)
		}
	}
	var raw []byte
	status, err := c.send(ctx, method, path, query, body, out, &raw)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && c.Password != "" && path != "/auth/login" {
		if lerr := c.Login(ctx); lerr != nil {
			return status, raw, fmt.Errorf("re-login after 401 failed: %w", lerr)
		}
		raw = nil
		status, err = c.send(ctx, method, path, query, body, out, &raw)
	}
	return status, raw, err
}

// needsAuth reports whether a path requires a logged-in user.
func needsAuth(path string) bool {
	switch path {
	case "/health", "/auth/setup", "/auth/login":
		return false
	}
	return true
}

// send performs one HTTP exchange. When rawOut is not nil it receives the
// response body as read, error answers included.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body, out any, rawOut *[]byte) (int, error) {
	u := c.Base + APIBasePath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" && needsAuth(path) {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if rawOut != nil {
		*rawOut = raw
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &APIError{Status: resp.StatusCode, Detail: problemDetail(raw)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// problemDetail extracts the "detail" or "title" of an RFC 9457 problem body.
func problemDetail(raw []byte) string {
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	s := p.Detail
	if s == "" {
		s = p.Title
	}
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// Health checks GET /health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	_, err := c.Do(ctx, http.MethodGet, "/health", nil, nil, &h)
	return h, err
}

// Setup creates the first admin with the one-time setup token (FA-AUTH-01).
// On success the returned access token is kept for later calls.
func (c *Client) Setup(ctx context.Context, setupToken, displayName string) error {
	var tr TokenResponse
	body := map[string]string{
		"setupToken":  setupToken,
		"username":    c.User,
		"displayName": displayName,
		"password":    c.Password,
	}
	if _, err := c.send(ctx, http.MethodPost, "/auth/setup", nil, body, &tr, nil); err != nil {
		return err
	}
	c.Token = tr.AccessToken
	return nil
}

// Login authenticates with the configured user and password.
func (c *Client) Login(ctx context.Context) error {
	var tr TokenResponse
	body := map[string]string{"username": c.User, "password": c.Password}
	if _, err := c.send(ctx, http.MethodPost, "/auth/login", nil, body, &tr, nil); err != nil {
		return err
	}
	c.Token = tr.AccessToken
	return nil
}

// ListUsers lists the users of the tenant.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	_, err := c.Do(ctx, http.MethodGet, "/users", nil, nil, &users)
	return users, err
}

// CreateChannel creates a shoutrrr notification channel that is attached to
// new monitors by default.
func (c *Client) CreateChannel(ctx context.Context, name, shoutrrrURL string, events []string) (NotificationChannel, error) {
	var ch NotificationChannel
	body := map[string]any{
		"name":      name,
		"kind":      "shoutrrr",
		"url":       shoutrrrURL,
		"events":    events,
		"isDefault": true,
	}
	_, err := c.Do(ctx, http.MethodPost, "/notifications", nil, body, &ch)
	return ch, err
}

// ListChannels lists the notification channels (secrets masked).
func (c *Client) ListChannels(ctx context.Context) ([]NotificationChannel, error) {
	var chs []NotificationChannel
	_, err := c.Do(ctx, http.MethodGet, "/notifications", nil, nil, &chs)
	return chs, err
}

// TestChannel sends the channel's test message synchronously.
func (c *Client) TestChannel(ctx context.Context, channelID string) (DeliveryResult, error) {
	var res DeliveryResult
	_, err := c.Do(ctx, http.MethodPost, "/notifications/"+url.PathEscape(channelID)+"/test", nil, nil, &res)
	return res, err
}

// CreateMonitor creates an HTTP monitor with the given interval and retries.
func (c *Client) CreateMonitor(ctx context.Context, name, targetURL string, intervalSeconds, retries int, channelIDs []string) (Monitor, error) {
	return c.CreateMonitorInput(ctx, MonitorInput{
		Name:                   name,
		Type:                   "http",
		Config:                 map[string]any{"url": targetURL},
		IntervalSeconds:        intervalSeconds,
		Retries:                retries,
		NotificationChannelIDs: channelIDs,
	})
}

// CreateMonitorInput creates a monitor from a complete input.
func (c *Client) CreateMonitorInput(ctx context.Context, in MonitorInput) (Monitor, error) {
	var m Monitor
	_, err := c.Do(ctx, http.MethodPost, "/monitors", nil, in, &m)
	return m, err
}

// GetMonitor reads one monitor including its consensus status.
func (c *Client) GetMonitor(ctx context.Context, id string) (Monitor, error) {
	var m Monitor
	_, err := c.Do(ctx, http.MethodGet, "/monitors/"+url.PathEscape(id), nil, nil, &m)
	return m, err
}

// ListMonitors lists all monitors, following the cursor until the end.
func (c *Client) ListMonitors(ctx context.Context) ([]Monitor, error) {
	var all []Monitor
	cursor := ""
	for pages := 0; pages < 1000; pages++ {
		q := url.Values{"limit": {"500"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page MonitorPage
		if _, err := c.Do(ctx, http.MethodGet, "/monitors", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		cursor = *page.NextCursor
	}
	return nil, errors.New("monitor list does not end")
}

// ListHeartbeats lists the heartbeats of one monitor in the query window,
// following the cursor until the end.
func (c *Client) ListHeartbeats(ctx context.Context, monitorID string, hq HeartbeatQuery) ([]Heartbeat, error) {
	var all []Heartbeat
	cursor := ""
	for pages := 0; pages < 10000; pages++ {
		q := url.Values{"limit": {"5000"}}
		if !hq.From.IsZero() {
			q.Set("from", hq.From.UTC().Format(time.RFC3339Nano))
		}
		if !hq.To.IsZero() {
			q.Set("to", hq.To.UTC().Format(time.RFC3339Nano))
		}
		if hq.Node != "" {
			q.Set("node", hq.Node)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page HeartbeatPage
		if _, err := c.Do(ctx, http.MethodGet, "/monitors/"+url.PathEscape(monitorID)+"/heartbeats", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		cursor = *page.NextCursor
	}
	return nil, errors.New("heartbeat list does not end")
}

// ListIncidents lists incidents, optionally only those of one monitor and
// only unresolved ones.
func (c *Client) ListIncidents(ctx context.Context, monitorID string, openOnly bool) ([]Incident, error) {
	q := url.Values{}
	if monitorID != "" {
		q.Set("monitorId", monitorID)
	}
	if openOnly {
		q.Set("open", "true")
	}
	var page IncidentPage
	_, err := c.Do(ctx, http.MethodGet, "/incidents", q, nil, &page)
	return page.Items, err
}

// OpenIncidents lists unresolved incidents of one monitor.
func (c *Client) OpenIncidents(ctx context.Context, monitorID string) ([]Incident, error) {
	return c.ListIncidents(ctx, monitorID, true)
}

// GetIncident reads one incident.
func (c *Client) GetIncident(ctx context.Context, id string) (Incident, error) {
	var inc Incident
	_, err := c.Do(ctx, http.MethodGet, "/incidents/"+url.PathEscape(id), nil, nil, &inc)
	return inc, err
}

// Uptime24h returns the 24 h uptime percentage of a monitor, nil if no data.
func (c *Client) Uptime24h(ctx context.Context, monitorID string) (*float64, error) {
	var st UptimeStats
	q := url.Values{"range": {"24h"}}
	_, err := c.Do(ctx, http.MethodGet, "/monitors/"+url.PathEscape(monitorID)+"/uptime", q, nil, &st)
	return st.UptimePercent, err
}

// ListMaintenance lists the maintenance windows.
func (c *Client) ListMaintenance(ctx context.Context) ([]MaintenanceWindow, error) {
	var ws []MaintenanceWindow
	_, err := c.Do(ctx, http.MethodGet, "/maintenance", nil, nil, &ws)
	return ws, err
}

// ListClusterNodes reads the cluster members (platform admins only).
func (c *Client) ListClusterNodes(ctx context.Context) (ClusterNodeList, error) {
	var list ClusterNodeList
	_, err := c.Do(ctx, http.MethodGet, "/admin/nodes", nil, nil, &list)
	return list, err
}

// CreateSystemChannel creates a system notification channel (ADR 0025). The
// node forces the tenant to _system and adds the event system. Platform admins only.
func (c *Client) CreateSystemChannel(ctx context.Context, name, shoutrrrURL string, events []string) (NotificationChannel, error) {
	var ch NotificationChannel
	body := map[string]any{
		"name":   name,
		"kind":   "shoutrrr",
		"url":    shoutrrrURL,
		"events": events,
	}
	_, err := c.Do(ctx, http.MethodPost, "/admin/system-channels", nil, body, &ch)
	return ch, err
}

// TestSystemChannel sends the test message of a system channel synchronously.
func (c *Client) TestSystemChannel(ctx context.Context, channelID string) (DeliveryResult, error) {
	var res DeliveryResult
	_, err := c.Do(ctx, http.MethodPost, "/admin/system-channels/"+url.PathEscape(channelID)+"/test", nil, nil, &res)
	return res, err
}

// ListSystemAlerts lists the active system alerts, or with includeCleared
// also the cleared ones (query active=false).
func (c *Client) ListSystemAlerts(ctx context.Context, includeCleared bool) ([]SystemAlert, error) {
	var out []SystemAlert
	var q url.Values
	if includeCleared {
		q = url.Values{"active": {"false"}}
	}
	_, err := c.Do(ctx, http.MethodGet, "/admin/system-alerts", q, nil, &out)
	return out, err
}

// CreateJoinToken creates a one-time join token for a new node of the given
// role ("voter" or "probe") that is valid for ttlMinutes.
func (c *Client) CreateJoinToken(ctx context.Context, role string, ttlMinutes int) (JoinToken, error) {
	var tok JoinToken
	body := map[string]any{"role": role, "ttlMinutes": ttlMinutes}
	_, err := c.Do(ctx, http.MethodPost, "/admin/join-tokens", nil, body, &tok)
	return tok, err
}

// RemoveClusterNode removes a member and revokes its certificates.
func (c *Client) RemoveClusterNode(ctx context.Context, nodeID string) (NodeRemoval, error) {
	var res NodeRemoval
	_, err := c.Do(ctx, http.MethodDelete, "/admin/nodes/"+url.PathEscape(nodeID), nil, nil, &res)
	return res, err
}

// StatusCode returns the HTTP status of an error from this package, or 0.
func StatusCode(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// UpdateMonitor replaces the writable part of a monitor (PUT /monitors/{id}).
func (c *Client) UpdateMonitor(ctx context.Context, id string, in MonitorInput) (Monitor, error) {
	var m Monitor
	_, err := c.Do(ctx, http.MethodPut, "/monitors/"+url.PathEscape(id), nil, in, &m)
	return m, err
}

// DeleteMonitor deletes a monitor (DELETE /monitors/{id}, 204).
func (c *Client) DeleteMonitor(ctx context.Context, id string) error {
	_, err := c.Do(ctx, http.MethodDelete, "/monitors/"+url.PathEscape(id), nil, nil, nil)
	return err
}

// MaintenanceWindowInput is the writable part of a maintenance window.
type MaintenanceWindowInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	MonitorIDs  []string `json:"monitorIds"`
	Schedule    any      `json:"schedule"`
	Active      bool     `json:"active"`
}

// CreateMaintenance creates a maintenance window (POST /maintenance).
func (c *Client) CreateMaintenance(ctx context.Context, in MaintenanceWindowInput) (MaintenanceWindow, error) {
	var w MaintenanceWindow
	_, err := c.Do(ctx, http.MethodPost, "/maintenance", nil, in, &w)
	return w, err
}

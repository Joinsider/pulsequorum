// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

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

// apiBasePath is the REST prefix of the node (api/openapi.yaml, servers.url).
const apiBasePath = "/api/v1"

// apiClient is a minimal client for the parts of the REST API that AS-01
// uses. Secrets (access token, password) are never included in errors.
type apiClient struct {
	base     string // node base URL without trailing slash
	http     *http.Client
	token    string
	user     string
	password string
}

// apiError is a non-2xx answer. Detail is the Problem detail, truncated.
type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
}

func newAPIClient(base, user, password string) *apiClient {
	return &apiClient{
		base:     strings.TrimRight(base, "/"),
		http:     &http.Client{Timeout: 15 * time.Second},
		user:     user,
		password: password,
	}
}

// Wire types. Only the fields AS-01 reads are declared.

type tokenResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type notificationChannel struct {
	ID string `json:"id"`
}

type deliveryResult struct {
	Delivered  bool   `json:"delivered"`
	Error      string `json:"error"`
	DurationMs int    `json:"durationMs"`
}

type monitor struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type incident struct {
	ID         string     `json:"id"`
	MonitorID  string     `json:"monitorId"`
	StartedAt  time.Time  `json:"startedAt"`
	ResolvedAt *time.Time `json:"resolvedAt"`
	Cause      string     `json:"cause"`
	Confirmed  bool       `json:"confirmed"`
}

type incidentPage struct {
	Items []incident `json:"items"`
}

type uptimeStats struct {
	UptimePercent *float64 `json:"uptimePercent"`
}

// do sends one request. On 401 it logs in again once when a password is known
// (the access token is short-lived) and retries.
func (c *apiClient) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	status, err := c.send(ctx, method, path, query, body, out)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && c.password != "" && path != "/auth/login" {
		if lerr := c.Login(ctx); lerr != nil {
			return status, fmt.Errorf("re-login after 401 failed: %w", lerr)
		}
		return c.send(ctx, method, path, query, body, out)
	}
	return status, err
}

func (c *apiClient) send(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	u := c.base + apiBasePath + path
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
	if c.token != "" && path != "/auth/setup" && path != "/auth/login" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, &apiError{Status: resp.StatusCode, Detail: problemDetail(raw)}
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
func (c *apiClient) Health(ctx context.Context) error {
	_, err := c.send(ctx, http.MethodGet, "/health", nil, nil, nil)
	return err
}

// Setup creates the first admin with the one-time setup token (FA-AUTH-01).
// On success the returned access token is kept for later calls.
func (c *apiClient) Setup(ctx context.Context, setupToken string) error {
	var tr tokenResponse
	body := map[string]string{
		"setupToken":  setupToken,
		"username":    c.user,
		"displayName": "AS-01 acceptance",
		"password":    c.password,
	}
	if _, err := c.send(ctx, http.MethodPost, "/auth/setup", nil, body, &tr); err != nil {
		return err
	}
	c.token = tr.AccessToken
	return nil
}

// Login authenticates with the configured user and password.
func (c *apiClient) Login(ctx context.Context) error {
	var tr tokenResponse
	body := map[string]string{"username": c.user, "password": c.password}
	if _, err := c.send(ctx, http.MethodPost, "/auth/login", nil, body, &tr); err != nil {
		return err
	}
	c.token = tr.AccessToken
	return nil
}

// CreateChannel creates a shoutrrr notification channel that is attached to
// new monitors by default.
func (c *apiClient) CreateChannel(ctx context.Context, name, shoutrrrURL string, events []string) (notificationChannel, error) {
	var ch notificationChannel
	body := map[string]any{
		"name":      name,
		"kind":      "shoutrrr",
		"url":       shoutrrrURL,
		"events":    events,
		"isDefault": true,
	}
	_, err := c.do(ctx, http.MethodPost, "/notifications", nil, body, &ch)
	return ch, err
}

// TestChannel sends the channel's test message synchronously.
func (c *apiClient) TestChannel(ctx context.Context, channelID string) (deliveryResult, error) {
	var res deliveryResult
	_, err := c.do(ctx, http.MethodPost, "/notifications/"+url.PathEscape(channelID)+"/test", nil, nil, &res)
	return res, err
}

// CreateMonitor creates an HTTP monitor with the given interval and retries.
func (c *apiClient) CreateMonitor(ctx context.Context, name, targetURL string, intervalSeconds, retries int, channelIDs []string) (monitor, error) {
	var m monitor
	body := map[string]any{
		"name":                   name,
		"type":                   "http",
		"config":                 map[string]any{"url": targetURL},
		"intervalSeconds":        intervalSeconds,
		"retries":                retries,
		"notificationChannelIds": channelIDs,
	}
	_, err := c.do(ctx, http.MethodPost, "/monitors", nil, body, &m)
	return m, err
}

// GetMonitor reads one monitor including its consensus status.
func (c *apiClient) GetMonitor(ctx context.Context, id string) (monitor, error) {
	var m monitor
	_, err := c.do(ctx, http.MethodGet, "/monitors/"+url.PathEscape(id), nil, nil, &m)
	return m, err
}

// OpenIncidents lists unresolved incidents of one monitor.
func (c *apiClient) OpenIncidents(ctx context.Context, monitorID string) ([]incident, error) {
	q := url.Values{"monitorId": {monitorID}, "open": {"true"}}
	var page incidentPage
	_, err := c.do(ctx, http.MethodGet, "/incidents", q, nil, &page)
	return page.Items, err
}

// GetIncident reads one incident.
func (c *apiClient) GetIncident(ctx context.Context, id string) (incident, error) {
	var inc incident
	_, err := c.do(ctx, http.MethodGet, "/incidents/"+url.PathEscape(id), nil, nil, &inc)
	return inc, err
}

// Uptime24h returns the 24 h uptime percentage of a monitor, nil if no data.
func (c *apiClient) Uptime24h(ctx context.Context, monitorID string) (*float64, error) {
	var st uptimeStats
	q := url.Values{"range": {"24h"}}
	_, err := c.do(ctx, http.MethodGet, "/monitors/"+url.PathEscape(monitorID)+"/uptime", q, nil, &st)
	return st.UptimePercent, err
}

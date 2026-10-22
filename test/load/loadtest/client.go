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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiPrefix         = "/api/v1"
	tokenMargin       = time.Minute
	maxResponseBytes  = 64 << 20
	listPageSize      = 500
	maxListPages      = 1000
	idempotencyHeader = "Idempotency-Key"
)

// apiError is a non-2xx answer of the voter.
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

func newAPIError(status int, body []byte) *apiError {
	var p struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	detail := ""
	if json.Unmarshal(body, &p) == nil {
		detail = p.Detail
		if detail == "" {
			detail = p.Title
		}
	}
	if detail == "" {
		detail = strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
	}
	return &apiError{Status: status, Detail: detail}
}

// apiClient talks to the REST API with a bearer token. The token is kept
// until shortly before its expiry; a 401 triggers one re-login with the
// configured user and password.
type apiClient struct {
	base     string
	user     string
	password string
	http     *http.Client

	loginMu sync.Mutex // serializes logins
	mu      sync.Mutex // guards the fields below
	token   string
	expires time.Time
	epoch   uint64 // incremented on every stored token
}

func newAPIClient(base, user, password string) (*apiClient, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q: want http://host:port", base)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 32
	return &apiClient{
		base:     strings.TrimRight(base, "/"),
		user:     user,
		password: password,
		http:     &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}, nil
}

type tokenResponse struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func (c *apiClient) storeToken(t tokenResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = t.AccessToken
	c.expires = t.ExpiresAt
	c.epoch++
}

// setup creates the first admin with the one-time setup token (FA-AUTH setup).
func (c *apiClient) setup(ctx context.Context, setupToken string) error {
	body := map[string]string{"setupToken": setupToken, "username": c.user, "password": c.password}
	return c.postToken(ctx, "/auth/setup", body)
}

func (c *apiClient) login(ctx context.Context) error {
	return c.postToken(ctx, "/auth/login", map[string]string{"username": c.user, "password": c.password})
}

// reLogin logs in even though the stored token may still look fresh. It is
// used after the stream was closed with 4001 "session expired".
func (c *apiClient) reLogin(ctx context.Context) error {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.login(ctx)
}

func (c *apiClient) postToken(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := c.send(ctx, http.MethodPost, path, payload, nil, "")
	if err != nil {
		return err
	}
	var t tokenResponse
	if err := json.Unmarshal(data, &t); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	if t.AccessToken == "" {
		return errors.New("token response without access token")
	}
	c.storeToken(t)
	return nil
}

// currentToken returns a valid token and the epoch it belongs to.
func (c *apiClient) currentToken(ctx context.Context) (string, uint64, error) {
	c.mu.Lock()
	tok, ep := c.token, c.epoch
	fresh := tok != "" && time.Until(c.expires) > tokenMargin
	c.mu.Unlock()
	if fresh {
		return tok, ep, nil
	}
	return c.refresh(ctx, ep)
}

// refresh logs in again unless another goroutine already replaced the token
// seen at epoch seen.
func (c *apiClient) refresh(ctx context.Context, seen uint64) (string, uint64, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	c.mu.Lock()
	if c.epoch != seen && c.token != "" && time.Until(c.expires) > tokenMargin {
		tok, ep := c.token, c.epoch
		c.mu.Unlock()
		return tok, ep, nil
	}
	c.mu.Unlock()
	if err := c.login(ctx); err != nil {
		return "", 0, fmt.Errorf("login as %q: %w", c.user, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, c.epoch, nil
}

// send performs one request. Non-2xx answers are returned as *apiError.
func (c *apiClient) send(ctx context.Context, method, path string, payload []byte, header http.Header, token string) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp.StatusCode, data)
	}
	return data, nil
}

// call sends an authenticated request. A 401 is retried once after a re-login.
func (c *apiClient) call(ctx context.Context, method, path string, in any, header http.Header) ([]byte, error) {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	tok, ep, err := c.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	data, err := c.send(ctx, method, path, payload, header, tok)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		tok, _, err = c.refresh(ctx, ep)
		if err != nil {
			return nil, err
		}
		data, err = c.send(ctx, method, path, payload, header, tok)
	}
	return data, err
}

// monitorRef is the part of a monitor the tools need.
type monitorRef struct {
	ID          string
	Name        string
	Interval    int
	TargetIndex int // index on the target farm, -1 if the URL is not a /t/{i} URL
}

// listMonitors loads all monitors of the tenant, following nextCursor.
func (c *apiClient) listMonitors(ctx context.Context) ([]monitorRef, error) {
	var out []monitorRef
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		path := fmt.Sprintf("/monitors?limit=%d", listPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		data, err := c.call(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return nil, err
		}
		var pg struct {
			Items []struct {
				ID              string         `json:"id"`
				Name            string         `json:"name"`
				IntervalSeconds int            `json:"intervalSeconds"`
				Config          map[string]any `json:"config"`
			} `json:"items"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &pg); err != nil {
			return nil, fmt.Errorf("decode monitor page: %w", err)
		}
		for _, it := range pg.Items {
			ref := monitorRef{ID: it.ID, Name: it.Name, Interval: it.IntervalSeconds, TargetIndex: -1}
			if raw, ok := it.Config["url"].(string); ok {
				if idx, ok := targetIndexFromURL(raw); ok {
					ref.TargetIndex = idx
				}
			}
			out = append(out, ref)
		}
		if pg.NextCursor == nil || *pg.NextCursor == "" {
			return out, nil
		}
		cursor = *pg.NextCursor
	}
	return nil, fmt.Errorf("monitor listing exceeds %d pages", maxListPages)
}

// targetIndexFromURL extracts i from a URL ending in /t/{i}.
func targetIndexFromURL(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	at := strings.LastIndex(u.Path, "/t/")
	if at < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(u.Path[at+len("/t/"):])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func (c *apiClient) uptime(ctx context.Context, monitorID, rng string) error {
	path := "/monitors/" + url.PathEscape(monitorID) + "/uptime?range=" + url.QueryEscape(rng)
	_, err := c.call(ctx, http.MethodGet, path, nil, nil)
	return err
}

func (c *apiClient) streamTicket(ctx context.Context) (string, error) {
	data, err := c.call(ctx, http.MethodPost, "/stream/ticket", nil, nil)
	if err != nil {
		return "", err
	}
	var t struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return "", fmt.Errorf("decode stream ticket: %w", err)
	}
	if t.Ticket == "" {
		return "", errors.New("empty stream ticket")
	}
	return t.Ticket, nil
}

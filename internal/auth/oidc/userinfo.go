// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/joinsider/uptimecluster/internal/auth"
)

// Limits of the userinfo lookup (ADR 0028).
const (
	userInfoTimeout   = 5 * time.Second
	userInfoCacheSize = 10_000
)

// errUserInfoRejected means the provider refused the access token.
var errUserInfoRejected = errors.New("the provider rejected the access token")

// UserInfoOptions configures UserInfo.
type UserInfoOptions struct {
	// Keys supplies the userinfo endpoint from discovery and the HTTP client
	// with its restrictions (no redirects, size limit).
	Keys *KeySet
	// OnCall and OnFailure count lookups and failed lookups (metrics).
	OnCall    func()
	OnFailure func()
	Now       func() time.Time
}

// UserInfo fetches the claims of an access token from the provider's
// userinfo endpoint and caches them per token until it expires (ADR 0028).
// It is only called for tokens that passed every other check.
type UserInfo struct {
	opts UserInfoOptions

	mu       sync.Mutex
	cache    map[[32]byte]userInfoEntry
	inflight map[[32]byte]*userInfoCall
}

type userInfoEntry struct {
	claims  map[string]json.RawMessage
	expires time.Time
}

type userInfoCall struct {
	done   chan struct{}
	claims map[string]json.RawMessage
	err    error
}

// NewUserInfo returns a userinfo client.
func NewUserInfo(opts UserInfoOptions) *UserInfo {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &UserInfo{opts: opts, cache: map[[32]byte]userInfoEntry{}, inflight: map[[32]byte]*userInfoCall{}}
}

// Claims returns the userinfo claims for token, whose verified subject is
// sub and which expires at expires. Errors wrap auth.ErrAuthUnavailable when
// the provider cannot be asked; other errors mean the token must be rejected.
func (u *UserInfo) Claims(ctx context.Context, token, sub string, expires time.Time) (map[string]json.RawMessage, error) {
	key := sha256.Sum256([]byte(token))
	now := u.opts.Now()

	u.mu.Lock()
	if e, ok := u.cache[key]; ok && now.Before(e.expires) {
		u.mu.Unlock()
		return e.claims, nil
	}
	if c, ok := u.inflight[key]; ok {
		u.mu.Unlock()
		select {
		case <-c.done:
			return c.claims, c.err
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", auth.ErrAuthUnavailable, ctx.Err())
		}
	}
	c := &userInfoCall{done: make(chan struct{})}
	u.inflight[key] = c
	u.mu.Unlock()

	c.claims, c.err = u.fetch(ctx, token, sub)
	u.mu.Lock()
	delete(u.inflight, key)
	if c.err == nil {
		u.store(key, userInfoEntry{claims: c.claims, expires: expires}, now)
	}
	u.mu.Unlock()
	close(c.done)
	return c.claims, c.err
}

// store adds an entry; mu is held. Expired entries are dropped first; if the
// cache is still full the entry is not cached (the next request asks again).
func (u *UserInfo) store(key [32]byte, e userInfoEntry, now time.Time) {
	if len(u.cache) >= userInfoCacheSize {
		for k, v := range u.cache {
			if !now.Before(v.expires) {
				delete(u.cache, k)
			}
		}
	}
	if len(u.cache) < userInfoCacheSize {
		u.cache[key] = e
	}
}

func (u *UserInfo) fetch(ctx context.Context, token, sub string) (map[string]json.RawMessage, error) {
	if u.opts.OnCall != nil {
		u.opts.OnCall()
	}
	claims, err := u.request(ctx, token, sub)
	if err != nil && u.opts.OnFailure != nil {
		u.opts.OnFailure()
	}
	return claims, err
}

func (u *UserInfo) request(ctx context.Context, token, sub string) (map[string]json.RawMessage, error) {
	ks := u.opts.Keys
	endpoint := ks.UserInfoEndpoint()
	if endpoint == "" {
		return nil, fmt.Errorf("%w: the provider announces no userinfo endpoint", auth.ErrAuthUnavailable)
	}
	if err := ks.checkURL(endpoint); err != nil {
		return nil, fmt.Errorf("%w: userinfo endpoint: %w", auth.ErrAuthUnavailable, err)
	}
	ctx, cancel := context.WithTimeout(ctx, userInfoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrAuthUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := ks.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrAuthUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, errUserInfoRejected
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: userinfo answered %d", auth.ErrAuthUnavailable, resp.StatusCode)
	}
	// Signed or encrypted userinfo responses (application/jwt) are not
	// supported; the response must be plain JSON.
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, fmt.Errorf("userinfo answered with content type %q, want application/json", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrAuthUnavailable, err)
	}
	if len(body) > maxDocumentSize {
		return nil, fmt.Errorf("userinfo answered with more than %d bytes", maxDocumentSize)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("userinfo: %w", err)
	}
	// The response must describe the token's subject (OIDC Core 5.3.2).
	var got string
	if err := json.Unmarshal(claims["sub"], &got); err != nil || got != sub {
		return nil, errors.New("userinfo subject does not match the token")
	}
	return claims, nil
}

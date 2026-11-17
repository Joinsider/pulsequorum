// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Limits and timings of the key set (ADR 0027).
const (
	maxDocumentSize   = 64 << 10
	minRSABits        = 2048
	defaultRefresh    = 5 * time.Minute
	defaultMinRefetch = 30 * time.Second
	defaultHTTPTimout = 10 * time.Second
	cacheFileMode     = 0o600
)

// Signature algorithms the verifier can be configured with (ADR 0027).
const (
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
	AlgEdDSA = "EdDSA"
)

// ErrNoKeys means the key set has never been loaded: no cache file and no
// successful fetch. Requests are answered with 503 until keys are present.
var ErrNoKeys = errors.New("identity provider keys are not available yet")

// errUnknownKey is returned when no published key matches the token.
var errUnknownKey = errors.New("no matching signing key")

// KeySetOptions configures a KeySet.
type KeySetOptions struct {
	// Issuer is the configured issuer, compared exactly with the discovery
	// document and the cache file.
	Issuer string
	// Algorithms is the allow-list of signature algorithms.
	Algorithms []string
	// CacheFile is where fetched keys are persisted; empty disables the file.
	CacheFile string
	// AllowHTTP permits http URLs for discovery and JWKS (development only).
	AllowHTTP bool
	// Client is the HTTP client; nil uses a client with timeouts. Redirects
	// are never followed, whatever the client says.
	Client *http.Client
	// RefreshInterval is the regular refresh period (default 5 min); a
	// shorter Cache-Control max-age of the JWKS response wins.
	RefreshInterval time.Duration
	// MinRefetch is the minimum time between two fetches triggered by an
	// unknown key ID (default 30 s).
	MinRefetch time.Duration
	// OnFetchFailure is called after every failed fetch (metrics).
	OnFetchFailure func()
	Now            func() time.Time
	Log            *slog.Logger
}

// KeySet holds the identity provider's signing keys. It implements the
// go-oidc KeySet interface: it verifies the signature only; header and claim
// checks are done by the verifier. Keys survive provider outages: a failed
// fetch keeps the last good set, and the set is persisted so a restart during
// an outage can still verify tokens (FA-AUTH-07).
type KeySet struct {
	opts   KeySetOptions
	client *http.Client
	algs   []jose.SignatureAlgorithm

	mu       sync.RWMutex
	keys     []jose.JSONWebKey
	jwksURI  string
	userinfo string // userinfo_endpoint from discovery, "" if none
	loaded   bool
	maxAge   time.Duration
	lastTry  time.Time // last fetch attempt, for the refetch rate limit
	lastGood time.Time

	// fetching serializes fetches; a waiter that gets it after another fetch
	// finds lastTry recent and does not fetch again (single flight).
	fetching chan struct{}
}

// cacheDocument is the persisted form of the key set.
type cacheDocument struct {
	Issuer    string          `json:"issuer"`
	JWKSURI   string          `json:"jwks_uri"`
	UserInfo  string          `json:"userinfo_endpoint,omitempty"`
	JWKS      json.RawMessage `json:"jwks"`
	FetchedAt time.Time       `json:"fetched_at"`
}

// NewKeySet returns an empty key set. Call Load before use.
func NewKeySet(opts KeySetOptions) (*KeySet, error) {
	if opts.Issuer == "" {
		return nil, errors.New("oidc key set: issuer is required")
	}
	algs, err := signatureAlgorithms(opts.Algorithms)
	if err != nil {
		return nil, err
	}
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = defaultRefresh
	}
	if opts.MinRefetch <= 0 {
		opts.MinRefetch = defaultMinRefetch
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	client := &http.Client{Timeout: defaultHTTPTimout}
	if opts.Client != nil {
		c := *opts.Client
		client = &c
		if client.Timeout == 0 {
			client.Timeout = defaultHTTPTimout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &KeySet{opts: opts, client: client, algs: algs, fetching: make(chan struct{}, 1)}, nil
}

func signatureAlgorithms(names []string) ([]jose.SignatureAlgorithm, error) {
	if len(names) == 0 {
		return nil, errors.New("oidc: at least one signature algorithm is required")
	}
	out := make([]jose.SignatureAlgorithm, 0, len(names))
	for _, n := range names {
		switch n {
		case AlgRS256, AlgES256, AlgEdDSA:
			out = append(out, jose.SignatureAlgorithm(n))
		default:
			return nil, fmt.Errorf("oidc: unsupported signature algorithm %q", n)
		}
	}
	return out, nil
}

// Load reads the cache file and then fetches the current keys. It fails only
// if neither produced keys; the caller may still start and retry later.
func (k *KeySet) Load(ctx context.Context) error {
	if err := k.readCache(); err != nil {
		k.opts.Log.Warn("oidc key cache not used", "file", k.opts.CacheFile, "err", err)
	}
	if err := k.fetch(ctx); err != nil {
		k.opts.Log.Warn("oidc key fetch failed", "issuer", k.opts.Issuer, "err", err)
		if !k.Ready() {
			return fmt.Errorf("%w: %w", ErrNoKeys, err)
		}
	}
	return nil
}

// Ready reports whether keys are available.
func (k *KeySet) Ready() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.loaded
}

// Run refreshes the keys until ctx is cancelled.
func (k *KeySet) Run(ctx context.Context) {
	for {
		timer := time.NewTimer(k.refreshInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := k.fetch(ctx); err != nil && ctx.Err() == nil {
			k.opts.Log.Warn("oidc key refresh failed, keeping the last keys", "issuer", k.opts.Issuer, "err", err)
		}
	}
}

func (k *KeySet) refreshInterval() time.Duration {
	k.mu.RLock()
	defer k.mu.RUnlock()
	d := k.opts.RefreshInterval
	if !k.loaded {
		// Without keys, retry at the refetch pace instead of waiting minutes.
		return k.opts.MinRefetch
	}
	if k.maxAge > 0 && k.maxAge < d {
		d = k.maxAge
	}
	return max(d, k.opts.MinRefetch)
}

// VerifySignature implements the go-oidc KeySet interface.
func (k *KeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSignedCompact(raw, k.algs)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("token must have exactly one signature")
	}
	hdr := jws.Signatures[0].Header
	if hdr.KeyID == "" {
		return nil, errors.New("token has no key ID")
	}
	if !k.Ready() {
		return nil, ErrNoKeys
	}
	key, ok := k.find(hdr.KeyID, hdr.Algorithm)
	if !ok {
		// Possibly a rotated key: refetch (FA-AUTH-07). fetch waits for a
		// running fetch and skips the request if the last one is more recent
		// than MinRefetch, so unknown key IDs cannot flood the provider.
		if err := k.fetch(ctx); err != nil {
			k.opts.Log.Warn("oidc key refetch for unknown key ID failed", "kid", hdr.KeyID, "err", err)
		}
		if key, ok = k.find(hdr.KeyID, hdr.Algorithm); !ok {
			return nil, errUnknownKey
		}
	}
	payload, err := jws.Verify(key.Key)
	if err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}
	return payload, nil
}

// find returns the key with kid that may verify alg.
func (k *KeySet) find(kid, alg string) (jose.JSONWebKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	for _, key := range k.keys {
		if key.KeyID == kid && keyMatchesAlg(key, alg) {
			return key, true
		}
	}
	return jose.JSONWebKey{}, false
}

// keyMatchesAlg reports whether key may verify signatures of alg: the key
// type must fit, and a key that names its algorithm must name this one.
func keyMatchesAlg(key jose.JSONWebKey, alg string) bool {
	if key.Algorithm != "" && key.Algorithm != alg {
		return false
	}
	switch pub := key.Key.(type) {
	case *rsa.PublicKey:
		return alg == AlgRS256
	case *ecdsa.PublicKey:
		return alg == AlgES256 && pub.Curve == elliptic.P256()
	case ed25519.PublicKey:
		return alg == AlgEdDSA
	}
	return false
}

// fetch loads discovery (once) and the JWKS. Concurrent callers wait for the
// running fetch and then return without fetching again if it was recent.
func (k *KeySet) fetch(ctx context.Context) error {
	select {
	case k.fetching <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-k.fetching }()

	started := k.opts.Now()
	k.mu.Lock()
	recent := !k.lastTry.IsZero() && started.Sub(k.lastTry) < k.opts.MinRefetch && k.loaded
	k.mu.Unlock()
	if recent {
		// Another caller fetched while we waited, or a refresh just ran.
		return nil
	}
	k.mu.Lock()
	k.lastTry = started
	k.mu.Unlock()

	err := k.doFetch(ctx)
	if err != nil && k.opts.OnFetchFailure != nil {
		k.opts.OnFetchFailure()
	}
	return err
}

func (k *KeySet) doFetch(ctx context.Context) error {
	jwksURI, userinfo, err := k.discover(ctx)
	if err != nil {
		k.mu.RLock()
		known, knownInfo := k.jwksURI, k.userinfo
		k.mu.RUnlock()
		if known == "" {
			return err
		}
		// Keep using the last known endpoints if only discovery fails.
		k.opts.Log.Warn("oidc discovery failed, using the last known endpoints", "err", err)
		jwksURI, userinfo = known, knownInfo
	}
	body, maxAge, err := k.get(ctx, jwksURI)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	keys, err := k.parseJWKS(body)
	if err != nil {
		return fmt.Errorf("JWKS: %w", err)
	}
	now := k.opts.Now()
	k.mu.Lock()
	k.keys, k.jwksURI, k.userinfo, k.loaded, k.maxAge, k.lastGood = keys, jwksURI, userinfo, true, maxAge, now
	k.mu.Unlock()
	if err := k.writeCache(cacheDocument{Issuer: k.opts.Issuer, JWKSURI: jwksURI, UserInfo: userinfo, JWKS: body, FetchedAt: now}); err != nil {
		k.opts.Log.Warn("oidc key cache not written", "file", k.opts.CacheFile, "err", err)
	}
	return nil
}

// discover returns the JWKS and userinfo URIs from the discovery document,
// whose issuer must equal the configured issuer exactly. A missing or
// unusable userinfo endpoint is returned as "".
func (k *KeySet) discover(ctx context.Context) (jwksURI, userinfo string, err error) {
	u := strings.TrimSuffix(k.opts.Issuer, "/") + "/.well-known/openid-configuration"
	body, _, err := k.get(ctx, u)
	if err != nil {
		return "", "", fmt.Errorf("fetch discovery document: %w", err)
	}
	var doc struct {
		Issuer   string `json:"issuer"`
		JWKSURI  string `json:"jwks_uri"`
		UserInfo string `json:"userinfo_endpoint"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("discovery document: %w", err)
	}
	if doc.Issuer != k.opts.Issuer {
		return "", "", fmt.Errorf("discovery issuer %q does not match the configured issuer %q", doc.Issuer, k.opts.Issuer)
	}
	if err := k.checkURL(doc.JWKSURI); err != nil {
		return "", "", fmt.Errorf("discovery jwks_uri: %w", err)
	}
	if doc.UserInfo != "" && k.checkURL(doc.UserInfo) != nil {
		k.opts.Log.Warn("oidc: ignoring unusable userinfo_endpoint", "url", doc.UserInfo)
		doc.UserInfo = ""
	}
	return doc.JWKSURI, doc.UserInfo, nil
}

// UserInfoEndpoint returns the userinfo endpoint from discovery, "" if unknown.
func (k *KeySet) UserInfoEndpoint() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.userinfo
}

func (k *KeySet) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch {
	case u.Host == "":
		return fmt.Errorf("%q is not an absolute URL", raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && k.opts.AllowHTTP:
		return nil
	}
	return fmt.Errorf("%q must use https", raw)
}

// get fetches a JSON document of bounded size and returns its max-age.
func (k *KeySet) get(ctx context.Context, rawURL string) ([]byte, time.Duration, error) {
	if err := k.checkURL(rawURL); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s answered %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > maxDocumentSize {
		return nil, 0, fmt.Errorf("%s answered with more than %d bytes", rawURL, maxDocumentSize)
	}
	return body, maxAge(resp.Header.Get("Cache-Control")), nil
}

func maxAge(cacheControl string) time.Duration {
	for _, d := range strings.Split(cacheControl, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(d)), "max-age=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// parseJWKS returns the usable signing keys. Keys are parsed one by one, so
// one malformed or unsupported key does not discard the others; a set without
// any usable key is an error and keeps the previous keys.
func (k *KeySet) parseJWKS(body []byte) ([]jose.JSONWebKey, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	var keys []jose.JSONWebKey
	for _, raw := range set.Keys {
		var key jose.JSONWebKey
		if err := key.UnmarshalJSON(raw); err != nil {
			k.opts.Log.Debug("oidc: skipping malformed JWK", "err", err)
			continue
		}
		if reason := unusable(key, k.algs); reason != "" {
			k.opts.Log.Debug("oidc: skipping JWK", "kid", key.KeyID, "reason", reason)
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, errors.New("no usable signing key")
	}
	return keys, nil
}

// unusable returns why key cannot verify tokens, or "" if it can.
func unusable(key jose.JSONWebKey, algs []jose.SignatureAlgorithm) string {
	switch {
	case key.KeyID == "":
		return "no key ID"
	case key.Use != "" && key.Use != "sig":
		return "not a signing key"
	case !key.IsPublic() || !key.Valid():
		return "not a valid public key"
	}
	if pub, ok := key.Key.(*rsa.PublicKey); ok && pub.N.BitLen() < minRSABits {
		return "RSA key shorter than 2048 bits"
	}
	for _, a := range algs {
		if keyMatchesAlg(key, string(a)) {
			return ""
		}
	}
	return "key type or algorithm not allowed"
}

func (k *KeySet) readCache() error {
	if k.opts.CacheFile == "" {
		return nil
	}
	raw, err := os.ReadFile(k.opts.CacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc cacheDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if doc.Issuer != k.opts.Issuer {
		return fmt.Errorf("cache is for issuer %q, configured is %q", doc.Issuer, k.opts.Issuer)
	}
	keys, err := k.parseJWKS(doc.JWKS)
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys, k.jwksURI, k.userinfo, k.loaded, k.lastGood = keys, doc.JWKSURI, doc.UserInfo, true, doc.FetchedAt
	return nil
}

// writeCache writes the cache file atomically: temp file, sync, rename.
func (k *KeySet) writeCache(doc cacheDocument) error {
	if k.opts.CacheFile == "" {
		return nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(k.opts.CacheFile)
	tmp, err := os.CreateTemp(dir, ".oidc-jwks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(cacheFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, k.opts.CacheFile)
}

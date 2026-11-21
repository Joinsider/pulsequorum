// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package httpcheck implements the HTTP(S) monitor type (FA-CHK-10..15).
package httpcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Type is the monitor type key.
const Type = "http"

// Checker runs HTTP(S) checks.
type Checker struct {
	// RootCAs overrides the system trust store (tests only).
	RootCAs *x509.CertPool
	// now is the clock for TLS expiry (tests only); nil = time.Now.
	now func() time.Time
}

var _ checker.Checker = (*Checker)(nil)
var _ checker.TargetLister = (*Checker)(nil)
var _ checker.ClusterCapabilityRequirer = (*Checker)(nil)

func init() { checker.Default.Register(&Checker{}) }

// Type implements checker.Checker.
func (*Checker) Type() string { return Type }

// Name implements checker.Checker.
func (*Checker) Name() string { return "HTTP(S)" }

// ConfigSchema implements checker.Checker.
func (*Checker) ConfigSchema() json.RawMessage { return json.RawMessage(schemaJSON) }

// Validate implements checker.Checker.
func (*Checker) Validate(cfg json.RawMessage) error { return validate(cfg) }

// Check implements checker.Checker. Every check uses its own transport without
// keep-alive, so a failing connection setup is never hidden by a reused
// connection (FA-CHK-11, PF-13).
func (c *Checker) Check(ctx context.Context, env *checker.Env, raw json.RawMessage) checker.Result {
	start := time.Now()
	cfg, err := parse(raw)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}
	ranges, err := parseStatusRanges(cfg.ExpectedStatus)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}

	// The client certificate is parsed per check and lives only in this call
	// (ADR 0042, section 8).
	certs := &clientCertSession{host: urlHost(cfg.URL)}
	var certWarning string
	if cc := cfg.ClientCertificate; cc != nil {
		cert, err := loadClientCertificate(cc)
		if err != nil {
			return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
		}
		if certWarning, err = clientCertValidity(cert.Leaf, c.clock(env), *cfg.TLSExpiryWarningDays); err != nil {
			return checker.Down(err, 0, nil)
		}
		certs.cert = cert
	}

	obs := &observer{start: start, now: func() time.Time { return c.clock(env) }}
	transport := c.transport(env, cfg, obs, certs)
	defer transport.CloseIdleConnections()

	redirects := 0
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if !*cfg.FollowRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) > cfg.MaxRedirects {
				return &checker.AssertionError{Msg: fmt.Sprintf("stopped after %d redirects", cfg.MaxRedirects)}
			}
			redirects = len(via)
			return nil
		},
	}

	req, err := c.request(httptrace.WithClientTrace(ctx, obs.trace()), env, cfg)
	if err != nil {
		return checker.Result{Status: checker.StatusDown, Message: err.Error(), ErrorClass: checker.ClassInternal}
	}

	resp, err := client.Do(req)
	if err != nil {
		details := obs.details(nil, redirects)
		certs.addDetails(details)
		return checker.Down(certs.explain(err), ms(time.Since(start)), details)
	}
	defer func() { _ = resp.Body.Close() }()

	body, truncated, err := readBody(resp.Body, cfg.MaxBodyBytes)
	latency := ms(time.Since(start))
	details := obs.details(resp, redirects)
	certs.addDetails(details)
	if truncated {
		details["bodyTruncated"] = true
	}
	if err != nil {
		return checker.Down(certs.explain(err), latency, details)
	}

	if err := evaluate(cfg, ranges, resp.StatusCode, body, latency); err != nil {
		return checker.Down(err, latency, details)
	}

	res := checker.Result{Status: checker.StatusUp, LatencyMs: latency, Message: resp.Status, Details: details}
	if info := obs.tlsInfo(); info != nil && *cfg.TLSExpiryWarningDays > 0 {
		left := info.notAfter.Sub(c.clock(env)).Hours() / 24
		if left < float64(*cfg.TLSExpiryWarningDays) {
			res.Status = checker.StatusDegraded
			res.Message = fmt.Sprintf("TLS certificate expires in %.0f days (%s)", left, info.notAfter.UTC().Format(time.DateOnly))
		}
	}
	if certWarning != "" {
		if res.Status == checker.StatusDegraded {
			res.Message += "; " + certWarning
		} else {
			res.Status, res.Message = checker.StatusDegraded, certWarning
		}
	}
	return res
}

// addDetails adds the non-secret facts of the configured client certificate
// to d (ADR 0042). Certificate and key material never enter the details.
func (s *clientCertSession) addDetails(d map[string]any) {
	if s.cert == nil || s.cert.Leaf == nil {
		return
	}
	leaf := s.cert.Leaf
	d["clientCertSubject"] = leaf.Subject.String()
	d["clientCertIssuer"] = leaf.Issuer.String()
	d["clientCertNotAfter"] = leaf.NotAfter.UTC().Format(time.RFC3339)
	d["clientCertSent"] = s.wasSent()
}

// wasSent reports whether the latest handshake that asked for a client
// certificate was answered with it.
func (s *clientCertSession) wasSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// clock is the clock for TLS expiry: the test override, then the node clock
// of env, then time.Now.
func (c *Checker) clock(env *checker.Env) time.Time {
	switch {
	case c.now != nil:
		return c.now()
	case env != nil && env.Now != nil:
		return env.Now()
	default:
		return time.Now()
	}
}

func (c *Checker) transport(env *checker.Env, cfg *Config, obs *observer, certs *clientCertSession) *http.Transport {
	network := map[string]string{"ipv4": "tcp4", "ipv6": "tcp6"}[cfg.IPFamily]
	dial := func(ctx context.Context, n, addr string) (net.Conn, error) {
		if network != "" {
			n = network
		}
		if cfg.ConnectTimeoutMs > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.ConnectTimeoutMs)*time.Millisecond)
			defer cancel()
		}
		return env.Dialer.DialContext(ctx, n, addr)
	}
	return &http.Transport{
		Proxy:       nil, // ignore HTTP_PROXY & co. (FA-CHK-13)
		DialContext: dial,
		// The TLS handshake is done here rather than by the transport so the
		// certificate is always verified against the host of the request URL.
		// Relying on ConnectionState.ServerName would skip the host check for
		// IP targets, which send no SNI.
		DialTLSContext: func(ctx context.Context, n, addr string) (net.Conn, error) {
			raw, err := dial(ctx, n, addr)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			conn, err := c.handshake(ctx, raw, host, cfg, obs, certs)
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			return conn, nil
		},
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
		ResponseHeaderTimeout:  time.Duration(cfg.ResponseHeaderTimeoutMs) * time.Millisecond,
		MaxResponseHeaderBytes: 64 << 10,
	}
}

// handshake runs the TLS handshake and verifies the chain itself, so the
// expiry is recorded even when errors are ignored (FA-CHK-14).
func (c *Checker) handshake(ctx context.Context, raw net.Conn, host string, cfg *Config, obs *observer, certs *clientCertSession) (*tls.Conn, error) {
	conn := tls.Client(raw, &tls.Config{
		ServerName:         host,
		NextProtos:         []string{"h2", "http/1.1"},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // G402: the chain and host are verified in VerifyConnection
		// host is the host of this connection, so a redirect target gets no
		// client certificate unless it is the monitor's own host (ADR 0042).
		GetClientCertificate: certs.hook(host),
		VerifyConnection: func(cs tls.ConnectionState) error {
			info, err := c.verify(cs, host, obs.now)
			obs.setTLS(info, err)
			if err != nil && !cfg.IgnoreTLSErrors {
				return &checker.TLSError{Err: err}
			}
			return nil
		},
	})
	if cfg.TLSTimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TLSTimeoutMs)*time.Millisecond)
		defer cancel()
	}
	start := time.Now()
	err := conn.HandshakeContext(ctx)
	obs.setTLSDuration(time.Since(start))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (c *Checker) request(ctx context.Context, env *checker.Env, cfg *Config) (*http.Request, error) {
	var body io.Reader
	if cfg.Body != "" {
		body = strings.NewReader(cfg.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for name, value := range cfg.Headers {
		if strings.EqualFold(name, "Host") {
			req.Host = value
			continue
		}
		req.Header.Set(name, value)
	}
	switch ua := cfg.UserAgent; {
	case ua != "":
		req.Header.Set("User-Agent", ua)
	case req.Header.Get("User-Agent") == "":
		req.Header.Set("User-Agent", env.UserAgent)
	}
	if cfg.Auth != nil {
		switch cfg.Auth.Type {
		case "basic":
			req.SetBasicAuth(cfg.Auth.Username, cfg.Auth.Password)
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+cfg.Auth.Token)
		}
	}
	return req, nil
}

type tlsInfo struct {
	notAfter time.Time
	subject  string
	issuer   string
	version  uint16
}

// verify checks the chain against the trust store and the host name the
// connection was made for, and returns the earliest expiry in the chain.
func (c *Checker) verify(cs tls.ConnectionState, host string, now func() time.Time) (*tlsInfo, error) {
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("server sent no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, cert := range cs.PeerCertificates[1:] {
		inter.AddCert(cert)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: inter,
		Roots:         c.RootCAs,
		CurrentTime:   now(),
	})

	certs := cs.PeerCertificates
	if err == nil && len(chains) > 0 {
		certs = chains[0]
	}
	info := &tlsInfo{notAfter: leaf.NotAfter, subject: leaf.Subject.String(), issuer: leaf.Issuer.String(), version: cs.Version}
	for _, cert := range certs {
		if cert.NotAfter.Before(info.notAfter) {
			info.notAfter = cert.NotAfter
		}
	}
	return info, err
}

func readBody(r io.Reader, limit int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, fmt.Errorf("read body: %w", err)
	}
	if len(body) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

func evaluate(cfg *Config, ranges []statusRange, code int, body []byte, latencyMs int) error {
	if !statusMatches(ranges, code) {
		return &checker.StatusCodeError{Code: code, Expected: cfg.ExpectedStatus}
	}
	if k := cfg.Keyword; k != nil {
		haystack, needle := string(body), k.Value
		if !k.CaseSensitive {
			haystack, needle = strings.ToLower(haystack), strings.ToLower(needle)
		}
		found := strings.Contains(haystack, needle)
		switch {
		case k.Mode == "notContains" && found:
			return &checker.AssertionError{Msg: fmt.Sprintf("keyword %q found but must be absent", k.Value)}
		case k.Mode != "notContains" && !found:
			return &checker.AssertionError{Msg: fmt.Sprintf("keyword %q not found", k.Value)}
		}
	}
	if j := cfg.JSONPath; j != nil {
		got, err := lookup(body, j.Path)
		if err != nil {
			return &checker.AssertionError{Msg: fmt.Sprintf("JSON path %s: %v", j.Path, err)}
		}
		if got != j.Expected {
			return &checker.AssertionError{Msg: fmt.Sprintf("JSON path %s is %q, expected %q", j.Path, truncate(got, 100), j.Expected)}
		}
	}
	if cfg.MaxResponseMs > 0 && latencyMs > cfg.MaxResponseMs {
		return &checker.AssertionError{Msg: fmt.Sprintf("response took %d ms (max %d ms)", latencyMs, cfg.MaxResponseMs)}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func ms(d time.Duration) int { return int(d.Milliseconds()) }

// observer collects phase timings, the remote address and TLS data. Trace
// callbacks may run on other goroutines, hence the mutex.
type observer struct {
	start time.Time
	// now is the clock for TLS verification and expiry.
	now func() time.Time

	mu         sync.Mutex
	dnsStart   time.Time
	dnsMs      int
	connStart  time.Time
	connectMs  int
	tlsMs      int
	ttfbMs     int
	remoteAddr string
	tls        *tlsInfo
	tlsErr     error
}

func (o *observer) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { o.mu.Lock(); o.dnsStart = time.Now(); o.mu.Unlock() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			o.mu.Lock()
			o.dnsMs = ms(time.Since(o.dnsStart))
			o.mu.Unlock()
		},
		ConnectStart: func(string, string) { o.mu.Lock(); o.connStart = time.Now(); o.mu.Unlock() },
		ConnectDone: func(_, _ string, err error) {
			o.mu.Lock()
			if err == nil {
				o.connectMs = ms(time.Since(o.connStart))
			}
			o.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			o.mu.Lock()
			o.remoteAddr = info.Conn.RemoteAddr().String()
			o.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			o.mu.Lock()
			o.ttfbMs = ms(time.Since(o.start))
			o.mu.Unlock()
		},
	}
}

func (o *observer) setTLS(info *tlsInfo, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tls, o.tlsErr = info, err
}

func (o *observer) setTLSDuration(d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tlsMs = ms(d)
}

func (o *observer) tlsInfo() *tlsInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.tls
}

// details renders the collected data (FA-CHK-03: phase timings and resolved IP).
func (o *observer) details(resp *http.Response, redirects int) map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := map[string]any{
		"timings": map[string]int{"dnsMs": o.dnsMs, "connectMs": o.connectMs, "tlsMs": o.tlsMs, "firstByteMs": o.ttfbMs},
	}
	if host, _, err := net.SplitHostPort(o.remoteAddr); err == nil {
		d["resolvedIp"] = host
	}
	if resp != nil {
		d["statusCode"] = resp.StatusCode
		d["protocol"] = resp.Proto
	}
	if redirects > 0 {
		d["redirects"] = redirects
	}
	if o.tls != nil {
		d["tlsExpiresAt"] = o.tls.notAfter.UTC().Format(time.RFC3339)
		d["tlsSubject"] = o.tls.subject
		d["tlsIssuer"] = o.tls.issuer
		d["tlsVersion"] = tls.VersionName(o.tls.version)
	}
	if o.tlsErr != nil {
		d["tlsError"] = o.tlsErr.Error()
	}
	return d
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// SendTimeout bounds one delivery (ADR 0014).
const SendTimeout = 10 * time.Second

// Target is a decrypted channel. It only lives in memory during a delivery.
type Target struct {
	Kind     string
	URL      string
	Method   string
	Headers  map[string]string
	Template string
}

// secrets returns every value that must not appear in errors or logs.
func (t Target) secrets() []string {
	s := secretParts(t.URL)
	for _, v := range t.Headers {
		s = append(s, v)
	}
	return s
}

// DeliveryError is a failed delivery. Its message is free of secrets.
type DeliveryError struct {
	// Permanent failures are not retried (FA-ALR-05).
	Permanent bool
	// RetryAfter is the earliest retry requested by the receiver (HTTP 429,
	// ADR 0023). Zero means none.
	RetryAfter time.Duration
	Msg        string
}

func (e *DeliveryError) Error() string { return e.Msg }

// Retry-After limits (ADR 0023). Shoutrrr does not expose the header, so a 429
// in its error text waits a fixed time.
const (
	maxRetryAfter      = time.Hour
	shoutrrrRetryAfter = 60 * time.Second
)

// Sender delivers alerts through the central dialer, so check and
// notification targets share the SSRF policy (FA-ALR-12).
type Sender struct {
	dialer *checker.Dialer
	client *http.Client
	log    *slog.Logger
	// now is the clock for HTTP-date Retry-After values (default time.Now).
	now func() time.Time
}

// NewSender creates a sender that dials through dialer.
func NewSender(dialer *checker.Dialer, log *slog.Logger) *Sender {
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   SendTimeout,
		ResponseHeaderTimeout: SendTimeout,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   SendTimeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	return &Sender{dialer: dialer, client: client, log: log, now: time.Now}
}

// WithClock sets the clock used to read HTTP-date Retry-After headers and
// returns the sender. The node's clock is injected this way (ADR 0020).
func (s *Sender) WithClock(now func() time.Time) *Sender {
	if now != nil {
		s.now = now
	}
	return s
}

// Validate checks a target before it is saved: a parsable Shoutrrr URL whose
// service uses our HTTP client or dialer, or an absolute http(s) webhook URL
// with a valid template.
func (s *Sender) Validate(t Target) error {
	switch t.Kind {
	case store.ChannelShoutrrr:
		router, err := s.router(t)
		if err != nil {
			return err
		}
		svc, err := router.Locate(t.URL)
		if err != nil {
			return &DeliveryError{Permanent: true, Msg: "invalid Shoutrrr URL: " + Redact(err.Error(), t.secrets()...)}
		}
		_, http := svc.(types.HTTPClientSetter)
		_, dial := svc.(types.DialContextSetter)
		if !http && !dial && s.dialer.Policy != nil {
			return &DeliveryError{Permanent: true, Msg: "this service cannot be used while SSRF protection is on"}
		}
		if err := s.validateShoutrrrHost(t.URL); err != nil {
			return &DeliveryError{Permanent: true, Msg: "invalid Shoutrrr URL: " + Redact(err.Error(), t.secrets()...)}
		}
		return nil
	case store.ChannelWebhook:
		if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
			return &DeliveryError{Permanent: true, Msg: "webhook URL must be an absolute http(s) URL"}
		}
		req, err := http.NewRequest(http.MethodPost, t.URL, nil)
		if err != nil {
			return &DeliveryError{Permanent: true, Msg: "invalid webhook URL: " + Redact(err.Error(), t.secrets()...)}
		}
		// Same save-time rules as for monitors (ADR 0030): notation always,
		// blocked IP literals when SSRF protection is on. Names are checked
		// when the webhook is sent.
		if err := checker.ValidateHost(req.URL.Hostname(), s.policy()); err != nil {
			return &DeliveryError{Permanent: true, Msg: "invalid webhook URL: " + Redact(err.Error(), t.secrets()...)}
		}
		switch t.Method {
		case "", http.MethodPost, http.MethodPut:
		default:
			return &DeliveryError{Permanent: true, Msg: "webhook method must be POST or PUT"}
		}
		return ValidateTemplate(t.Template)
	}
	return &DeliveryError{Permanent: true, Msg: fmt.Sprintf("unknown channel kind %q", t.Kind)}
}

// urlHostServices are the Shoutrrr services that contact the host of their
// URL; the others talk to a fixed public API and use the URL host for
// tokens or IDs (which may well be numeric). The list comes from the
// per-service test in shoutrrr_ssrf_test.go.
var urlHostServices = map[string]bool{
	"bark": true, "generic": true, "gotify": true, "googlechat": true, "hangouts": true,
	"homeassistant": true, "matrix": true, "mattermost": true, "mqtt": true, "mqtts": true,
	"ntfy": true, "opsgenie": true, "pagerduty": true, "rocketchat": true, "signal": true,
	"smtp": true, "xmpp": true, "xmpps": true, "zulip": true,
}

// validateShoutrrrHost applies the save-time host rules of ADR 0030 to the
// destination of a Shoutrrr URL. Telegram takes its server from the apiurl
// query parameter. Teams only accepts hosts below a fixed Microsoft domain,
// and the fixed-host services never contact the URL host, so these rely on
// the dial-time check alone.
func (s *Sender) validateShoutrrrHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL does not parse")
	}
	scheme, _, _ := strings.Cut(strings.ToLower(u.Scheme), "+")
	host := ""
	switch {
	case urlHostServices[scheme]:
		host = u.Hostname()
	case scheme == "telegram":
		api := u.Query().Get("apiurl")
		if api == "" {
			return nil
		}
		au, err := url.Parse(api)
		if err != nil {
			return errors.New("apiurl does not parse")
		}
		host = au.Hostname()
	}
	if host == "" {
		return nil // the service falls back to its default public host
	}
	return checker.ValidateHost(host, s.policy())
}

// policy is the address policy of the sender's dialer, nil when SSRF
// protection is off.
func (s *Sender) policy() checker.AddressPolicy {
	if s.dialer == nil {
		return nil
	}
	return s.dialer.Policy
}

// Send delivers one alert. Errors are *DeliveryError without secrets.
func (s *Sender) Send(ctx context.Context, t Target, p cluster.AlertPayload) error {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	var err error
	switch t.Kind {
	case store.ChannelShoutrrr:
		err = s.sendShoutrrr(ctx, t, p)
	case store.ChannelWebhook:
		err = s.sendWebhook(ctx, t, p)
	default:
		err = &DeliveryError{Permanent: true, Msg: fmt.Sprintf("unknown channel kind %q", t.Kind)}
	}
	return s.finish(t, err)
}

// SendBundle delivers several alerts as one message (ADR 0023). Only Shoutrrr
// channels are bundled; webhooks get one request per alert.
func (s *Sender) SendBundle(ctx context.Context, t Target, ps []cluster.AlertPayload) error {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	var err error
	if t.Kind != store.ChannelShoutrrr {
		err = &DeliveryError{Permanent: true, Msg: "only Shoutrrr channels take bundles"}
	} else {
		err = s.sendShoutrrrText(ctx, t, BundleTitle(ps), BundleText(ps))
	}
	return s.finish(t, err)
}

// finish turns err into a *DeliveryError without secrets, or returns nil.
func (s *Sender) finish(t Target, err error) error {
	if err == nil {
		return nil
	}
	var de *DeliveryError
	if !errors.As(err, &de) {
		de = &DeliveryError{Msg: err.Error()}
	}
	de.Msg = truncate(Redact(de.Msg, t.secrets()...), 500)
	return de
}

func (s *Sender) router(t Target) (interface {
	Locate(string) (types.Service, error)
}, error) {
	r, err := shoutrrr.NewSenderWithOptions(s.shoutrrrLogger(t), types.SenderOptions{
		HTTPClient: s.client, DialContext: s.dialer.DialContext, Timeout: SendTimeout,
	})
	if err != nil {
		return nil, &DeliveryError{Msg: Redact(err.Error(), t.secrets()...)}
	}
	return r, nil
}

// permanentStatus finds HTTP 401/403/404/410 in a Shoutrrr error message.
var permanentStatus = regexp.MustCompile(`\b(401|403|404|410)\b`)

// rateLimitStatus finds HTTP 429 in a Shoutrrr error message (ADR 0023).
var rateLimitStatus = regexp.MustCompile(`\b429\b`)

// shoutrrrError classifies an error message of the Shoutrrr library.
func shoutrrrError(msg string) *DeliveryError {
	de := &DeliveryError{Permanent: permanentStatus.MatchString(msg), Msg: msg}
	if !de.Permanent && rateLimitStatus.MatchString(msg) {
		de.RetryAfter = shoutrrrRetryAfter
	}
	return de
}

func (s *Sender) sendShoutrrr(ctx context.Context, t Target, p cluster.AlertPayload) error {
	return s.sendShoutrrrText(ctx, t, title(p), Text(p))
}

func (s *Sender) sendShoutrrrText(ctx context.Context, t Target, title, text string) error {
	router, err := shoutrrr.NewSenderWithOptions(s.shoutrrrLogger(t), types.SenderOptions{
		HTTPClient: s.client, DialContext: s.dialer.DialContext, Timeout: SendTimeout,
	}, t.URL)
	if err != nil {
		return &DeliveryError{Permanent: true, Msg: "invalid Shoutrrr URL: " + err.Error()}
	}
	params := types.Params{}
	params.SetTitle(title)
	for _, e := range router.SendContext(ctx, text, &params) {
		if e != nil {
			return shoutrrrError(e.Error())
		}
	}
	return nil
}

// parseRetryAfter reads a Retry-After header given in seconds or as an HTTP
// date relative to now. It returns 0 if the value is missing, invalid or in the
// past, and at most maxRetryAfter (ADR 0023).
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs >= int(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = at.Sub(now)
	} else {
		return 0
	}
	return min(max(d, 0), maxRetryAfter)
}

func (s *Sender) sendWebhook(ctx context.Context, t Target, p cluster.AlertPayload) error {
	body, err := Render(t.Template, p)
	if err != nil {
		return &DeliveryError{Permanent: true, Msg: "template: " + err.Error()}
	}
	method := t.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{Permanent: true, Msg: "invalid webhook URL"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Pulsequorum")
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return &DeliveryError{Permanent: errors.Is(err, checker.ErrSSRFBlocked), Msg: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return &DeliveryError{Permanent: true, Msg: "webhook answered " + resp.Status}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &DeliveryError{
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), s.now()),
			Msg:        "webhook answered " + resp.Status,
		}
	}
	return &DeliveryError{Msg: "webhook answered " + resp.Status}
}

// shoutrrrLogger routes the library's log output to slog at debug level,
// with the target's secrets removed.
func (s *Sender) shoutrrrLogger(t Target) *log.Logger {
	secrets := t.secrets()
	return log.New(writerFunc(func(b []byte) (int, error) {
		s.log.Debug("shoutrrr", "msg", Redact(strings.TrimSpace(string(b)), secrets...))
		return len(b), nil
	}), "", 0)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

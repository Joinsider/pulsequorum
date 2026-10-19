// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

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
	"regexp"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
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
	Msg       string
}

func (e *DeliveryError) Error() string { return e.Msg }

// Sender delivers alerts through the central dialer, so check and
// notification targets share the SSRF policy (FA-ALR-12).
type Sender struct {
	dialer *checker.Dialer
	client *http.Client
	log    *slog.Logger
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
	return &Sender{dialer: dialer, client: client, log: log}
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
			return &DeliveryError{Permanent: true, Msg: Redact("invalid Shoutrrr URL: "+err.Error(), t.secrets()...)}
		}
		_, http := svc.(types.HTTPClientSetter)
		_, dial := svc.(types.DialContextSetter)
		if !http && !dial && s.dialer.Policy != nil {
			return &DeliveryError{Permanent: true, Msg: "this service cannot be used while SSRF protection is on"}
		}
		return nil
	case store.ChannelWebhook:
		if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
			return &DeliveryError{Permanent: true, Msg: "webhook URL must be an absolute http(s) URL"}
		}
		if _, err := http.NewRequest(http.MethodPost, t.URL, nil); err != nil {
			return &DeliveryError{Permanent: true, Msg: Redact("invalid webhook URL: "+err.Error(), t.secrets()...)}
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

func (s *Sender) sendShoutrrr(ctx context.Context, t Target, p cluster.AlertPayload) error {
	router, err := shoutrrr.NewSenderWithOptions(s.shoutrrrLogger(t), types.SenderOptions{
		HTTPClient: s.client, DialContext: s.dialer.DialContext, Timeout: SendTimeout,
	}, t.URL)
	if err != nil {
		return &DeliveryError{Permanent: true, Msg: "invalid Shoutrrr URL: " + err.Error()}
	}
	params := types.Params{}
	params.SetTitle(fmt.Sprintf("%s: %s", p.Status, p.MonitorName))
	for _, e := range router.SendContext(ctx, Text(p), &params) {
		if e != nil {
			return &DeliveryError{Permanent: permanentStatus.MatchString(e.Error()), Msg: e.Error()}
		}
	}
	return nil
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
	req.Header.Set("User-Agent", "UptimeCluster")
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

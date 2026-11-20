// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package alert

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"missing", "", 0},
		{"seconds", "120", 2 * time.Minute},
		{"seconds with spaces", " 30 ", 30 * time.Second},
		{"zero seconds", "0", 0},
		{"negative seconds", "-5", 0},
		{"seconds capped at one hour", "7200", time.Hour},
		{"seconds overflowing int", "99999999999999999999", 0},
		{"http date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http date capped at one hour", now.Add(5 * time.Hour).Format(http.TimeFormat), time.Hour},
		{"http date in the past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"garbage", "soon", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.value, now); got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}
}

// TestWebhookRateLimited checks that a webhook's 429 is not permanent and
// carries its Retry-After, read with the injected clock.
func TestWebhookRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "120", 2 * time.Minute},
		{"http date", now.Add(90 * time.Second).UTC().Format(http.TimeFormat), 90 * time.Second},
		{"capped", "7200", time.Hour},
		{"missing", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			s := NewSender(&checker.Dialer{}, slog.New(slog.DiscardHandler)).WithClock(func() time.Time { return now })
			err := s.Send(context.Background(), Target{Kind: store.ChannelWebhook, URL: srv.URL},
				cluster.AlertPayload{Event: store.EventDown})
			var de *DeliveryError
			if !errors.As(err, &de) {
				t.Fatalf("error = %v, want *DeliveryError", err)
			}
			if de.Permanent || de.RetryAfter != tt.want {
				t.Errorf("error = %+v, want not permanent with RetryAfter %s", de, tt.want)
			}
		})
	}
}

// TestShoutrrrErrorClassification checks the status codes found in Shoutrrr
// error text: 429 waits 60 s, 401/403/404/410 are permanent, and other numbers
// are not statuses.
func TestShoutrrrErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		msg        string
		permanent  bool
		retryAfter time.Duration
	}{
		{"rate limited", "generic: 429 Too Many Requests", false, shoutrrrRetryAfter},
		{"forbidden", "generic: 403 Forbidden", true, 0},
		{"other number", "generic: connection to 4290 refused", false, 0},
		{"server error", "generic: 503 Service Unavailable", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			de := shoutrrrError(tt.msg)
			if de.Permanent != tt.permanent || de.RetryAfter != tt.retryAfter || de.Msg != tt.msg {
				t.Errorf("shoutrrrError(%q) = %+v, want permanent %v, RetryAfter %s", tt.msg, de, tt.permanent, tt.retryAfter)
			}
		})
	}
}

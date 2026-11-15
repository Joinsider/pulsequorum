// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package fake

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"
)

func TestNtfyRecorderRecordsShoutrrrMessage(t *testing.T) {
	rec := NewRecorder()
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The same URL shape the driver puts into the notification channel.
	shoutURL := "ntfy://" + u.Host + "/as01-test?scheme=http"
	sender, err := shoutrrr.CreateSenderWithOptions(types.SenderOptions{}, shoutURL)
	if err != nil {
		t.Fatalf("CreateSender: %v", err)
	}
	params := types.Params{}
	params.SetTitle("DOWN: as01-http-test")
	body := "🔴 DOWN: as01-http-test\nSince 2026-01-01 00:00:00 UTC"
	for _, e := range sender.Send(body, &params) {
		if e != nil {
			t.Fatalf("Send: %v", e)
		}
	}

	msgs := rec.Messages()
	if len(msgs) != 1 {
		t.Fatalf("recorded %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Path != "/as01-test" {
		t.Errorf("path = %q, want /as01-test", m.Path)
	}
	if m.Title != "DOWN: as01-http-test" {
		t.Errorf("title = %q", m.Title)
	}
	if !strings.Contains(m.Body, "DOWN: as01-http-test") {
		t.Errorf("body = %q", m.Body)
	}
	if m.At.IsZero() {
		t.Error("arrival time not recorded")
	}
}

func TestNtfyRecorderRejectsNonPost(t *testing.T) {
	srv := httptest.NewServer(NewRecorder())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/topic")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestNtfyRecorderWaitForFiltersBySinceAndMatch(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec.msgs = []Message{
		{At: base.Add(-time.Second), Path: "/t", Body: "DOWN: x"},    // before since
		{At: base.Add(time.Second), Path: "/other", Body: "DOWN: x"}, // other topic
		{At: base.Add(2 * time.Second), Path: "/t", Body: "UP: x"},   // wrong event
		{At: base.Add(3 * time.Second), Path: "/t", Body: "DOWN: x"}, // match
	}
	match := func(m Message) bool { return m.Path == "/t" && strings.Contains(m.Body, "DOWN: x") }

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, ok := rec.WaitFor(ctx, base, match)
	if !ok {
		t.Fatal("WaitFor found nothing")
	}
	if !got.At.Equal(base.Add(3 * time.Second)) {
		t.Errorf("matched message at %s, want %s", got.At, base.Add(3*time.Second))
	}
}

func TestNtfyRecorderWaitForTimesOut(t *testing.T) {
	rec := NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, ok := rec.WaitFor(ctx, time.Now(), func(Message) bool { return true }); ok {
		t.Error("WaitFor returned a message from an empty recorder")
	}
}

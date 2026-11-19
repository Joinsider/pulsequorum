// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/joinsider/uptimecluster/test/internal/ucapi"
)

func TestStreamURL(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{name: "http", base: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/api/v1/stream?ticket=t1"},
		{name: "https", base: "https://node.example.test", want: "wss://node.example.test/api/v1/stream?ticket=t1"},
		{name: "base with trailing slash", base: "http://127.0.0.1:8080/", want: "ws://127.0.0.1:8080/api/v1/stream?ticket=t1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := streamURL(tt.base, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("streamURL = %q, want %q", got, tt.want)
			}
		})
	}
	if _, err := streamURL("ftp://node", "t1"); err == nil {
		t.Error("unsupported scheme accepted")
	}
}

// fakeStreamServer issues one ticket and sends one heartbeat on the stream
// that mentions monitor m-1. The stream stays open until the client closes it.
func fakeStreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/stream/ticket", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ticket":"tkt-1","expiresAt":"2030-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /api/v1/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ticket") != "tkt-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"heartbeat","monitorId":"m-1"}`)); err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamCollectsMessagesAfterIndex(t *testing.T) {
	srv := fakeStreamServer(t)
	c := ucapi.New(srv.URL, "", "")
	c.Token = "tok"

	ctx := context.Background()
	sc, err := startStream(ctx, c, srv.URL)
	if err != nil {
		t.Fatalf("startStream: %v", err)
	}
	defer sc.close()

	isM1 := func(msg string) bool { return strings.Contains(msg, "m-1") }
	if _, ok := sc.waitFor(ctx, 0, 5*time.Second, isM1); !ok {
		t.Fatal("heartbeat of m-1 not received")
	}
	if sc.count() != 1 {
		t.Errorf("count = %d, want 1", sc.count())
	}
	// Messages before index 1 are not considered: nothing follows the heartbeat.
	if _, ok := sc.waitFor(ctx, 1, 300*time.Millisecond, isM1); ok {
		t.Error("waitFor matched a message before its start index")
	}
}

func TestStreamDialErrorHidesTicket(t *testing.T) {
	srv := fakeStreamServer(t)
	c := ucapi.New(srv.URL, "", "")
	c.Token = "tok"
	// The ticket comes from the fake node, but the WebSocket is dialed on a
	// port where nothing listens. The dial error may quote the URL with the
	// ticket in it; the error returned must not.
	_, err := startStream(context.Background(), c, "http://127.0.0.1:1")
	if err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	if strings.Contains(err.Error(), "tkt-1") {
		t.Errorf("error leaks the ticket: %v", err)
	}
}

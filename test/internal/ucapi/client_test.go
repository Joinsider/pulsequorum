// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package ucapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeNode serves the few endpoints the client tests use. It accepts the
// password "pw-correct-123" and hands out token "tok-1".
func fakeNode(t *testing.T, logins *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "pw-correct-123" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","detail":"invalid credentials"}`))
			return
		}
		logins.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "tok-1", "expiresAt": time.Now()})
	})
	mux.HandleFunc("GET /api/v1/monitors", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":      []map[string]any{{"id": "m1", "name": "one", "type": "http"}},
				"nextCursor": "c2",
			})
		case "c2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":      []map[string]any{{"id": "m2", "name": "two", "type": "http"}},
				"nextCursor": nil,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /api/v1/admin/nodes/forbidden", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"title":"Forbidden","detail":"platform admin required"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClientLogsInLazilyAndPages(t *testing.T) {
	var logins atomic.Int32
	srv := fakeNode(t, &logins)
	c := New(srv.URL+"/", "admin", "pw-correct-123")

	mons, err := c.ListMonitors(context.Background())
	if err != nil {
		t.Fatalf("ListMonitors: %v", err)
	}
	if len(mons) != 2 || mons[0].ID != "m1" || mons[1].ID != "m2" {
		t.Fatalf("monitors = %+v, want m1 and m2 across two pages", mons)
	}
	if n := logins.Load(); n != 1 {
		t.Errorf("logins = %d, want 1 (lazy login, then token reuse)", n)
	}
	if c.Token != "tok-1" {
		t.Errorf("Token not kept after login")
	}
}

func TestClientErrorCarriesDetailButNoSecrets(t *testing.T) {
	var logins atomic.Int32
	srv := fakeNode(t, &logins)
	c := New(srv.URL, "admin", "pw-wrong-456")

	err := c.Login(context.Background())
	if err == nil {
		t.Fatal("Login with a wrong password succeeded")
	}
	if StatusCode(err) != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", StatusCode(err))
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Errorf("error lacks problem detail: %v", err)
	}
	if strings.Contains(err.Error(), "pw-wrong-456") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestClientForbiddenDetail(t *testing.T) {
	var logins atomic.Int32
	srv := fakeNode(t, &logins)
	c := New(srv.URL, "admin", "")
	c.Token = "tok-x"

	_, err := c.Do(context.Background(), http.MethodGet, "/admin/nodes/forbidden", nil, nil, nil)
	if StatusCode(err) != http.StatusForbidden {
		t.Fatalf("err = %v, want 403", err)
	}
	if got := err.Error(); got != "HTTP 403: platform admin required" {
		t.Errorf("error = %q", got)
	}
}

func TestProblemDetailTruncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	raw := []byte(fmt.Sprintf(`{"title":"T","detail":%q}`, long))
	got := problemDetail(raw)
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Errorf("detail length %d, want 200 plus ellipsis", len(got))
	}
	if problemDetail([]byte("not json")) != "" {
		t.Error("non-JSON body produced a detail")
	}
	if got := problemDetail([]byte(`{"title":"Only title"}`)); got != "Only title" {
		t.Errorf("title fallback = %q", got)
	}
}

func TestNeedsAuth(t *testing.T) {
	for path, want := range map[string]bool{
		"/health":      false,
		"/auth/setup":  false,
		"/auth/login":  false,
		"/monitors":    true,
		"/admin/nodes": true,
	} {
		if got := needsAuth(path); got != want {
			t.Errorf("needsAuth(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestDoRawReturnsBodyOfEveryAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/missing" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"Not Found","detail":"monitor not found","id":"m-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "")
	c.Token = "tok-x"

	var out struct {
		Name string `json:"name"`
	}
	status, raw, err := c.DoRaw(context.Background(), http.MethodGet, "/ok", nil, nil, &out)
	if err != nil || status != http.StatusOK || out.Name != "ok" || string(raw) != `{"name":"ok"}` {
		t.Errorf("success: status %d, raw %q, out %+v, err %v", status, raw, out, err)
	}

	status, raw, err = c.DoRaw(context.Background(), http.MethodGet, "/missing", nil, nil, nil)
	if StatusCode(err) != http.StatusNotFound || status != http.StatusNotFound {
		t.Errorf("error answer: status %d, err %v", status, err)
	}
	if !strings.Contains(string(raw), `"id":"m-1"`) {
		t.Errorf("error answer body not returned: %q", raw)
	}
	if !strings.Contains(err.Error(), "monitor not found") {
		t.Errorf("error lacks detail: %v", err)
	}
}

func TestSystemAndHealthFields(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]any
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"accessToken":"tok-1","expiresAt":"2030-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","role":"voter","nodeName":"n1","version":"v","commit":"c",` +
			`"degraded":true,"partition":["node-a"]}`))
	})
	mux.HandleFunc("POST /api/v1/admin/system-channels", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"id":"ch-1","name":"sys","kind":"shoutrrr","events":["system"]}`))
	})
	mux.HandleFunc("GET /api/v1/admin/system-alerts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("active") == "false" {
			_, _ = w.Write([]byte(`[{"kind":"node_impaired","subject":"node-b","active":false,"details":{}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"kind":"node_impaired","subject":"node-b","active":true,"level":0,"details":{"reason":"reference"}}]`))
	})
	mux.HandleFunc("POST /api/v1/admin/system-channels/ch-1/test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"delivered":true,"durationMs":3}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "admin", "pw")
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Degraded || len(h.Partition) != 1 || h.Partition[0] != "node-a" {
		t.Errorf("health = %+v, want degraded with partition [node-a]", h)
	}

	ch, err := c.CreateSystemChannel(ctx, "sys", "ntfy://x/y", []string{"system"})
	if err != nil || ch.ID != "ch-1" {
		t.Fatalf("CreateSystemChannel = %+v, %v", ch, err)
	}
	if gotBody["kind"] != "shoutrrr" || gotBody["url"] != "ntfy://x/y" {
		t.Errorf("request body = %v", gotBody)
	}
	if res, err := c.TestSystemChannel(ctx, "ch-1"); err != nil || !res.Delivered {
		t.Errorf("TestSystemChannel = %+v, %v", res, err)
	}

	active, err := c.ListSystemAlerts(ctx, false)
	if err != nil || len(active) != 1 || !active[0].Active || active[0].Subject != "node-b" {
		t.Errorf("active alerts = %+v, %v", active, err)
	}
	all, err := c.ListSystemAlerts(ctx, true)
	if err != nil || len(all) != 1 || all[0].Active {
		t.Errorf("all alerts = %+v, %v", all, err)
	}
}

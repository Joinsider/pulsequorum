package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		NodeName:        "node-1",
		Role:            config.RoleVoter,
		AuthMode:        config.AuthLocal,
		PublicURL:       "https://mon1.example.com",
		ShutdownTimeout: time.Second,
	}
}

func testServer() *Server {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "spa") })
	return New(testConfig(), slog.New(slog.DiscardHandler), ui)
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealth(t *testing.T) {
	rec := get(t, testServer().Handler(), "/api/v1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Role != "voter" || h.NodeName != "node-1" || h.Version == "" {
		t.Errorf("unexpected health: %+v", h)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("missing security header, got %q", got)
	}
}

func TestReadiness(t *testing.T) {
	s := testServer()
	if rec := get(t, s.Handler(), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before start: status = %d, want 503", rec.Code)
	}
	s.ready.Store(true)
	if rec := get(t, s.Handler(), "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("after start: status = %d, want 200", rec.Code)
	}
	if rec := get(t, s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness: status = %d, want 200", rec.Code)
	}
}

func TestFrontendConfig(t *testing.T) {
	rec := get(t, testServer().Handler(), "/config.json")
	var fc FrontendConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatal(err)
	}
	if len(fc.Nodes) != 1 || fc.Nodes[0] != "https://mon1.example.com" || fc.Auth.Mode != "local" {
		t.Errorf("unexpected config: %+v", fc)
	}

	s := testServer()
	s.cfg.PublicURL = ""
	rec = get(t, s.Handler(), "/config.json")
	if body := rec.Body.String(); body != "{\"nodes\":[],\"auth\":{\"mode\":\"local\"}}\n" {
		t.Errorf("without PUBLIC_URL: body = %q, want empty node list", body)
	}
}

func TestUnknownAPIRouteIsProblem(t *testing.T) {
	rec := get(t, testServer().Handler(), "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestNonAPIRoutesServeUI(t *testing.T) {
	for _, path := range []string{"/", "/monitors/1"} {
		if rec := get(t, testServer().Handler(), path); rec.Body.String() != "spa" {
			t.Errorf("%s: body = %q, want spa", path, rec.Body.String())
		}
	}
}

func TestPanicRecovery(t *testing.T) {
	s := New(testConfig(), slog.New(slog.DiscardHandler), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	if rec := get(t, s.Handler(), "/"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := testServer()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	url := "http://" + ln.Addr().String() + "/readyz"
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}

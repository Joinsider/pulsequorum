// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/checker"
	_ "github.com/joinsider/uptimecluster/internal/checker/builtin"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/config"
	"github.com/joinsider/uptimecluster/internal/store"
	"github.com/joinsider/uptimecluster/internal/store/sqlite"
)

// headerAuth reads "tenant:role" from X-Test-Principal.
type headerAuth struct{}

func (headerAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	tenant, role, ok := strings.Cut(r.Header.Get("X-Test-Principal"), ":")
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return auth.Principal{UserID: "u-" + role, TenantID: tenant, Roles: []auth.Role{auth.Role(role)}}, nil
}

type env struct {
	t       *testing.T
	handler http.Handler
	metrics *sqlite.MetricsStore
	node    *cluster.Node
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	state, err := sqlite.OpenState(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := sqlite.OpenMetrics(ctx, dir, sqlite.MetricsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := cluster.Start(ctx, cluster.Config{NodeID: "55555555-5555-5555-5555-555555555555", Advertise: "n:9443",
		DataDir: dir, Timeout: 200 * time.Millisecond, Logger: log, RaftLogLevel: "error"}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = metrics.Close()
		_ = state.Close()
	})
	if err := cluster.EnsureDefaultTenant(ctx, node, state); err != nil {
		t.Fatal(err)
	}
	cmd, _ := cluster.NewPutTenant("tenant-b", "tenant-b", cluster.TenantDocument{Name: "B", Plan: "default"})
	if _, err := node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	Register(mux, config.Config{NodeName: "n1", Role: config.RoleVoter}, log, Deps{
		State: state, Metrics: metrics, Cluster: node, Registry: checker.Default,
		CheckEnv: &checker.Env{Dialer: &checker.Dialer{}, UserAgent: "test"}, NodeID: "n1",
		Authenticator: headerAuth{},
	})
	return &env{t: t, handler: mux, metrics: metrics, node: node}
}

type response struct {
	code int
	body []byte
	hdr  http.Header
}

func (e *env) do(method, path, principal string, body any, headers ...string) response {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Test-Principal", principal)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return response{code: rec.Code, body: rec.Body.Bytes(), hdr: rec.Header()}
}

func decode[T any](t *testing.T, r response) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

const (
	editor = "default:tenant-editor"
	viewer = "default:tenant-viewer"
	other  = "tenant-b:tenant-admin"
)

func httpMonitor(name, url string) map[string]any {
	return map[string]any{"name": name, "type": "http", "intervalSeconds": 60, "config": map[string]any{"url": url}}
}

func TestMonitorCRUD(t *testing.T) {
	e := newEnv(t)

	res := e.do("POST", "/api/v1/monitors", editor, map[string]any{
		"name": "API", "type": "http", "intervalSeconds": 30, "tags": []string{"prod", "api"},
		"config": map[string]any{"url": "https://example.com/health"},
	})
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	m := decode[gen.Monitor](t, res)
	if *m.TimeoutSeconds != 24 || *m.Retries != 1 || m.Assignment.Mode != gen.AssignmentModeAll ||
		*m.ConsensusPolicy != gen.ConsensusPolicyMajority || m.Status != gen.MonitorStatusPENDING {
		t.Errorf("defaults not applied: %+v", m)
	}
	id := m.Id.String()

	got := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+id, viewer, nil))
	if got.Name != "API" || len(*got.Tags) != 2 {
		t.Errorf("get = %+v", got)
	}

	upd := httpMonitor("API v2", "https://example.com/v2")
	upd["timeoutSeconds"] = 10
	res = e.do("PUT", "/api/v1/monitors/"+id, editor, upd)
	if res.code != http.StatusOK || decode[gen.Monitor](t, res).Name != "API v2" {
		t.Fatalf("update = %d %s", res.code, res.body)
	}

	res = e.do("POST", "/api/v1/monitors/"+id+"/pause", editor, nil)
	if decode[gen.Monitor](t, res).Status != gen.MonitorStatusPAUSED {
		t.Errorf("pause = %s", res.body)
	}
	res = e.do("POST", "/api/v1/monitors/"+id+"/resume", editor, nil)
	if decode[gen.Monitor](t, res).Status != gen.MonitorStatusPENDING {
		t.Errorf("resume = %s", res.body)
	}

	res = e.do("POST", "/api/v1/monitors/"+id+"/clone", editor, nil)
	clone := decode[gen.Monitor](t, res)
	if res.code != http.StatusCreated || clone.Name != "API v2 (copy)" || !*clone.Paused || clone.Id == m.Id {
		t.Errorf("clone = %d %+v", res.code, clone)
	}

	if res := e.do("DELETE", "/api/v1/monitors/"+id, editor, nil); res.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if res := e.do("GET", "/api/v1/monitors/"+id, editor, nil); res.code != http.StatusNotFound {
		t.Errorf("get after delete = %d", res.code)
	}
	if res := e.do("DELETE", "/api/v1/monitors/"+id, editor, nil); res.code != http.StatusNotFound {
		t.Errorf("second delete = %d", res.code)
	}
}

func TestValidationErrors(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"interval too short", map[string]any{"name": "x", "type": "http", "intervalSeconds": 10, "config": map[string]any{"url": "https://a"}}, "/intervalSeconds"},
		{"timeout above 80 %", map[string]any{"name": "x", "type": "http", "intervalSeconds": 20, "timeoutSeconds": 17, "config": map[string]any{"url": "https://a"}}, "/timeoutSeconds"},
		{"unknown type", map[string]any{"name": "x", "type": "smtp", "intervalSeconds": 60, "config": map[string]any{}}, "/type"},
		{"invalid config", map[string]any{"name": "x", "type": "http", "intervalSeconds": 60, "config": map[string]any{"url": "ftp://a"}}, "/config/url"},
		{"missing config field", map[string]any{"name": "x", "type": "http", "intervalSeconds": 60, "config": map[string]any{}}, "/config/url"},
		{"empty name", map[string]any{"name": " ", "type": "http", "intervalSeconds": 60, "config": map[string]any{"url": "https://a"}}, "/name"},
		{"assignment count", map[string]any{"name": "x", "type": "http", "intervalSeconds": 60, "config": map[string]any{"url": "https://a"},
			"assignment": map[string]any{"mode": "count"}}, "/assignment/count"},
		{"channels not yet", map[string]any{"name": "x", "type": "http", "intervalSeconds": 60, "config": map[string]any{"url": "https://a"},
			"notificationChannelIds": []string{"0192d5e0-0000-7000-8000-000000000000"}}, "/notificationChannelIds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.do("POST", "/api/v1/monitors", editor, tt.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d %s", res.code, res.body)
			}
			p := decode[gen.Problem](t, res)
			found := false
			for _, f := range *p.Errors {
				found = found || f.Field == tt.field
			}
			if !found {
				t.Errorf("errors = %+v, want field %s", *p.Errors, tt.field)
			}
		})
	}
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	if res := e.do("GET", "/api/v1/monitors", "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d", res.code)
	}
	if res := e.do("POST", "/api/v1/monitors", viewer, httpMonitor("x", "https://a")); res.code != http.StatusForbidden {
		t.Errorf("viewer create = %d", res.code)
	}
	if res := e.do("GET", "/api/v1/monitors", viewer, nil); res.code != http.StatusOK {
		t.Errorf("viewer list = %d", res.code)
	}
	if res := e.do("GET", "/api/v1/health", "", nil); res.code != http.StatusOK {
		t.Errorf("public health = %d", res.code)
	}
}

// TestTenantIsolation checks every monitor endpoint with another tenant
// (FA-TEN-01/02, AS-06).
func TestTenantIsolation(t *testing.T) {
	e := newEnv(t)
	m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, httpMonitor("secret", "https://a")))
	id := m.Id.String()

	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/monitors/" + id},
		{"PUT", "/api/v1/monitors/" + id},
		{"DELETE", "/api/v1/monitors/" + id},
		{"POST", "/api/v1/monitors/" + id + "/pause"},
		{"POST", "/api/v1/monitors/" + id + "/resume"},
		{"POST", "/api/v1/monitors/" + id + "/clone"},
		{"POST", "/api/v1/monitors/" + id + "/test"},
		{"GET", "/api/v1/monitors/" + id + "/heartbeats"},
	} {
		var body any
		if c.method == "PUT" {
			body = httpMonitor("hijacked", "https://evil")
		}
		if res := e.do(c.method, c.path, other, body); res.code != http.StatusNotFound {
			t.Errorf("%s %s as other tenant = %d, want 404", c.method, c.path, res.code)
		}
	}
	list := decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors", other, nil))
	if len(list.Items) != 0 {
		t.Errorf("other tenant sees %d monitors", len(list.Items))
	}
	if got := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+id, editor, nil)); got.Name != "secret" {
		t.Errorf("owner's monitor changed: %+v", got)
	}
}

func TestIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	key := "0192d5e0-1111-7000-8000-000000000001"
	first := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, httpMonitor("once", "https://a"), "Idempotency-Key", key))
	second := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, httpMonitor("once", "https://a"), "Idempotency-Key", key))
	if first.Id != second.Id {
		t.Errorf("retry created a second monitor: %s vs %s", first.Id, second.Id)
	}
	// The same key from another tenant is a different request and must not
	// return tenant default's monitor.
	foreign := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", other, httpMonitor("theirs", "https://b"), "Idempotency-Key", key))
	if foreign.Id == first.Id || foreign.Name != "theirs" {
		t.Errorf("idempotency key crossed tenants: %+v", foreign)
	}
	if n := len(decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors", editor, nil)).Items); n != 1 {
		t.Errorf("monitors = %d, want 1", n)
	}
}

func TestListPagingAndFilters(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		body := httpMonitor(fmt.Sprintf("svc-%d", i), "https://a")
		if i%2 == 0 {
			body["tags"] = []string{"even"}
		}
		if i == 4 {
			body["paused"] = true
		}
		e.do("POST", "/api/v1/monitors", editor, body)
	}
	var names []string
	path := "/api/v1/monitors?limit=2"
	for range 10 {
		page := decode[gen.MonitorPage](t, e.do("GET", path, viewer, nil))
		for _, m := range page.Items {
			names = append(names, m.Name)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/monitors?limit=2&cursor=" + *page.NextCursor
	}
	if len(names) != 5 {
		t.Errorf("paged names = %v", names)
	}
	if n := len(decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?tag=even", viewer, nil)).Items); n != 3 {
		t.Errorf("tag filter = %d, want 3", n)
	}
	if n := len(decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?q=SVC-1", viewer, nil)).Items); n != 1 {
		t.Errorf("search = %d, want 1", n)
	}
	if n := len(decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?status=PAUSED", viewer, nil)).Items); n != 1 {
		t.Errorf("status filter = %d, want 1", n)
	}
	if res := e.do("GET", "/api/v1/monitors?cursor=garbage", viewer, nil); res.code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d", res.code)
	}
}

func TestHeartbeatsAndTypesAndTestRun(t *testing.T) {
	e := newEnv(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer target.Close()
	m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, httpMonitor("target", target.URL)))
	id := m.Id.String()

	var hbs []store.Heartbeat
	for i := range 5 {
		hbs = append(hbs, store.Heartbeat{NodeID: "n1", Incarnation: "i", Seq: uint64(i + 1), TenantID: "default", MonitorID: id,
			TsMs: int64(1000 * (i + 1)), Status: store.CheckUp, LatencyMs: 5})
	}
	hbs[4].Status, hbs[4].ErrorClass, hbs[4].Message = store.CheckDown, "connect", "refused"
	if _, err := e.metrics.InsertHeartbeats(context.Background(), hbs); err != nil {
		t.Fatal(err)
	}
	var all []gen.Heartbeat
	path := "/api/v1/monitors/" + id + "/heartbeats?limit=2"
	for range 10 {
		page := decode[gen.HeartbeatPage](t, e.do("GET", path, viewer, nil))
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/monitors/" + id + "/heartbeats?limit=2&cursor=" + *page.NextCursor
	}
	if len(all) != 5 || all[4].Status != gen.CheckStatusDOWN || *all[4].ErrorClass != "connect" {
		t.Errorf("heartbeats = %+v", all)
	}

	types := decode[[]gen.MonitorType](t, e.do("GET", "/api/v1/monitor-types", viewer, nil))
	byType := map[string]gen.MonitorType{}
	for _, mt := range types {
		byType[mt.Type] = mt
	}
	if len(byType) != 4 || byType["icmp"].RequiresCapability == nil || byType["http"].ConfigSchema["properties"] == nil {
		t.Errorf("monitor types = %+v", types)
	}

	res := e.do("POST", "/api/v1/monitors/"+id+"/test", editor, nil)
	if r := decode[gen.CheckResult](t, res); res.code != http.StatusOK || r.Status != gen.CheckStatusUP || r.NodeId != "n1" {
		t.Errorf("test run = %d %s", res.code, res.body)
	}
	if res := e.do("POST", "/api/v1/monitors/"+id+"/test", viewer, nil); res.code != http.StatusForbidden {
		t.Errorf("viewer test run = %d", res.code)
	}
}

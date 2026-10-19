// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/store"
)

func (e *env) createMonitor(name string) string {
	e.t.Helper()
	res := e.do("POST", "/api/v1/monitors", editor, httpMonitor(name, "https://example.com"))
	if res.code != http.StatusCreated {
		e.t.Fatalf("create = %d %s", res.code, res.body)
	}
	return decode[gen.Monitor](e.t, res).Id.String()
}

func (e *env) changeState(monitorID string, from, to store.MonitorStatus, sinceMs int64) string {
	e.t.Helper()
	c := cluster.StateChange{TenantID: cluster.DefaultTenantID, MonitorID: monitorID, From: from, To: to, SinceMs: sinceMs}
	if to == store.StatusDown {
		c.Incident = &store.Incident{ID: uuid.NewString(), Cause: "connection refused", ErrorClass: "connect", AffectedNodes: []string{"n1"}}
	}
	if _, err := e.node.Apply(context.Background(), cluster.NewSetMonitorState(c)); err != nil {
		e.t.Fatal(err)
	}
	if c.Incident != nil {
		return c.Incident.ID
	}
	return ""
}

// TestMonitorStatusAndIncidents covers FA-INC-02 and the consensus status
// fields of the monitor resource.
func TestMonitorStatusAndIncidents(t *testing.T) {
	e := newEnv(t)
	web, db := e.createMonitor("web"), e.createMonitor("db")
	b := time.Now().UnixMilli()

	first := e.changeState(web, store.StatusPending, store.StatusDown, b+1_000_000)
	e.changeState(web, store.StatusDown, store.StatusUp, b+1_060_000)
	second := e.changeState(web, store.StatusUp, store.StatusDown, b+2_000_000)
	third := e.changeState(db, store.StatusPending, store.StatusDown, b+3_000_000)

	_, err := e.metrics.InsertHeartbeats(context.Background(), []store.Heartbeat{{NodeID: "n1", Incarnation: "i", Seq: 1,
		TenantID: cluster.DefaultTenantID, MonitorID: web, TsMs: b + 2_000_000, Status: store.CheckDown, LatencyMs: 42}})
	if err != nil {
		t.Fatal(err)
	}
	m := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+web, viewer, nil))
	if m.Status != gen.MonitorStatusDOWN || !m.StatusSince.Equal(time.UnixMilli(b+2_000_000)) ||
		m.LastCheckAt == nil || !m.LastCheckAt.Equal(time.UnixMilli(b+2_000_000)) || *m.LastLatencyMs != 42 {
		t.Fatalf("monitor = %+v", m)
	}
	if m := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+db, viewer, nil)); m.LastCheckAt != nil {
		t.Errorf("lastCheckAt without heartbeats = %v", m.LastCheckAt)
	}

	page := decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?status=DOWN", viewer, nil))
	if len(page.Items) != 2 {
		t.Errorf("DOWN monitors = %d", len(page.Items))
	}
	if page := decode[gen.MonitorPage](t, e.do("GET", "/api/v1/monitors?status=UP", viewer, nil)); len(page.Items) != 0 {
		t.Errorf("UP monitors = %d", len(page.Items))
	}

	ids := func(p gen.IncidentPage) []string {
		var out []string
		for _, i := range p.Items {
			out = append(out, i.Id.String())
		}
		return out
	}
	list := decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents", viewer, nil))
	if got := ids(list); len(got) != 3 || got[0] != third || got[1] != second || got[2] != first {
		t.Fatalf("incidents = %v", got)
	}
	if got := ids(decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents?open=true&monitorId="+web, viewer, nil))); len(got) != 1 || got[0] != second {
		t.Errorf("open incidents of web = %v", got)
	}
	q := url.Values{"from": {time.UnixMilli(b + 1_030_000).UTC().Format(time.RFC3339)}, "to": {time.UnixMilli(b + 1_500_000).UTC().Format(time.RFC3339)}}
	if got := ids(decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents?"+q.Encode(), viewer, nil))); len(got) != 1 || got[0] != first {
		t.Errorf("incidents in period = %v", got)
	}

	page1 := decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents?limit=2", viewer, nil))
	if len(page1.Items) != 2 || page1.NextCursor == nil {
		t.Fatalf("page 1 = %+v", page1)
	}
	page2 := decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents?limit=2&cursor="+*page1.NextCursor, viewer, nil))
	if got := ids(page2); len(got) != 1 || got[0] != first || page2.NextCursor != nil {
		t.Errorf("page 2 = %v", got)
	}
	if res := e.do("GET", "/api/v1/incidents?cursor=bogus", viewer, nil); res.code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d", res.code)
	}

	res := e.do("GET", "/api/v1/incidents/"+first, viewer, nil)
	inc := decode[gen.Incident](t, res)
	if res.code != http.StatusOK || inc.MonitorName != "web" || inc.Cause != "connection refused" ||
		*inc.ErrorClass != gen.Connect || inc.ResolvedAt == nil || !inc.ResolvedAt.Equal(time.UnixMilli(b+1_060_000)) ||
		len(*inc.AffectedNodes) != 1 || !*inc.Confirmed {
		t.Fatalf("incident = %d %+v", res.code, inc)
	}
	if res := e.do("GET", "/api/v1/incidents/"+uuid.NewString(), viewer, nil); res.code != http.StatusNotFound {
		t.Errorf("unknown incident = %d", res.code)
	}

	// Tenant isolation (FA-TEN-01) and authentication.
	if res := e.do("GET", "/api/v1/incidents/"+first, other, nil); res.code != http.StatusNotFound {
		t.Errorf("foreign incident = %d", res.code)
	}
	if got := ids(decode[gen.IncidentPage](t, e.do("GET", "/api/v1/incidents", other, nil))); len(got) != 0 {
		t.Errorf("tenant B sees %v", got)
	}
	if res := e.do("GET", "/api/v1/incidents", "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", res.code)
	}
}

// TestUptime covers GET /monitors/{id}/uptime and uptime24h (FA-UPT-01).
func TestUptime(t *testing.T) {
	e := newEnv(t)
	id := e.createMonitor("web")
	// The monitor was UP for 10 minutes after its creation, then DOWN for 10
	// minutes until the API's now.
	start := time.Now().UnixMilli() + 1000
	now := start + 20*60_000
	e.now = time.UnixMilli(now)
	e.changeState(id, store.StatusPending, store.StatusUp, start)
	e.changeState(id, store.StatusUp, store.StatusDown, now-10*60_000)
	var hbs []store.Heartbeat
	for i := range 20 {
		s, lat := store.CheckUp, 80
		if i >= 10 {
			s, lat = store.CheckDown, 0
		}
		hbs = append(hbs, store.Heartbeat{NodeID: "n1", Incarnation: "i", Seq: uint64(i + 1), TenantID: cluster.DefaultTenantID,
			MonitorID: id, TsMs: start + int64(i)*60_000, Status: s, LatencyMs: lat})
	}
	if _, err := e.metrics.InsertHeartbeats(context.Background(), hbs); err != nil {
		t.Fatal(err)
	}

	res := e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=24h", viewer, nil)
	stats := decode[gen.UptimeStats](t, res)
	if res.code != http.StatusOK || stats.UptimePercent == nil || *stats.UptimePercent != 50 ||
		len(stats.Buckets) != 24 || stats.NodeId != nil || *stats.LatencyAvgMs != 80 {
		t.Fatalf("uptime = %d %s", res.code, res.body)
	}
	res = e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=24h&node=n1", viewer, nil)
	node := decode[gen.UptimeStats](t, res)
	if res.code != http.StatusOK || *node.NodeId != "n1" || node.UpSeconds != 600 || node.DownSeconds != 600 {
		t.Fatalf("node uptime = %d %s", res.code, res.body)
	}
	if res := e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=365d", viewer, nil); res.code != http.StatusOK ||
		len(decode[gen.UptimeStats](t, res).Buckets) != 365 {
		t.Fatalf("365d = %d %s", res.code, res.body)
	}

	m := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+id, viewer, nil))
	if m.Uptime24h == nil || *m.Uptime24h != *stats.UptimePercent {
		t.Errorf("uptime24h = %v, want %v", m.Uptime24h, *stats.UptimePercent)
	}

	if res := e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=1y", viewer, nil); res.code != http.StatusBadRequest {
		t.Errorf("invalid range = %d", res.code)
	}
	if res := e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=24h", other, nil); res.code != http.StatusNotFound {
		t.Errorf("foreign monitor = %d", res.code)
	}
	if res := e.do("GET", "/api/v1/monitors/"+id+"/uptime?range=24h", "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", res.code)
	}
}

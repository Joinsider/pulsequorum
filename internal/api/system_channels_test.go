// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/joinsider/pulsequorum/internal/alert"
	"github.com/joinsider/pulsequorum/internal/api/gen"
	"github.com/joinsider/pulsequorum/internal/cluster"
	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
	"github.com/joinsider/pulsequorum/internal/store"
)

const (
	systemChannelsPath = "/api/v1/admin/system-channels"
	systemAlertsPath   = "/api/v1/admin/system-alerts"
	// systemLevel is the command level that admits the event "system" (ADR 0025).
	systemLevel = 6
)

// enableSystemChannels prepares the test cluster for system channels: command
// level 6 admits the event system, and the reserved tenant exists as the
// bootstrap creates it, by a system actor (ADR 0025).
func enableSystemChannels(t *testing.T, e *env) {
	t.Helper()
	ctx := context.Background()
	if err := e.state.Update(ctx, func(tx store.StateTx) error {
		return tx.SetCommandLevel(ctx, systemLevel)
	}); err != nil {
		t.Fatal(err)
	}
	cmd, err := cluster.NewPutTenant(uuid.NewString(), store.SystemTenantID, cluster.TenantDocument{Name: "System", Plan: "system"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.Actor = &pb.Actor{Kind: pb.Actor_KIND_SYSTEM}
	if _, err := e.node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
}

// createSystemChannel creates a system channel as platform admin and fails the
// test on any other status.
func createSystemChannel(t *testing.T, e *env, body map[string]any) gen.NotificationChannel {
	t.Helper()
	res := e.do("POST", systemChannelsPath, platformAdmin, body)
	if res.code != http.StatusCreated {
		t.Fatalf("create system channel = %d %s", res.code, res.body)
	}
	return decode[gen.NotificationChannel](t, res)
}

func listedIDs(chs []gen.NotificationChannel) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(chs))
	for _, ch := range chs {
		ids = append(ids, ch.Id)
	}
	return ids
}

// TestSystemOperationsRequirePlatformAdmin: every system operation needs the
// platform-admin role, tenant admins included (ADR 0025).
func TestSystemOperationsRequirePlatformAdmin(t *testing.T) {
	e := newEnv(t)
	missing := uuid.NewString()
	createBody := map[string]any{"name": "sys", "kind": "shoutrrr", "url": shoutrrrURL}
	ops := []struct {
		name, method, path string
		body               any
	}{
		{"list system channels", "GET", systemChannelsPath, nil},
		{"create system channel", "POST", systemChannelsPath, createBody},
		{"get system channel", "GET", systemChannelsPath + "/" + missing, nil},
		{"update system channel", "PUT", systemChannelsPath + "/" + missing, createBody},
		{"delete system channel", "DELETE", systemChannelsPath + "/" + missing, nil},
		{"test system channel", "POST", systemChannelsPath + "/" + missing + "/test", nil},
		{"list system alerts", "GET", systemAlertsPath, nil},
	}
	for _, o := range ops {
		if res := e.do(o.method, o.path, "", o.body); res.code != http.StatusUnauthorized {
			t.Errorf("%s: anonymous = %d, want 401", o.name, res.code)
		}
		for _, p := range []string{tenantAdmin, tenantEditor, tenantViewer} {
			if res := e.do(o.method, o.path, p, o.body); res.code != http.StatusForbidden {
				t.Errorf("%s as %s = %d, want 403: %s", o.name, p, res.code, res.body)
			}
		}
		if res := e.do(o.method, o.path, platformAdmin, o.body); res.code == http.StatusForbidden || res.code == http.StatusUnauthorized {
			t.Errorf("%s as platform-admin = %d: %s", o.name, res.code, res.body)
		}
	}
}

// TestCreateSystemChannelLandsInSystemTenant: the tenant is the system tenant
// whatever the request says, the event system is always included, and no
// tenant sees the channel.
func TestCreateSystemChannelLandsInSystemTenant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	enableSystemChannels(t, e)

	ch := createSystemChannel(t, e, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL,
		"tenantId": "default"})
	if len(ch.Events) != 1 || ch.Events[0] != gen.System {
		t.Errorf("default events = %v, want [system]", ch.Events)
	}
	if ch.IsDefault {
		t.Errorf("system channel is default = true")
	}
	stored, err := e.state.GetChannel(ctx, store.SystemTenantID, ch.Id.String())
	if err != nil {
		t.Fatalf("channel not in the system tenant: %v", err)
	}
	if stored.TenantID != store.SystemTenantID || !slices.Equal(stored.Events, []string{store.EventSystem}) {
		t.Errorf("stored channel = tenant %q events %v", stored.TenantID, stored.Events)
	}
	if _, err := e.state.GetChannel(ctx, "default", ch.Id.String()); err == nil {
		t.Errorf("system channel is visible in the default tenant")
	}

	// Requested events are kept, system is added.
	withDown := createSystemChannel(t, e, map[string]any{"name": "ops-down", "kind": "shoutrrr", "url": shoutrrrURL,
		"events": []string{store.EventDown}})
	if len(withDown.Events) != 2 || !slices.Contains(withDown.Events, gen.NotificationEvent(store.EventDown)) ||
		!slices.Contains(withDown.Events, gen.System) {
		t.Errorf("events = %v, want [down system]", withDown.Events)
	}

	// Tenant endpoints neither list nor serve the system channel.
	tenantList := decode[[]gen.NotificationChannel](t, e.do("GET", channelsPath, tenantAdmin, nil))
	if slices.Contains(listedIDs(tenantList), ch.Id) {
		t.Errorf("tenant list contains the system channel")
	}
	if res := e.do("GET", channelsPath+"/"+ch.Id.String(), tenantAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("tenant get of system channel = %d, want 404", res.code)
	}

	// The system list contains it.
	sysList := decode[[]gen.NotificationChannel](t, e.do("GET", systemChannelsPath, platformAdmin, nil))
	if !slices.Contains(listedIDs(sysList), ch.Id) {
		t.Errorf("system list lacks the channel: %v", sysList)
	}
}

// TestTenantChannelRejectsSystemEvent: tenant channels answer 400 for the
// reserved event, on create and on update, and never store it.
func TestTenantChannelRejectsSystemEvent(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)

	res := e.do("POST", channelsPath, tenantAdmin, map[string]any{"name": "t", "kind": "shoutrrr", "url": shoutrrrURL,
		"events": []string{store.EventSystem}})
	if res.code != http.StatusBadRequest {
		t.Fatalf("create with system = %d, want 400: %s", res.code, res.body)
	}
	if d := problemDetail(t, res); !strings.Contains(d, "reserved") {
		t.Errorf("detail = %q", d)
	}

	ch := createChannel(t, e, tenantAdmin, map[string]any{"name": "t", "kind": "shoutrrr", "url": shoutrrrURL})
	res = e.do("PUT", channelsPath+"/"+ch.Id.String(), tenantAdmin, map[string]any{"name": "t", "kind": "shoutrrr",
		"events": []string{store.EventDown, store.EventSystem}})
	if res.code != http.StatusBadRequest {
		t.Fatalf("update with system = %d, want 400: %s", res.code, res.body)
	}

	for _, c := range decode[[]gen.NotificationChannel](t, e.do("GET", channelsPath, tenantAdmin, nil)) {
		if slices.Contains(c.Events, gen.System) {
			t.Errorf("tenant channel %s has event system", c.Id)
		}
	}
}

// TestSystemChannelSecretsMasked: URLs and header values of system channels
// never leave the API in plaintext, in any response.
func TestSystemChannelSecretsMasked(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)
	srv, rec := newRecorder(t, http.StatusOK)
	secretURL := srv.URL + "/" + webhookPathPart

	created := e.do("POST", systemChannelsPath, platformAdmin,
		webhookBody("sys-hook", secretURL, map[string]string{"Authorization": webhookSecret}))
	if created.code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.code, created.body)
	}
	ch := decode[gen.NotificationChannel](t, created)
	responses := []response{
		created,
		e.do("GET", systemChannelsPath+"/"+ch.Id.String(), platformAdmin, nil),
		e.do("GET", systemChannelsPath, platformAdmin, nil),
	}
	for _, res := range responses {
		for _, leak := range []string{webhookPathPart, "sekret-token"} {
			if strings.Contains(string(res.body), leak) {
				t.Errorf("response contains %q: %s", leak, res.body)
			}
		}
	}

	// An update with the masked values keeps the stored secrets, so the test
	// send still reaches the receiver with the original path and header.
	if ch.Webhook == nil {
		t.Fatal("webhook settings missing from the response")
	}
	update := map[string]any{"name": "sys-hook", "kind": "webhook", "webhook": map[string]any{
		"url": ch.Webhook.Url, "method": "POST", "headers": map[string]string{"Authorization": alert.Mask}}}
	if res := e.do("PUT", systemChannelsPath+"/"+ch.Id.String(), platformAdmin, update); res.code != http.StatusOK {
		t.Fatalf("update = %d %s", res.code, res.body)
	}
	res := e.do("POST", systemChannelsPath+"/"+ch.Id.String()+"/test", platformAdmin, nil)
	if result := decode[gen.DeliveryResult](t, res); res.code != http.StatusOK || !result.Delivered {
		t.Fatalf("test after masked update = %d %s", res.code, res.body)
	}
	got := rec.received()
	if len(got) != 1 || got[0].path != "/"+webhookPathPart || got[0].auth != webhookSecret {
		t.Errorf("receiver got %+v, want the stored path and header", got)
	}
}

// TestSystemChannelTestSend: a test of a system channel carries the event
// system and reports the delivery.
func TestSystemChannelTestSend(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)
	srv, rec := newRecorder(t, http.StatusOK)
	ch := createSystemChannel(t, e, webhookBody("sys-hook", srv.URL+"/hook", nil))

	res := e.do("POST", systemChannelsPath+"/"+ch.Id.String()+"/test", platformAdmin, nil)
	result := decode[gen.DeliveryResult](t, res)
	if res.code != http.StatusOK || !result.Delivered || result.Error != nil {
		t.Fatalf("test = %d %s", res.code, res.body)
	}
	got := rec.received()
	if len(got) != 1 {
		t.Fatalf("receiver got %d requests, want 1", len(got))
	}
	var payload map[string]any
	if err := json.Unmarshal(got[0].body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, got[0].body)
	}
	if payload["event"] != store.EventSystem {
		t.Errorf("event = %v, want %q: %s", payload["event"], store.EventSystem, got[0].body)
	}
}

// TestSystemChannelsAreIsolatedFromTenants: platform admins reach only the
// system tenant through the system endpoints, and tenant channels are not
// found there.
func TestSystemChannelsAreIsolatedFromTenants(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)
	tenantCh := createChannel(t, e, tenantAdmin, map[string]any{"name": "t", "kind": "shoutrrr", "url": shoutrrrURL})
	id := tenantCh.Id.String()

	checks := []struct {
		name, method, path string
		body               any
	}{
		{"get", "GET", systemChannelsPath + "/" + id, nil},
		{"update", "PUT", systemChannelsPath + "/" + id, map[string]any{"name": "x", "kind": "shoutrrr"}},
		{"delete", "DELETE", systemChannelsPath + "/" + id, nil},
		{"test", "POST", systemChannelsPath + "/" + id + "/test", nil},
	}
	for _, c := range checks {
		if res := e.do(c.method, c.path, platformAdmin, c.body); res.code != http.StatusNotFound {
			t.Errorf("system %s of a tenant channel = %d, want 404: %s", c.name, res.code, res.body)
		}
	}
	if res := e.do("GET", channelsPath+"/"+id, tenantAdmin, nil); res.code != http.StatusOK {
		t.Errorf("tenant channel after system probes = %d, want 200", res.code)
	}
}

// TestUpdateAndDeleteSystemChannel: updates keep the system event, deletes
// remove the channel.
func TestUpdateAndDeleteSystemChannel(t *testing.T) {
	e := newEnv(t)
	enableSystemChannels(t, e)
	ch := createSystemChannel(t, e, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL})
	path := systemChannelsPath + "/" + ch.Id.String()

	res := e.do("PUT", path, platformAdmin, map[string]any{"name": "ops-renamed", "kind": "shoutrrr",
		"events": []string{store.EventDown}})
	updated := decode[gen.NotificationChannel](t, res)
	if res.code != http.StatusOK || updated.Name != "ops-renamed" {
		t.Fatalf("update = %d %s", res.code, res.body)
	}
	if !slices.Contains(updated.Events, gen.System) {
		t.Errorf("update dropped the system event: %v", updated.Events)
	}
	if updated.MaskedUrl == nil || strings.Contains(*updated.MaskedUrl, "secretpass") {
		t.Errorf("masked url = %v", updated.MaskedUrl)
	}

	if res := e.do("DELETE", path, platformAdmin, nil); res.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if res := e.do("GET", path, platformAdmin, nil); res.code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", res.code)
	}
}

// TestListSystemAlerts: the default lists active alerts only, active=false
// lists all of them, and details are objects.
func TestListSystemAlerts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if err := e.state.Update(ctx, func(tx store.StateTx) error {
		if err := tx.PutSystemAlert(ctx, store.SystemAlert{Kind: "voter_offline", Subject: "n2", Active: true,
			SinceMs: now, UpdatedMs: now, DetailsJSON: json.RawMessage(`{"node":"n2"}`)}); err != nil {
			return err
		}
		return tx.PutSystemAlert(ctx, store.SystemAlert{Kind: "clock_skew", Subject: "n3", Active: false,
			SinceMs: now, UpdatedMs: now})
	}); err != nil {
		t.Fatal(err)
	}

	active := decode[[]gen.SystemAlert](t, e.do("GET", systemAlertsPath, platformAdmin, nil))
	if len(active) != 1 || active[0].Kind != "voter_offline" || active[0].Subject != "n2" || !active[0].Active {
		t.Fatalf("active alerts = %+v", active)
	}
	if active[0].Details["node"] != "n2" {
		t.Errorf("details = %v", active[0].Details)
	}
	if active[0].Since.UnixMilli() != now {
		t.Errorf("since = %v, want %d ms", active[0].Since, now)
	}

	all := decode[[]gen.SystemAlert](t, e.do("GET", systemAlertsPath+"?active=false", platformAdmin, nil))
	if len(all) != 2 {
		t.Fatalf("all alerts = %+v, want 2", all)
	}
	for _, a := range all {
		if a.Kind == "clock_skew" && (a.Active || a.Details == nil || len(a.Details) != 0) {
			t.Errorf("cleared alert = %+v, want inactive with empty details", a)
		}
	}
}

// TestSystemChannelBeforeSystemTenant: until the leader created the system
// tenant, creating a system channel answers 503 with Retry-After, not 404.
func TestSystemChannelBeforeSystemTenant(t *testing.T) {
	e := newEnv(t)
	res := e.do("POST", systemChannelsPath, platformAdmin, webhookBody("sys-hook", "https://example.com/hook", nil))
	if res.code != http.StatusServiceUnavailable {
		t.Fatalf("create before the system tenant = %d %s, want 503", res.code, res.body)
	}
}

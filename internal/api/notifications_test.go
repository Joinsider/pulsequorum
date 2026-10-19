// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/secrets"
	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	admin           = "default:tenant-admin"
	shoutrrrURL     = "ntfy://user:secretpass@ntfy.example.com/topic" //nolint:gosec // G101: test fixture, not a credential
	channelsPath    = "/api/v1/notifications"
	webhookSecret   = "Bearer sekret-token" //nolint:gosec // G101: test fixture, not a credential
	webhookPathPart = "hook-secret-path"
)

// newTestKeyring returns a keyring with one data key, as in internal/alert.
func newTestKeyring(t *testing.T) *secrets.Keyring {
	t.Helper()
	kek, err := secrets.LoadKEK("", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), "")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := secrets.NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := kek.Wrap("key-1", dek)
	if err != nil {
		t.Fatal(err)
	}
	kr := secrets.NewKeyring(kek, func(context.Context) ([]store.SecretKey, error) {
		return []store.SecretKey{{KeyID: "key-1", WrappedDEK: wrapped, KEKFingerprint: kek.Fingerprint}}, nil
	})
	if err := kr.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return kr
}

// recorder is a webhook receiver that remembers the requests it got.
type recorder struct {
	mu     sync.Mutex
	status int
	reqs   []recorded
}

type recorded struct {
	path, auth string
	body       []byte
}

func newRecorder(t *testing.T, status int) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, recorded{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		rec.mu.Unlock()
		w.WriteHeader(rec.status)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (r *recorder) received() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func webhookBody(name, url string, headers map[string]string) map[string]any {
	wh := map[string]any{"url": url, "method": "POST"}
	if headers != nil {
		wh["headers"] = headers
	}
	return map[string]any{"name": name, "kind": "webhook", "webhook": wh}
}

// createChannel creates a channel as admin and fails the test on any other status.
func createChannel(t *testing.T, e *env, principal string, body map[string]any) gen.NotificationChannel {
	t.Helper()
	res := e.do("POST", channelsPath, principal, body)
	if res.code != http.StatusCreated {
		t.Fatalf("create channel = %d %s", res.code, res.body)
	}
	return decode[gen.NotificationChannel](t, res)
}

func TestCreateShoutrrrChannelMasksURL(t *testing.T) {
	e := newEnv(t)
	res := e.do("POST", channelsPath, admin, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL})
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	for _, leak := range []string{"secretpass", "topic"} {
		if strings.Contains(string(res.body), leak) {
			t.Errorf("response contains %q: %s", leak, res.body)
		}
	}
	ch := decode[gen.NotificationChannel](t, res)
	if ch.MaskedUrl == nil || *ch.MaskedUrl != "ntfy://***@ntfy.example.com/***" {
		t.Errorf("maskedUrl = %v", ch.MaskedUrl)
	}
	if len(ch.Events) != 2 || ch.Events[0] != gen.NotificationEvent(store.EventDown) || ch.Events[1] != gen.NotificationEvent(store.EventUp) {
		t.Errorf("default events = %v, want [down up]", ch.Events)
	}
	if ch.State != gen.NotificationChannelState(store.ChannelOK) || ch.Kind != gen.NotificationKind(store.ChannelShoutrrr) {
		t.Errorf("channel = %+v", ch)
	}
}

// TestChannelSecretsNeverReturned reads a channel back through GET and LIST and
// checks the raw bodies for the plaintext secrets.
func TestChannelSecretsNeverReturned(t *testing.T) {
	e := newEnv(t)
	srv, _ := newRecorder(t, http.StatusOK)
	secretURL := srv.URL + "/" + webhookPathPart
	ch := createChannel(t, e, admin, webhookBody("hook", secretURL, map[string]string{"Authorization": webhookSecret}))
	createChannel(t, e, admin, map[string]any{"name": "ntfy", "kind": "shoutrrr", "url": shoutrrrURL})

	get := e.do("GET", channelsPath+"/"+ch.Id.String(), viewer, nil)
	list := e.do("GET", channelsPath, viewer, nil)
	for _, res := range []response{get, list} {
		if res.code != http.StatusOK {
			t.Fatalf("read = %d %s", res.code, res.body)
		}
		for _, leak := range []string{"secretpass", "topic", "sekret-token", webhookPathPart} {
			if strings.Contains(string(res.body), leak) {
				t.Errorf("response contains %q: %s", leak, res.body)
			}
		}
	}
	got := decode[gen.NotificationChannel](t, get)
	if got.Webhook == nil || (*got.Webhook.Headers)["Authorization"] != "***" || !strings.Contains(got.Webhook.Url, "***") {
		t.Errorf("webhook view = %+v", got.Webhook)
	}
	if n := len(decode[[]gen.NotificationChannel](t, list)); n != 2 {
		t.Errorf("list = %d channels, want 2", n)
	}
}

func TestUpdateKeepsURLAndHeaders(t *testing.T) {
	e := newEnv(t)
	srv, rec := newRecorder(t, http.StatusOK)
	ch := createChannel(t, e, admin, webhookBody("hook", srv.URL+"/original", map[string]string{"Authorization": "Bearer original"}))
	id := ch.Id.String()

	// Masked URL and header values keep the stored secrets.
	upd := map[string]any{"name": "renamed", "kind": "webhook", "webhook": map[string]any{
		"url": "***", "method": "POST", "headers": map[string]string{"Authorization": "***"},
	}}
	res := e.do("PUT", channelsPath+"/"+id, admin, upd)
	if res.code != http.StatusOK || decode[gen.NotificationChannel](t, res).Name != "renamed" {
		t.Fatalf("update = %d %s", res.code, res.body)
	}
	test := decode[gen.DeliveryResult](t, e.do("POST", channelsPath+"/"+id+"/test", admin, nil))
	if !test.Delivered {
		t.Fatalf("test after update not delivered: %+v", test)
	}
	got := rec.received()
	if len(got) != 1 || got[0].path != "/original" || got[0].auth != "Bearer original" {
		t.Errorf("receiver got %+v, want path /original with the stored header", got)
	}

	// A shoutrrr update without url keeps the stored URL.
	sh := createChannel(t, e, admin, map[string]any{"name": "ntfy", "kind": "shoutrrr", "url": shoutrrrURL})
	res = e.do("PUT", channelsPath+"/"+sh.Id.String(), admin, map[string]any{"name": "ntfy 2", "kind": "shoutrrr"})
	if res.code != http.StatusOK {
		t.Fatalf("update without url = %d %s", res.code, res.body)
	}
	if masked := decode[gen.NotificationChannel](t, res).MaskedUrl; masked == nil || *masked != "ntfy://***@ntfy.example.com/***" {
		t.Errorf("masked url after update = %v", masked)
	}
}

func TestInvalidChannelInput(t *testing.T) {
	e := newEnv(t)
	srv, _ := newRecorder(t, http.StatusOK)
	tests := []struct {
		name string
		body map[string]any
	}{
		{"invalid shoutrrr URL", map[string]any{"name": "x", "kind": "shoutrrr", "url": "notascheme://x"}},
		{"shoutrrr without url", map[string]any{"name": "x", "kind": "shoutrrr"}},
		{"invalid template", map[string]any{"name": "x", "kind": "webhook", "webhook": map[string]any{
			"url": srv.URL, "bodyTemplate": `{"m":"{{nosuchvar}}"}`}}},
		{"unknown event", map[string]any{"name": "x", "kind": "shoutrrr", "url": shoutrrrURL, "events": []string{"sms"}}},
		{"non-http webhook", map[string]any{"name": "x", "kind": "webhook", "webhook": map[string]any{"url": "ftp://x"}}},
		{"content type header", map[string]any{"name": "x", "kind": "webhook", "webhook": map[string]any{
			"url": srv.URL, "headers": map[string]string{"Content-Type": "text/plain"}}}},
		{"unknown kind", map[string]any{"name": "x", "kind": "sms", "url": shoutrrrURL}},
		{"empty name", map[string]any{"name": " ", "kind": "shoutrrr", "url": shoutrrrURL}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.do("POST", channelsPath, admin, tt.body)
			if res.code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d %s, want 422", res.code, res.body)
			}
		})
	}
}

func TestChannelAuthorization(t *testing.T) {
	e := newEnv(t)
	ch := createChannel(t, e, admin, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL})
	id := ch.Id.String()
	body := map[string]any{"name": "x", "kind": "shoutrrr", "url": shoutrrrURL}

	for _, principal := range []string{editor, viewer} {
		if res := e.do("POST", channelsPath, principal, body); res.code != http.StatusForbidden {
			t.Errorf("%s create = %d, want 403", principal, res.code)
		}
		if res := e.do("PUT", channelsPath+"/"+id, principal, body); res.code != http.StatusForbidden {
			t.Errorf("%s update = %d, want 403", principal, res.code)
		}
		if res := e.do("DELETE", channelsPath+"/"+id, principal, nil); res.code != http.StatusForbidden {
			t.Errorf("%s delete = %d, want 403", principal, res.code)
		}
		if res := e.do("POST", channelsPath+"/"+id+"/test", principal, nil); res.code != http.StatusForbidden {
			t.Errorf("%s test = %d, want 403", principal, res.code)
		}
	}
	for _, principal := range []string{editor, viewer} {
		if res := e.do("GET", channelsPath, principal, nil); res.code != http.StatusOK {
			t.Errorf("%s list = %d, want 200", principal, res.code)
		}
	}
	if res := e.do("GET", channelsPath, "", nil); res.code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d, want 401", res.code)
	}
}

// TestChannelTenantIsolation checks that another tenant's admin cannot see or
// change the channels of this tenant (FA-TEN-01/02).
func TestChannelTenantIsolation(t *testing.T) {
	e := newEnv(t)
	ch := createChannel(t, e, admin, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL})
	id := ch.Id.String()
	body := map[string]any{"name": "hijacked", "kind": "shoutrrr", "url": "ntfy://evil.example.com/t"}

	for _, c := range []struct{ method, path string }{
		{"GET", channelsPath + "/" + id},
		{"PUT", channelsPath + "/" + id},
		{"DELETE", channelsPath + "/" + id},
		{"POST", channelsPath + "/" + id + "/test"},
	} {
		var b any
		if c.method == "PUT" {
			b = body
		}
		if res := e.do(c.method, c.path, other, b); res.code != http.StatusNotFound {
			t.Errorf("%s %s as other tenant = %d, want 404", c.method, c.path, res.code)
		}
	}
	if n := len(decode[[]gen.NotificationChannel](t, e.do("GET", channelsPath, other, nil))); n != 0 {
		t.Errorf("other tenant sees %d channels", n)
	}
	if got := decode[gen.NotificationChannel](t, e.do("GET", channelsPath+"/"+id, admin, nil)); got.Name != "ops" {
		t.Errorf("owner's channel changed: %+v", got)
	}
}

func TestTestNotificationChannel(t *testing.T) {
	e := newEnv(t)
	srv, rec := newRecorder(t, http.StatusOK)
	ch := createChannel(t, e, admin, webhookBody("hook", srv.URL+"/hook", nil))
	id := ch.Id.String()

	res := e.do("POST", channelsPath+"/"+id+"/test", admin, nil)
	result := decode[gen.DeliveryResult](t, res)
	if res.code != http.StatusOK || !result.Delivered || result.Error != nil || result.DurationMs == nil {
		t.Fatalf("test = %d %s", res.code, res.body)
	}
	got := rec.received()
	if len(got) != 1 || !json.Valid(got[0].body) {
		t.Fatalf("receiver got %+v, want one valid JSON body", got)
	}

	// A receiver that refuses the message reports the failure; the state stays ok.
	srv2, _ := newRecorder(t, http.StatusNotFound)
	bad := createChannel(t, e, admin, webhookBody("bad", srv2.URL+"/gone", nil))
	res = e.do("POST", channelsPath+"/"+bad.Id.String()+"/test", admin, nil)
	result = decode[gen.DeliveryResult](t, res)
	if res.code != http.StatusOK || result.Delivered || result.Error == nil || !strings.Contains(*result.Error, "404") {
		t.Errorf("failed test = %d %s", res.code, res.body)
	}
	after := decode[gen.NotificationChannel](t, e.do("GET", channelsPath+"/"+bad.Id.String(), admin, nil))
	if after.State != gen.NotificationChannelState(store.ChannelOK) || after.LastError != nil {
		t.Errorf("test changed the channel state: %+v", after)
	}
}

func TestDeleteChannel(t *testing.T) {
	e := newEnv(t)
	ch := createChannel(t, e, admin, map[string]any{"name": "ops", "kind": "shoutrrr", "url": shoutrrrURL})
	path := channelsPath + "/" + ch.Id.String()
	if res := e.do("DELETE", path, admin, nil); res.code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", res.code, res.body)
	}
	if res := e.do("GET", path, admin, nil); res.code != http.StatusNotFound {
		t.Errorf("get after delete = %d", res.code)
	}
	if res := e.do("DELETE", path, admin, nil); res.code != http.StatusNotFound {
		t.Errorf("second delete = %d", res.code)
	}
}

func TestMonitorChannelAssignment(t *testing.T) {
	e := newEnv(t)
	own := createChannel(t, e, admin, map[string]any{"name": "own", "kind": "shoutrrr", "url": shoutrrrURL})
	foreign := createChannel(t, e, other, map[string]any{"name": "foreign", "kind": "shoutrrr", "url": shoutrrrURL})
	def := createChannel(t, e, admin, map[string]any{"name": "default", "kind": "shoutrrr", "url": shoutrrrURL, "isDefault": true})

	withChannels := func(ids []string) map[string]any {
		b := httpMonitor("with-channels", "https://a")
		b["notificationChannelIds"] = ids
		return b
	}

	t.Run("own channel is assigned", func(t *testing.T) {
		m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, withChannels([]string{own.Id.String()})))
		if m.NotificationChannelIds == nil || len(*m.NotificationChannelIds) != 1 || (*m.NotificationChannelIds)[0] != own.Id {
			t.Errorf("channels = %v", m.NotificationChannelIds)
		}
	})

	t.Run("foreign and unknown channels are rejected", func(t *testing.T) {
		for _, id := range []string{foreign.Id.String(), "0192d5e0-0000-7000-8000-000000000000"} {
			res := e.do("POST", "/api/v1/monitors", editor, withChannels([]string{id}))
			if res.code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d %s, want 422", res.code, res.body)
			}
			p := decode[gen.Problem](t, res)
			if p.Errors == nil || len(*p.Errors) == 0 || (*p.Errors)[0].Field != "/notificationChannelIds" {
				t.Errorf("errors = %+v", p.Errors)
			}
		}
	})

	t.Run("omitted list takes default channels", func(t *testing.T) {
		m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, httpMonitor("defaults", "https://a")))
		if m.NotificationChannelIds == nil || len(*m.NotificationChannelIds) != 1 || (*m.NotificationChannelIds)[0] != def.Id {
			t.Errorf("channels = %v, want only the default channel", m.NotificationChannelIds)
		}
	})

	t.Run("explicit empty list means none", func(t *testing.T) {
		m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, withChannels([]string{})))
		if m.NotificationChannelIds == nil || len(*m.NotificationChannelIds) != 0 {
			t.Errorf("channels = %v, want none", m.NotificationChannelIds)
		}
	})

	t.Run("clone keeps channels", func(t *testing.T) {
		m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, withChannels([]string{own.Id.String()})))
		clone := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors/"+m.Id.String()+"/clone", editor, nil))
		if clone.NotificationChannelIds == nil || len(*clone.NotificationChannelIds) != 1 || (*clone.NotificationChannelIds)[0] != own.Id {
			t.Errorf("clone channels = %v", clone.NotificationChannelIds)
		}
	})

	t.Run("deleting a channel unlinks it from monitors", func(t *testing.T) {
		m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, withChannels([]string{own.Id.String()})))
		if res := e.do("DELETE", channelsPath+"/"+own.Id.String(), admin, nil); res.code != http.StatusNoContent {
			t.Fatalf("delete channel = %d %s", res.code, res.body)
		}
		got := decode[gen.Monitor](t, e.do("GET", "/api/v1/monitors/"+m.Id.String(), editor, nil))
		if got.NotificationChannelIds == nil || len(*got.NotificationChannelIds) != 0 {
			t.Errorf("channels after delete = %v, want none", got.NotificationChannelIds)
		}
	})
}

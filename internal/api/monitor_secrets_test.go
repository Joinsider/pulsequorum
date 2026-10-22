// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/joinsider/uptimecluster/internal/api/gen"
	"github.com/joinsider/uptimecluster/internal/checker"
	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/configsecret"
	"github.com/joinsider/uptimecluster/internal/store"
)

const (
	pwSecret     = "pw-secret-value-1"
	headerSecret = "hdr-secret-value-2"
)

// basicAuthTarget answers 200 only with the expected credentials and header.
func basicAuthTarget(t *testing.T, password, header string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if !ok || user != "bob" || pw != password || r.Header.Get("X-Api-Key") != header {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func securedMonitor(url, password, header string) map[string]any {
	return map[string]any{"name": "secured", "type": "http", "intervalSeconds": 60, "config": map[string]any{
		"url":     url,
		"headers": map[string]any{"X-Api-Key": header},
		"auth":    map[string]any{"type": "basic", "username": "bob", "password": password},
	}}
}

func assertNoPlaintext(t *testing.T, where string, data []byte, plain ...string) {
	t.Helper()
	for _, p := range plain {
		if strings.Contains(string(data), p) {
			t.Errorf("%s contains secret %q", where, p)
		}
	}
}

// ADR 0015: secrets are encrypted at rest, masked for every role and used
// in plaintext only when a check runs.
func TestMonitorSecretsAreEncryptedAndMasked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	target := basicAuthTarget(t, pwSecret, headerSecret)

	res := e.do("POST", "/api/v1/monitors", editor, securedMonitor(target.URL, pwSecret, headerSecret))
	if res.code != http.StatusCreated {
		t.Fatalf("create = %d %s", res.code, res.body)
	}
	assertNoPlaintext(t, "create response", res.body, pwSecret, headerSecret)
	m := decode[gen.Monitor](t, res)
	id := m.Id.String()
	auth := m.Config["auth"].(map[string]any)
	if auth["password"] != configsecret.Mask || auth["username"] != "bob" ||
		m.Config["headers"].(map[string]any)["X-Api-Key"] != configsecret.Mask {
		t.Errorf("config not masked: %v", m.Config)
	}

	for _, who := range []string{viewer, editor} {
		r := e.do("GET", "/api/v1/monitors/"+id, who, nil)
		assertNoPlaintext(t, "GET as "+who, r.body, pwSecret, headerSecret)
		r = e.do("GET", "/api/v1/monitors", who, nil)
		assertNoPlaintext(t, "list as "+who, r.body, pwSecret, headerSecret)
	}

	stored, err := e.state.GetMonitor(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPlaintext(t, "stored config", stored.Config, pwSecret, headerSecret)
	if len(stored.Secrets) != 2 {
		t.Fatalf("stored secrets = %v", stored.Secrets)
	}
	for ptr, c := range stored.Secrets {
		assertNoPlaintext(t, "ciphertext "+ptr, []byte(c), pwSecret, headerSecret)
	}

	// The on-demand test uses the decrypted credentials.
	res = e.do("POST", "/api/v1/monitors/"+id+"/test", editor, nil)
	if r := decode[gen.CheckResult](t, res); r.Status != gen.CheckStatusUP {
		t.Fatalf("test run with stored secrets = %s", res.body)
	}
	assertNoPlaintext(t, "test response", res.body, pwSecret, headerSecret)

	// So does the scheduler's resolver.
	cfg, err := configsecret.ResolveMonitor(ctx, e.keyring, stored)
	if err != nil || !strings.Contains(string(cfg), pwSecret) || !strings.Contains(string(cfg), headerSecret) {
		t.Fatalf("resolve = %s, %v", cfg, err)
	}
}

func TestMonitorSecretsUpdateSemantics(t *testing.T) {
	e := newEnv(t)
	target := basicAuthTarget(t, pwSecret, headerSecret)
	m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, securedMonitor(target.URL, pwSecret, headerSecret)))
	id := m.Id.String()
	testRun := func() gen.CheckStatus {
		return decode[gen.CheckResult](t, e.do("POST", "/api/v1/monitors/"+id+"/test", editor, nil)).Status
	}

	// Sending back what GET returned (masks) keeps everything.
	res := e.do("PUT", "/api/v1/monitors/"+id, editor, map[string]any{
		"name": "renamed", "type": "http", "intervalSeconds": 60, "config": m.Config,
	})
	if res.code != http.StatusOK || testRun() != gen.CheckStatusUP {
		t.Fatalf("update with masks = %d %s", res.code, res.body)
	}

	// Leaving out the password keeps it, too.
	res = e.do("PUT", "/api/v1/monitors/"+id, editor, map[string]any{
		"name": "renamed", "type": "http", "intervalSeconds": 60, "config": map[string]any{
			"url": target.URL, "headers": map[string]any{"X-Api-Key": "***"},
			"auth": map[string]any{"type": "basic", "username": "bob"},
		},
	})
	if res.code != http.StatusOK || testRun() != gen.CheckStatusUP {
		t.Fatalf("update without password = %d %s", res.code, res.body)
	}

	// A new value replaces the stored one.
	res = e.do("PUT", "/api/v1/monitors/"+id, editor, securedMonitor(target.URL, "wrong-password", "***"))
	if res.code != http.StatusOK || testRun() != gen.CheckStatusDOWN {
		t.Fatalf("update with new password = %d %s", res.code, res.body)
	}

	// A mask for a header that has no stored secret is a field error.
	res = e.do("PUT", "/api/v1/monitors/"+id, editor, map[string]any{
		"name": "x", "type": "http", "intervalSeconds": 60, "config": map[string]any{
			"url": target.URL, "headers": map[string]any{"X-Other": "***"},
		},
	})
	if p := decode[gen.Problem](t, res); res.code != http.StatusUnprocessableEntity || p.Errors == nil ||
		(*p.Errors)[0].Field != "/config/headers/X-Other" {
		t.Fatalf("unknown mask = %d %s", res.code, res.body)
	}

	// A type change drops all secrets: masks are no longer accepted.
	res = e.do("PUT", "/api/v1/monitors/"+id, editor, map[string]any{
		"name": "x", "type": "tcp", "intervalSeconds": 60, "config": map[string]any{"host": "127.0.0.1", "port": 1},
	})
	if res.code != http.StatusOK {
		t.Fatalf("type change = %d %s", res.code, res.body)
	}
	if stored, _ := e.state.GetMonitor(context.Background(), "default", id); len(stored.Secrets) != 0 {
		t.Errorf("secrets survived a type change: %v", stored.Secrets)
	}
}

func TestMonitorSecretsClone(t *testing.T) {
	e := newEnv(t)
	target := basicAuthTarget(t, pwSecret, headerSecret)
	m := decode[gen.Monitor](t, e.do("POST", "/api/v1/monitors", editor, securedMonitor(target.URL, pwSecret, headerSecret)))
	res := e.do("POST", "/api/v1/monitors/"+m.Id.String()+"/clone", editor, nil)
	if res.code != http.StatusCreated {
		t.Fatalf("clone = %d %s", res.code, res.body)
	}
	assertNoPlaintext(t, "clone response", res.body, pwSecret, headerSecret)
	clone := decode[gen.Monitor](t, res)
	if r := decode[gen.CheckResult](t, e.do("POST", "/api/v1/monitors/"+clone.Id.String()+"/test", editor, nil)); r.Status != gen.CheckStatusUP {
		t.Fatalf("clone cannot use the copied credentials: %+v", r)
	}
	// The clone's ciphertexts are bound to the clone, not to the source.
	src, _ := e.state.GetMonitor(context.Background(), "default", m.Id.String())
	cp, _ := e.state.GetMonitor(context.Background(), "default", clone.Id.String())
	for ptr := range src.Secrets {
		if src.Secrets[ptr] == cp.Secrets[ptr] {
			t.Errorf("%s: clone shares the source ciphertext", ptr)
		}
	}
}

// Monitors written before ADR 0015 are masked in responses at once and
// encrypted by the migration.
func TestMonitorSecretsMigration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	target := basicAuthTarget(t, pwSecret, headerSecret)
	id := uuid.NewString()
	legacy := cluster.MonitorDocument{Name: "legacy", Type: "http", IntervalSeconds: 60, TimeoutSeconds: 10,
		Retries: 1, RecoveryThreshold: 1, Assignment: store.Assignment{Mode: "all"}, ConsensusPolicy: "majority",
		Config: []byte(`{"url":"` + target.URL + `","headers":{"X-Api-Key":"` + headerSecret +
			`"},"auth":{"type":"basic","username":"bob","password":"` + pwSecret + `"}}`)}
	cmd, err := cluster.NewPutMonitor(uuid.NewString(), "default", id, legacy, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.node.Apply(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	res := e.do("GET", "/api/v1/monitors/"+id, viewer, nil)
	assertNoPlaintext(t, "legacy GET", res.body, pwSecret, headerSecret)

	opts := configsecret.MigrationOptions{
		State: e.state, Cluster: e.node, Keyring: e.keyring, Schemas: &configsecret.Schemas{Registry: checker.Default},
		IsLeader: func() bool { return true }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if n, err := configsecret.Migrate(ctx, opts); err != nil || n != 1 {
		t.Fatalf("migrate = %d, %v", n, err)
	}
	stored, _ := e.state.GetMonitor(ctx, "default", id)
	assertNoPlaintext(t, "migrated config", stored.Config, pwSecret, headerSecret)
	if len(stored.Secrets) != 2 {
		t.Fatalf("migrated secrets = %v", stored.Secrets)
	}
	if r := decode[gen.CheckResult](t, e.do("POST", "/api/v1/monitors/"+id+"/test", editor, nil)); r.Status != gen.CheckStatusUP {
		t.Fatalf("migrated monitor cannot check: %+v", r)
	}
	if n, err := configsecret.Migrate(ctx, opts); err != nil || n != 0 {
		t.Fatalf("second migrate = %d, %v (want idempotent)", n, err)
	}
}

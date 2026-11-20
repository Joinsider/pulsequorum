// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package authswitch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/config"
	"github.com/joinsider/pulsequorum/internal/store"
)

// fakeHintStore is an in-memory HintReader.
type fakeHintStore struct {
	tenants    []store.Tenant
	tenantsErr error
	users      map[string]store.User // key: tenant ID + "/" + user ID
	tokens     map[string][]store.APIToken
}

func (f *fakeHintStore) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	for _, t := range f.tenants {
		if t.ID == id {
			return t, nil
		}
	}
	return store.Tenant{}, store.ErrNotFound
}

func (f *fakeHintStore) ListTenants(_ context.Context) ([]store.Tenant, error) {
	return f.tenants, f.tenantsErr
}

func (f *fakeHintStore) GetUser(_ context.Context, tenantID, id string) (store.User, error) {
	if u, ok := f.users[tenantID+"/"+id]; ok {
		return u, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeHintStore) ListAPITokens(_ context.Context, tenantID string) ([]store.APIToken, error) {
	return f.tokens[tenantID], nil
}

// hintTime is the clock of the tests, in milliseconds 1_000_000.
var hintTime = time.UnixMilli(1_000_000)

func hintClock() time.Time { return hintTime }

func hintZitadelConfig(tenancy config.Tenancy) config.Config {
	c := config.Config{AuthMode: config.AuthOIDC}
	c.OIDC.Profile = config.ProfileZitadel
	c.OIDC.Tenancy = tenancy
	return c
}

func genericConfig() config.Config {
	c := config.Config{AuthMode: config.AuthOIDC}
	c.OIDC.Profile = config.ProfileGeneric
	return c
}

// unboundTenants is the state of a cluster after the switch: default and the
// system tenant have no organization, and t1 comes from local mode with none.
// t2 is bound.
func unboundTenants() []store.Tenant {
	return []store.Tenant{
		{ID: cluster.DefaultTenantID, Name: "Default"},
		{ID: store.SystemTenantID, Name: "System"},
		{ID: "t1", Name: "Team One"},
		{ID: "t2", Name: "Team Two", ZitadelOrgID: "org-2"},
	}
}

// warnLines returns the log lines at WARN level.
func warnLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "level=WARN") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestLogStartupHintsUnboundTenantMessage(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantMsg string
	}{
		{"zitadel multi", hintZitadelConfig(config.TenancyMulti),
			"bind it with PATCH /api/v1/admin/tenants/{tenantId} and orgId"},
		{"zitadel single", hintZitadelConfig(config.TenancySingle), "not reachable in this OIDC setup"},
		{"generic", genericConfig(), "not reachable in this OIDC setup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			st := &fakeHintStore{tenants: unboundTenants()}
			if err := LogStartupHints(t.Context(), tt.cfg, testLogger(&buf), st, hintClock); err != nil {
				t.Fatalf("LogStartupHints: %v", err)
			}
			warns := warnLines(buf.String())
			if len(warns) != 1 {
				t.Fatalf("got %d warn lines, want 1 (only t1 is unbound and not skipped): %v", len(warns), warns)
			}
			for _, want := range []string{"tenant_id=t1", "name=\"Team One\"", tt.wantMsg} {
				if !strings.Contains(warns[0], want) {
					t.Errorf("warn line lacks %q: %s", want, warns[0])
				}
			}
		})
	}
}

func TestLogStartupHintsSkipsDefaultAndSystem(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{tenants: []store.Tenant{
		{ID: cluster.DefaultTenantID, Name: "Default"},
		{ID: store.SystemTenantID, Name: "System"},
	}}
	if err := LogStartupHints(t.Context(), hintZitadelConfig(config.TenancyMulti), testLogger(&buf), st, hintClock); err != nil {
		t.Fatalf("LogStartupHints: %v", err)
	}
	if warns := warnLines(buf.String()); len(warns) != 0 {
		t.Errorf("got warn lines for default or system tenant: %v", warns)
	}
}

func TestLocalAPITokenCount(t *testing.T) {
	const future = 2_000_000
	const now = 1_000_000
	users := map[string]store.User{
		"t1/u1": {TenantID: "t1", ID: "u1"},
		"t2/u9": {TenantID: "t2", ID: "u9"},
	}
	token := func(id, kind, creator string, expires int64) store.APIToken {
		return store.APIToken{ID: id, TenantID: "t1", CreatedByKind: kind, CreatedByID: creator, ExpiresMs: expires}
	}
	tests := []struct {
		name   string
		tokens []store.APIToken
		want   int
	}{
		{"no tokens", nil, 0},
		{"token of a local user", []store.APIToken{token("tok_a", "user", "u1", future)}, 1},
		{"expired token is not counted", []store.APIToken{token("tok_a", "user", "u1", now)}, 0},
		{"creator not a local user of the tenant", []store.APIToken{token("tok_a", "user", "ghost", future)}, 0},
		{"user of another tenant is not local here", []store.APIToken{token("tok_a", "user", "u9", future)}, 0},
		{"token created by an API token", []store.APIToken{token("tok_a", "api_token", "u1", future)}, 0},
		{"mixed", []store.APIToken{
			token("tok_a", "user", "u1", future),
			token("tok_b", "user", "u1", now-1),
			token("tok_c", "user", "ghost", future),
			token("tok_d", "user", "u1", future),
		}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeHintStore{users: users, tokens: map[string][]store.APIToken{"t1": tt.tokens}}
			got, err := countLocalAPITokens(t.Context(), st, "t1", now)
			if err != nil {
				t.Fatalf("countLocalAPITokens: %v", err)
			}
			if got != tt.want {
				t.Errorf("count = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLogStartupHintsTokenInfo(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{
		tenants: []store.Tenant{{ID: cluster.DefaultTenantID, Name: "Default"}, {ID: "t2", Name: "Two", ZitadelOrgID: "org-2"}},
		users:   map[string]store.User{"default/u1": {TenantID: cluster.DefaultTenantID, ID: "u1"}},
		tokens: map[string][]store.APIToken{cluster.DefaultTenantID: {
			{ID: "tok_a", TenantID: cluster.DefaultTenantID, CreatedByKind: "user", CreatedByID: "u1",
				ExpiresMs: hintTime.UnixMilli() + 1000},
		}},
	}
	if err := LogStartupHints(t.Context(), genericConfig(), testLogger(&buf), st, hintClock); err != nil {
		t.Fatalf("LogStartupHints: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"level=INFO", "API tokens created by local users stay valid", "tenant_id=default", "count=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}
}

func TestLogStartupHintsNoTokenInfoWithoutLocalTokens(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{tenants: []store.Tenant{{ID: cluster.DefaultTenantID, Name: "Default"}}}
	if err := LogStartupHints(t.Context(), genericConfig(), testLogger(&buf), st, hintClock); err != nil {
		t.Fatalf("LogStartupHints: %v", err)
	}
	if strings.Contains(buf.String(), "API tokens created by local users") {
		t.Errorf("token hint logged without tokens: %s", buf.String())
	}
}

func TestLogStartupHintsListError(t *testing.T) {
	st := &fakeHintStore{tenantsErr: errors.New("db closed")}
	err := LogStartupHints(t.Context(), genericConfig(), testLogger(&bytes.Buffer{}), st, hintClock)
	if err == nil {
		t.Fatal("LogStartupHints returned no error for a failed tenant listing")
	}
}

func TestRunStartupHintsLogsOnceWhenLeaderAndBootstrapped(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{tenants: []store.Tenant{{ID: cluster.DefaultTenantID, Name: "Default"}, {ID: "t1", Name: "One"}}}
	done := make(chan struct{})
	go func() {
		RunStartupHints(t.Context(), hintZitadelConfig(config.TenancyMulti), testLogger(&buf), &fakeApplier{leader: true},
			st, hintClock, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunStartupHints did not return after logging")
	}
	if n := len(warnLines(buf.String())); n != 1 {
		t.Errorf("got %d warn lines, want 1", n)
	}
}

func TestRunStartupHintsWaitsForLeader(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{tenants: []store.Tenant{{ID: cluster.DefaultTenantID, Name: "Default"}, {ID: "t1", Name: "One"}}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	RunStartupHints(ctx, hintZitadelConfig(config.TenancyMulti), testLogger(&buf), &fakeApplier{leader: false},
		st, hintClock, time.Millisecond)
	if buf.Len() != 0 {
		t.Errorf("follower logged hints: %s", buf.String())
	}
}

func TestRunStartupHintsWaitsForDefaultTenant(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeHintStore{}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	RunStartupHints(ctx, hintZitadelConfig(config.TenancyMulti), testLogger(&buf), &fakeApplier{leader: true},
		st, hintClock, time.Millisecond)
	if buf.Len() != 0 {
		t.Errorf("hints logged before the default tenant exists: %s", buf.String())
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package authswitch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/uptimecluster/internal/cluster"
	"github.com/joinsider/uptimecluster/internal/config"
	pb "github.com/joinsider/uptimecluster/internal/pb/uptimecluster/v1"
	"github.com/joinsider/uptimecluster/internal/store"
)

// tenantResult is what fakeTenants returns for one GetTenant call.
type tenantResult struct {
	tenant store.Tenant
	err    error
}

// fakeTenants returns the results in order; the last one repeats.
type fakeTenants struct {
	results []tenantResult
	calls   int
}

func (f *fakeTenants) GetTenant(_ context.Context, _ string) (store.Tenant, error) {
	i := min(f.calls, len(f.results)-1)
	f.calls++
	return f.results[i].tenant, f.results[i].err
}

// fakeApplier records the commands and returns applyErr for each.
type fakeApplier struct {
	leader   bool
	applyErr error
	cmds     []*pb.Command
}

func (f *fakeApplier) IsLeader() bool { return f.leader }

func (f *fakeApplier) Apply(_ context.Context, cmd *pb.Command) (cluster.ApplyResult, error) {
	f.cmds = append(f.cmds, cmd)
	return cluster.ApplyResult{}, f.applyErr
}

// testLogger logs at debug level into buf.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

const platformOrg = "org-platform"

func TestTryBindPlatformOrg(t *testing.T) {
	boundElsewhere := store.Tenant{ID: cluster.DefaultTenantID, ZitadelOrgID: "org-other"}
	tests := []struct {
		name         string
		tenant       store.Tenant
		tenantErr    error
		leader       bool
		applyErr     error
		want         bool
		wantApplied  bool
		wantErrorLog bool
		wantInfoLog  string
	}{
		{name: "default tenant not found", tenantErr: store.ErrNotFound, leader: true, want: false},
		{name: "read error", tenantErr: errors.New("disk gone"), leader: true, want: false},
		{name: "already bound to the same organization", tenant: store.Tenant{ID: cluster.DefaultTenantID, ZitadelOrgID: platformOrg},
			leader: true, want: true},
		{name: "bound to another organization", tenant: boundElsewhere, leader: true, want: true, wantErrorLog: true},
		{name: "bound to another organization on a follower", tenant: boundElsewhere, leader: false, want: true, wantErrorLog: true},
		{name: "not leader, unbound", tenant: store.Tenant{ID: cluster.DefaultTenantID}, leader: false, want: false},
		{name: "leader applies the binding", tenant: store.Tenant{ID: cluster.DefaultTenantID}, leader: true,
			want: true, wantApplied: true, wantInfoLog: "default tenant bound"},
		{name: "command level too old waits", tenant: store.Tenant{ID: cluster.DefaultTenantID}, leader: true,
			applyErr: &cluster.CommandError{Code: cluster.CodeNotEnabled, Message: "voters are older"},
			want:     false, wantApplied: true, wantInfoLog: "waits until all voters"},
		{name: "conflict is final", tenant: store.Tenant{ID: cluster.DefaultTenantID}, leader: true,
			applyErr: &cluster.CommandError{Code: cluster.CodeConflict, Message: "organization already bound"},
			want:     true, wantApplied: true, wantErrorLog: true},
		{name: "non-command error retries", tenant: store.Tenant{ID: cluster.DefaultTenantID}, leader: true,
			applyErr: errors.New("no quorum"), want: false, wantApplied: true, wantErrorLog: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			st := &fakeTenants{results: []tenantResult{{tenant: tt.tenant, err: tt.tenantErr}}}
			n := &fakeApplier{leader: tt.leader, applyErr: tt.applyErr}

			got := TryBindPlatformOrg(t.Context(), testLogger(&buf), n, st, platformOrg)

			if got != tt.want {
				t.Errorf("TryBindPlatformOrg = %v, want %v", got, tt.want)
			}
			if applied := len(n.cmds) > 0; applied != tt.wantApplied {
				t.Errorf("command applied = %v, want %v", applied, tt.wantApplied)
			}
			if tt.wantApplied {
				cmd := n.cmds[0].GetBindTenantOrg()
				if cmd.GetTenantId() != cluster.DefaultTenantID || cmd.GetOrgId() != platformOrg {
					t.Errorf("command binds %q to %q, want %q to %q",
						cmd.GetTenantId(), cmd.GetOrgId(), cluster.DefaultTenantID, platformOrg)
				}
			}
			out := buf.String()
			if gotErr := strings.Contains(out, "level=ERROR"); gotErr != tt.wantErrorLog {
				t.Errorf("error logged = %v, want %v; log: %s", gotErr, tt.wantErrorLog, out)
			}
			if tt.wantInfoLog != "" && !strings.Contains(out, tt.wantInfoLog) {
				t.Errorf("log lacks %q; log: %s", tt.wantInfoLog, out)
			}
		})
	}
}

func TestTryBindPlatformOrgRemedyInLog(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeTenants{results: []tenantResult{{tenant: store.Tenant{ID: cluster.DefaultTenantID, ZitadelOrgID: "org-other"}}}}
	TryBindPlatformOrg(t.Context(), testLogger(&buf), &fakeApplier{leader: true}, st, platformOrg)
	out := buf.String()
	for _, want := range []string{"level=ERROR", "ADR 0032", "ZITADEL_PLATFORM_ORG_ID", "bound_org=org-other", "platform_org=" + platformOrg} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q; log: %s", want, out)
		}
	}
}

func TestBindPlatformOrgSkipsOtherModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  func() config.Config
	}{
		{"local mode", func() config.Config {
			return config.Config{AuthMode: config.AuthLocal}
		}},
		{"generic profile", func() config.Config {
			c := config.Config{AuthMode: config.AuthOIDC}
			c.OIDC.Profile = config.ProfileGeneric
			return c
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeTenants{results: []tenantResult{{tenant: store.Tenant{ID: cluster.DefaultTenantID}}}}
			n := &fakeApplier{leader: true}
			var buf bytes.Buffer
			BindPlatformOrg(t.Context(), tt.cfg(), testLogger(&buf), n, st)
			if st.calls != 0 || len(n.cmds) != 0 {
				t.Errorf("read %d times and applied %d commands, want none", st.calls, len(n.cmds))
			}
		})
	}
}

func zitadelConfig() config.Config {
	c := config.Config{AuthMode: config.AuthOIDC}
	c.OIDC.Profile = config.ProfileZitadel
	c.OIDC.PlatformOrgID = platformOrg
	return c
}

func TestBindPlatformOrgRetriesUntilBound(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeTenants{results: []tenantResult{
		{err: store.ErrNotFound},
		{err: store.ErrNotFound},
		{tenant: store.Tenant{ID: cluster.DefaultTenantID}},
	}}
	n := &fakeApplier{leader: true}
	bindPlatformOrgEvery(t.Context(), zitadelConfig(), testLogger(&buf), n, st, time.Millisecond)
	if st.calls != 3 {
		t.Errorf("GetTenant calls = %d, want 3", st.calls)
	}
	if len(n.cmds) != 1 {
		t.Errorf("applied %d commands, want 1", len(n.cmds))
	}
}

func TestBindPlatformOrgStopsWhenContextEnds(t *testing.T) {
	var buf bytes.Buffer
	st := &fakeTenants{results: []tenantResult{{err: store.ErrNotFound}}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		bindPlatformOrgEvery(ctx, zitadelConfig(), testLogger(&buf), &fakeApplier{}, st, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("BindPlatformOrg did not return after the context ended")
	}
}

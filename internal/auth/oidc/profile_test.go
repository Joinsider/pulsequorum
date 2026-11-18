// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/auth"
	"github.com/joinsider/uptimecluster/internal/store"
)

func rawClaims(t *testing.T, claims map[string]any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGenericProfile(t *testing.T) {
	mapped := map[string]auth.Role{"uptime-admins": auth.RoleTenantAdmin, "ops": auth.RoleTenantEditor, "root": auth.RolePlatformAdmin}
	tests := []struct {
		name    string
		claim   string
		roleMap map[string]auth.Role
		claims  map[string]any
		want    []auth.Role
		wantErr error
	}{
		{"array of role names", "roles", nil, map[string]any{"roles": []string{"tenant-viewer", "tenant-editor"}}, []auth.Role{auth.RoleTenantViewer, auth.RoleTenantEditor}, nil},
		{"single string", "roles", nil, map[string]any{"roles": "tenant-admin"}, []auth.Role{auth.RoleTenantAdmin}, nil},
		{"dot path", "realm_access.roles", nil, map[string]any{"realm_access": map[string]any{"roles": []string{"tenant-viewer"}}}, []auth.Role{auth.RoleTenantViewer}, nil},
		{"duplicates collapse", "roles", nil, map[string]any{"roles": []string{"tenant-viewer", "tenant-viewer"}}, []auth.Role{auth.RoleTenantViewer}, nil},
		{"unknown values ignored", "roles", nil, map[string]any{"roles": []string{"admin", "tenant-viewer"}}, []auth.Role{auth.RoleTenantViewer}, nil},
		{"only unknown values", "roles", nil, map[string]any{"roles": []string{"admin", "users"}}, nil, auth.ErrForbidden},
		{"claim missing", "roles", nil, map[string]any{"groups": []string{"tenant-admin"}}, nil, auth.ErrForbidden},
		{"path missing", "realm_access.roles", nil, map[string]any{"realm_access": map[string]any{}}, nil, auth.ErrForbidden},
		{"path through non-object", "realm_access.roles", nil, map[string]any{"realm_access": "x"}, nil, auth.ErrForbidden},
		{"wrong type", "roles", nil, map[string]any{"roles": []int{1}}, nil, auth.ErrForbidden},
		{"role map applies", "groups", mapped, map[string]any{"groups": []string{"ops", "root"}}, []auth.Role{auth.RoleTenantEditor, auth.RolePlatformAdmin}, nil},
		{"role map ignores raw role names", "groups", mapped, map[string]any{"groups": []string{"tenant-admin"}}, nil, auth.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := Generic{TenantID: "default", RolesClaim: tc.claim, RoleMap: tc.roleMap}
			id, err := g.Map(context.Background(), rawClaims(t, tc.claims))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.TenantID != "default" || !slices.Equal(id.Roles, tc.want) {
				t.Fatalf("identity = %+v, want default %v", id, tc.want)
			}
		})
	}
}

type fakeTenants map[string]store.Tenant

func (f fakeTenants) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	if t, ok := f[id]; ok {
		return t, nil
	}
	return store.Tenant{}, store.ErrNotFound
}

func (f fakeTenants) GetTenantByOrg(_ context.Context, org string) (store.Tenant, error) {
	for _, t := range f {
		if org != "" && t.ZitadelOrgID == org {
			return t, nil
		}
	}
	return store.Tenant{}, store.ErrNotFound
}

type failingTenants struct{}

func (failingTenants) GetTenantByOrg(context.Context, string) (store.Tenant, error) {
	return store.Tenant{}, errors.New("disk on fire")
}

func zitadelClaims(org string, roles map[string][]string) map[string]any {
	granted := map[string]any{}
	for role, orgs := range roles {
		m := map[string]string{}
		for _, o := range orgs {
			m[o] = o + ".example.test"
		}
		granted[role] = m
	}
	c := map[string]any{"urn:zitadel:iam:org:project:proj-1:roles": granted}
	if org != "" {
		c[claimResourceOwner] = org
	}
	return c
}

func TestZitadelProfile(t *testing.T) {
	tenants := fakeTenants{
		"default": {ID: "default", ZitadelOrgID: "org-platform"},
		"acme":    {ID: "acme", ZitadelOrgID: "org-acme"},
	}
	tests := []struct {
		name       string
		multi      bool
		claims     map[string]any
		wantTenant string
		want       []auth.Role
		wantErr    error
	}{
		{"single: platform org", false, zitadelClaims("org-platform", map[string][]string{"tenant-admin": {"org-platform"}}), "default", []auth.Role{auth.RoleTenantAdmin}, nil},
		{"single: platform admin", false, zitadelClaims("org-platform", map[string][]string{"platform-admin": {"org-platform"}}), "default", []auth.Role{auth.RolePlatformAdmin}, nil},
		{"single: other org rejected", false, zitadelClaims("org-acme", map[string][]string{"tenant-admin": {"org-acme"}}), "", nil, auth.ErrForbidden},
		{"no organization claim", false, zitadelClaims("", map[string][]string{"tenant-admin": {"org-platform"}}), "", nil, auth.ErrForbidden},
		{"role granted in another org only", false, zitadelClaims("org-platform", map[string][]string{"tenant-admin": {"org-acme"}}), "", nil, auth.ErrForbidden},
		{"only matching org counts", false, zitadelClaims("org-platform", map[string][]string{"tenant-admin": {"org-acme"}, "tenant-viewer": {"org-platform"}}), "default", []auth.Role{auth.RoleTenantViewer}, nil},
		{"unknown role names ignored", false, zitadelClaims("org-platform", map[string][]string{"owner": {"org-platform"}, "tenant-editor": {"org-platform"}}), "default", []auth.Role{auth.RoleTenantEditor}, nil},
		{"roles sorted", false, zitadelClaims("org-platform", map[string][]string{"tenant-viewer": {"org-platform"}, "tenant-admin": {"org-platform"}}), "default", []auth.Role{auth.RoleTenantAdmin, auth.RoleTenantViewer}, nil},
		{"multi: bound org", true, zitadelClaims("org-acme", map[string][]string{"tenant-editor": {"org-acme"}}), "acme", []auth.Role{auth.RoleTenantEditor}, nil},
		{"multi: platform org", true, zitadelClaims("org-platform", map[string][]string{"platform-admin": {"org-platform"}}), "default", []auth.Role{auth.RolePlatformAdmin}, nil},
		{"multi: customer cannot be platform admin", true, zitadelClaims("org-acme", map[string][]string{"platform-admin": {"org-acme"}}), "", nil, auth.ErrForbidden},
		{"multi: customer platform admin dropped, other roles kept", true, zitadelClaims("org-acme", map[string][]string{"platform-admin": {"org-acme"}, "tenant-viewer": {"org-acme"}}), "acme", []auth.Role{auth.RoleTenantViewer}, nil},
		{"multi: unknown org", true, zitadelClaims("org-evil", map[string][]string{"tenant-admin": {"org-evil"}}), "", nil, auth.ErrForbidden},
		{"roles of another project ignored", false, map[string]any{claimResourceOwner: "org-platform", "urn:zitadel:iam:org:project:proj-2:roles": map[string]any{"tenant-admin": map[string]string{"org-platform": "x"}}}, "", nil, auth.ErrForbidden},
		{"malformed roles claim", false, map[string]any{claimResourceOwner: "org-platform", "urn:zitadel:iam:org:project:proj-1:roles": []string{"tenant-admin"}}, "", nil, auth.ErrForbidden},
		{"malformed organization claim", false, map[string]any{claimResourceOwner: 42}, "", nil, auth.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := Zitadel{ProjectID: "proj-1", PlatformOrgID: "org-platform", Multi: tc.multi, DefaultTenantID: "default", Tenants: tenants}
			if tc.multi {
				// In multi tenancy the platform org is found through its binding.
				z.DefaultTenantID = ""
			}
			id, err := z.Map(context.Background(), rawClaims(t, tc.claims))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.TenantID != tc.wantTenant || !slices.Equal(id.Roles, tc.want) {
				t.Fatalf("identity = %+v, want %s %v", id, tc.wantTenant, tc.want)
			}
		})
	}
}

func TestZitadelProfileStoreFailureIsUnavailable(t *testing.T) {
	z := Zitadel{ProjectID: "proj-1", PlatformOrgID: "org-platform", Multi: true, Tenants: failingTenants{}}
	_, err := z.Map(context.Background(), rawClaims(t, zitadelClaims("org-acme", map[string][]string{"tenant-admin": {"org-acme"}})))
	if !errors.Is(err, auth.ErrAuthUnavailable) || errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("err = %v, want ErrAuthUnavailable", err)
	}
}

// fakeProvisioner records the organizations it was asked to provision.
type fakeProvisioner struct {
	calls    []string
	tenantID string
	err      error
}

func (p *fakeProvisioner) Provision(_ context.Context, orgID, orgName string) (string, error) {
	p.calls = append(p.calls, orgID+"|"+orgName)
	return p.tenantID, p.err
}

// TestZitadelProvisioning covers ADR 0029 (4A) at the profile: an unknown
// organization is provisioned only when a role is granted in that organization.
func TestZitadelProvisioning(t *testing.T) {
	tenants := fakeTenants{
		"default": {ID: "default", ZitadelOrgID: "org-platform"},
		"acme":    {ID: "acme", ZitadelOrgID: "org-acme"},
	}
	withName := func(c map[string]any, name string) map[string]any {
		c[claimResourceOwnerName] = name
		return c
	}
	tests := []struct {
		name      string
		claims    map[string]any
		prov      *fakeProvisioner // nil: no provisioner
		single    bool             // single tenancy
		wantTen   string
		want      []auth.Role
		wantErr   error
		wantMsg   string // substring of the error, if any
		wantCalls []string
	}{
		{
			name:      "unknown organization with a role in it is provisioned",
			claims:    zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-new"}}),
			prov:      &fakeProvisioner{tenantID: "new-tenant"},
			wantTen:   "new-tenant",
			want:      []auth.Role{auth.RoleTenantAdmin},
			wantCalls: []string{"org-new|"},
		},
		{
			name:      "organization name claim is passed to the provisioner",
			claims:    withName(zitadelClaims("org-new", map[string][]string{"tenant-viewer": {"org-new"}}), "Acme GmbH"),
			prov:      &fakeProvisioner{tenantID: "new-tenant"},
			wantTen:   "new-tenant",
			want:      []auth.Role{auth.RoleTenantViewer},
			wantCalls: []string{"org-new|Acme GmbH"},
		},
		{
			name:    "unknown organization with roles only in another organization is not provisioned",
			claims:  zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-acme"}}),
			prov:    &fakeProvisioner{tenantID: "new-tenant"},
			wantErr: auth.ErrForbidden,
			wantMsg: "not provisioned",
		},
		{
			name:    "unknown organization without any role is not provisioned",
			claims:  zitadelClaims("org-new", map[string][]string{}),
			prov:    &fakeProvisioner{tenantID: "new-tenant"},
			wantErr: auth.ErrForbidden,
			wantMsg: "not provisioned",
		},
		{
			name:    "unknown organization without provisioner is rejected",
			claims:  zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-new"}}),
			prov:    nil,
			wantErr: auth.ErrForbidden,
			wantMsg: "not provisioned",
		},
		{
			name:      "provisioner error is passed through",
			claims:    zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-new"}}),
			prov:      &fakeProvisioner{err: fmt.Errorf("%w: limit reached", auth.ErrForbidden)},
			wantErr:   auth.ErrForbidden,
			wantMsg:   "limit reached",
			wantCalls: []string{"org-new|"},
		},
		{
			name:      "provisioner unavailability is passed through",
			claims:    zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-new"}}),
			prov:      &fakeProvisioner{err: fmt.Errorf("%w: raft", auth.ErrAuthUnavailable)},
			wantErr:   auth.ErrAuthUnavailable,
			wantCalls: []string{"org-new|"},
		},
		{
			name:    "platform admin of a customer organization is dropped and does not provision alone",
			claims:  zitadelClaims("org-new", map[string][]string{"platform-admin": {"org-new"}}),
			prov:    &fakeProvisioner{tenantID: "new-tenant"},
			wantErr: auth.ErrForbidden,
		},
		{
			name:      "platform admin is dropped, other roles of the organization provision",
			claims:    zitadelClaims("org-new", map[string][]string{"platform-admin": {"org-new"}, "tenant-viewer": {"org-new"}}),
			prov:      &fakeProvisioner{tenantID: "new-tenant"},
			wantTen:   "new-tenant",
			want:      []auth.Role{auth.RoleTenantViewer},
			wantCalls: []string{"org-new|"},
		},
		{
			name:    "single tenancy never provisions",
			claims:  zitadelClaims("org-new", map[string][]string{"tenant-admin": {"org-new"}}),
			prov:    &fakeProvisioner{tenantID: "new-tenant"},
			single:  true,
			wantErr: auth.ErrForbidden,
		},
		{
			name:    "bound organization never provisions",
			claims:  zitadelClaims("org-acme", map[string][]string{"tenant-admin": {"org-acme"}}),
			prov:    &fakeProvisioner{tenantID: "new-tenant"},
			wantTen: "acme",
			want:    []auth.Role{auth.RoleTenantAdmin},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			z := Zitadel{ProjectID: "proj-1", PlatformOrgID: "org-platform", Multi: !tt.single,
				DefaultTenantID: "default", Tenants: tenants}
			if tt.single {
				z.DefaultTenantID = "default"
			} else {
				z.DefaultTenantID = ""
			}
			if tt.prov != nil {
				z.Provisioner = tt.prov
			}
			id, err := z.Map(context.Background(), rawClaims(t, tt.claims))
			if tt.prov != nil && !slices.Equal(tt.prov.calls, tt.wantCalls) {
				t.Errorf("provisioner calls = %v, want %v", tt.prov.calls, tt.wantCalls)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("err = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if id.TenantID != tt.wantTen || !slices.Equal(id.Roles, tt.want) {
				t.Fatalf("identity = %+v, want %s %v", id, tt.wantTen, tt.want)
			}
		})
	}
}

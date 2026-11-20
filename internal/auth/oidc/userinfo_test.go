// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package oidc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/auth"
)

// userInfoServer is a userinfo endpoint over plain HTTP; the key set under
// test allows HTTP for it.
type userInfoServer struct {
	calls   atomic.Int32
	status  atomic.Int32
	ctype   atomic.Value
	mu      sync.Mutex
	claims  map[string]any
	release chan struct{}
	lastTok atomic.Value
}

func newUserInfoServer(t *testing.T, claims map[string]any) (*userInfoServer, string) {
	t.Helper()
	u := &userInfoServer{claims: claims}
	u.status.Store(http.StatusOK)
	u.ctype.Store("application/json; charset=utf-8")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.lastTok.Store(r.Header.Get("Authorization"))
		if u.release != nil {
			<-u.release
		}
		w.Header().Set("Content-Type", u.ctype.Load().(string))
		w.WriteHeader(int(u.status.Load()))
		u.mu.Lock()
		defer u.mu.Unlock()
		_ = json.NewEncoder(w).Encode(u.claims)
	}))
	t.Cleanup(srv.Close)
	return u, srv.URL + "/userinfo"
}

// withUserInfo wires a userinfo source into the harness authenticator.
func withUserInfo(t *testing.T, h *harness, endpoint string) *UserInfo {
	t.Helper()
	h.keys.opts.AllowHTTP = true
	h.keys.mu.Lock()
	h.keys.userinfo = endpoint
	h.keys.mu.Unlock()
	ui := NewUserInfo(UserInfoOptions{Keys: h.keys, Now: h.clock.Now})
	h.authn.opts.UserInfo = ui
	return ui
}

func TestRolesFromUserInfo(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *KeySetOptions) {
		o.Profile = Generic{TenantID: "default", RolesClaim: "groups", RoleMap: map[string]auth.Role{"uc-admins": auth.RoleTenantAdmin}}
	})
	srv, endpoint := newUserInfoServer(t, map[string]any{"sub": "user-1", "groups": []string{"uc-admins"}})
	withUserInfo(t, h, endpoint)

	c := h.claims()
	delete(c, "roles") // like PocketID: no roles in the access token
	token := h.idp.Sign(c)
	for range 3 {
		p, err := h.authenticate(token)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(p.Roles, []auth.Role{auth.RoleTenantAdmin}) {
			t.Fatalf("roles = %v", p.Roles)
		}
	}
	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("userinfo calls = %d, want 1 (cached per token)", got)
	}
	if got := srv.lastTok.Load(); got != "Bearer "+token {
		t.Fatal("userinfo was not called with the access token")
	}

	// After the token expired, a new token asks again.
	h.clock.Advance(20 * time.Minute)
	if _, err := h.authenticate(h.idp.Sign(func() map[string]any { c := h.claims(); delete(c, "roles"); return c }())); err != nil {
		t.Fatal(err)
	}
	if got := srv.calls.Load(); got != 2 {
		t.Fatalf("userinfo calls = %d, want 2", got)
	}
}

func TestUserInfoFailures(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		status int32
		ctype  string
		want   error
	}{
		{"subject mismatch", map[string]any{"sub": "someone-else", "groups": []string{"tenant-admin"}}, 200, "application/json", auth.ErrUnauthenticated},
		{"no subject", map[string]any{"groups": []string{"tenant-admin"}}, 200, "application/json", auth.ErrUnauthenticated},
		{"token refused", map[string]any{}, http.StatusUnauthorized, "application/json", auth.ErrUnauthenticated},
		{"signed response", map[string]any{"sub": "user-1"}, 200, "application/jwt", auth.ErrUnauthenticated},
		{"provider error", map[string]any{}, http.StatusBadGateway, "application/json", auth.ErrAuthUnavailable},
		{"no role in userinfo", map[string]any{"sub": "user-1", "groups": []string{"other"}}, 200, "application/json", auth.ErrForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options, _ *KeySetOptions) {
				o.Profile = Generic{TenantID: "default", RolesClaim: "groups"}
			})
			srv, endpoint := newUserInfoServer(t, tc.claims)
			srv.status.Store(tc.status)
			srv.ctype.Store(tc.ctype)
			withUserInfo(t, h, endpoint)
			_, err := h.authenticate(h.idp.Sign(h.claims()))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUserInfoNotCalledForInvalidTokens(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *KeySetOptions) { o.Profile = Generic{TenantID: "default", RolesClaim: "groups"} })
	srv, endpoint := newUserInfoServer(t, map[string]any{"sub": "user-1", "groups": []string{"tenant-admin"}})
	withUserInfo(t, h, endpoint)
	c := h.claims()
	c["aud"] = []string{"other"}
	if _, err := h.authenticate(h.idp.Sign(c)); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
	if srv.calls.Load() != 0 {
		t.Fatal("userinfo called for an invalid token")
	}
}

func TestUserInfoWithoutEndpointIsUnavailable(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *KeySetOptions) { o.Profile = Generic{TenantID: "default", RolesClaim: "groups"} })
	withUserInfo(t, h, "")
	if _, err := h.authenticate(h.idp.Sign(h.claims())); !errors.Is(err, auth.ErrAuthUnavailable) {
		t.Fatalf("err = %v, want ErrAuthUnavailable", err)
	}
}

func TestUserInfoConcurrentSingleCall(t *testing.T) {
	h := newHarness(t, func(o *Options, _ *KeySetOptions) { o.Profile = Generic{TenantID: "default", RolesClaim: "groups"} })
	srv, endpoint := newUserInfoServer(t, map[string]any{"sub": "user-1", "groups": []string{"tenant-viewer"}})
	srv.release = make(chan struct{})
	withUserInfo(t, h, endpoint)
	token := h.idp.Sign(h.claims())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, err := h.authenticate(token)
			errs <- err
		})
	}
	time.Sleep(100 * time.Millisecond)
	close(srv.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := srv.calls.Load(); got != 1 {
		t.Fatalf("userinfo calls = %d, want 1", got)
	}
}

func TestDiscoveryRecordsUserInfo(t *testing.T) {
	h := newHarness(t)
	// The fake provider announces no userinfo endpoint.
	if got := h.keys.UserInfoEndpoint(); got != "" {
		t.Fatalf("userinfo endpoint = %q, want empty", got)
	}
}

// TestClientIDAsAZP covers RFC 9068 tokens with client_id instead of azp
// (ADR 0028, PocketID and Zitadel).
func TestClientIDAsAZP(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		mut  func(map[string]any)
		ok   bool
	}{
		{"client_id only", func(c map[string]any) { delete(c, "azp"); c["client_id"] = "spa-client" }, true},
		{"foreign client_id", func(c map[string]any) { delete(c, "azp"); c["client_id"] = "other-app" }, false},
		{"azp and client_id agree", func(c map[string]any) { c["client_id"] = "spa-client" }, true},
		{"azp and client_id differ", func(c map[string]any) { c["client_id"] = "other-app" }, false},
		{"neither", func(c map[string]any) { delete(c, "azp") }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := h.claims()
			tc.mut(c)
			_, err := h.authenticate(h.idp.Sign(c))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestDefaultRole(t *testing.T) {
	tests := []struct {
		name    string
		def     auth.Role
		claims  map[string]any
		want    []auth.Role
		wantErr bool
	}{
		{"no claim gets default", auth.RoleTenantViewer, map[string]any{}, []auth.Role{auth.RoleTenantViewer}, false},
		{"claim wins over default", auth.RoleTenantViewer, map[string]any{"roles": []string{"tenant-admin"}}, []auth.Role{auth.RoleTenantAdmin}, false},
		{"unknown values get default", auth.RoleTenantEditor, map[string]any{"roles": []string{"x"}}, []auth.Role{auth.RoleTenantEditor}, false},
		{"platform-admin is never a default", auth.RolePlatformAdmin, map[string]any{}, nil, true},
		{"malformed claim is not papered over", auth.RoleTenantViewer, map[string]any{"roles": 42}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := Generic{TenantID: "default", RolesClaim: "roles", DefaultRole: tc.def}
			id, err := g.Map(t.Context(), rawClaims(t, tc.claims))
			if tc.wantErr {
				if !errors.Is(err, auth.ErrForbidden) {
					t.Fatalf("err = %v, want ErrForbidden", err)
				}
				return
			}
			if err != nil || !slices.Equal(id.Roles, tc.want) {
				t.Fatalf("roles = %v, err = %v, want %v", id.Roles, err, tc.want)
			}
		})
	}
}

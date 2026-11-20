// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/joinsider/pulsequorum/internal/store"
)

type fakeTokenState struct {
	level   uint32
	tokens  map[string]store.APIToken // by hash
	tenants map[string]store.Tenant
	err     error
}

func (f *fakeTokenState) GetAPITokenBySecretHash(_ context.Context, h string) (store.APIToken, error) {
	if f.err != nil {
		return store.APIToken{}, f.err
	}
	t, ok := f.tokens[h]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeTokenState) GetTenant(_ context.Context, id string) (store.Tenant, error) {
	t, ok := f.tenants[id]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return t, nil
}

func (f *fakeTokenState) CommandLevel(context.Context) (uint32, error) { return f.level, nil }

type nextAuth struct{ called bool }

func (n *nextAuth) Authenticate(*http.Request) (Principal, error) {
	n.called = true
	return Principal{UserID: "jwt-user"}, nil
}

func TestNewAPIToken(t *testing.T) {
	id, secret, hash, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "tok_") || len(id) != 20 {
		t.Errorf("id = %q", id)
	}
	if !IsAPITokenSecret(secret) || len(secret) != apiTokenSecretLen {
		t.Errorf("secret has wrong form (len %d)", len(secret))
	}
	if hash != HashToken(secret) || strings.Contains(hash, secret) {
		t.Errorf("hash does not match the secret")
	}
	id2, secret2, _, _ := NewAPIToken()
	if id == id2 || secret == secret2 {
		t.Error("tokens repeat")
	}
}

func TestValidAPITokenRole(t *testing.T) {
	for role, want := range map[string]bool{
		"tenant-viewer": true, "tenant-editor": true, "tenant-admin": true,
		"platform-admin": false, "": false, "admin": false,
	} {
		if got := ValidAPITokenRole(role); got != want {
			t.Errorf("ValidAPITokenRole(%q) = %v", role, got)
		}
	}
}

func TestAPITokenAuthenticator(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	_, secret, hash, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	valid := store.APIToken{ID: "tok_a", TenantID: "acme", Scope: "tenant", Name: "ci", Role: "tenant-editor",
		SecretHash: hash, ExpiresMs: now.Add(time.Hour).UnixMilli()}
	mod := func(f func(*store.APIToken)) store.APIToken { c := valid; f(&c); return c }
	tenants := map[string]store.Tenant{"acme": {ID: "acme"}, "frozen": {ID: "frozen", Suspended: true}}

	tests := []struct {
		name   string
		level  uint32
		token  store.APIToken
		header string
		want   error // nil = success
	}{
		{"valid", 9, valid, "Bearer " + secret, nil},
		{"below level 9", 8, valid, "Bearer " + secret, ErrInvalidAPIToken},
		{"unknown", 9, mod(func(t *store.APIToken) { t.SecretHash = "x" }), "Bearer " + secret, ErrInvalidAPIToken},
		{"expired", 9, mod(func(t *store.APIToken) { t.ExpiresMs = now.UnixMilli() }), "Bearer " + secret, ErrInvalidAPIToken},
		{"platform role", 9, mod(func(t *store.APIToken) { t.Role = "platform-admin" }), "Bearer " + secret, ErrInvalidAPIToken},
		{"other scope", 9, mod(func(t *store.APIToken) { t.Scope = "provisioning" }), "Bearer " + secret, ErrInvalidAPIToken},
		{"system tenant", 9, mod(func(t *store.APIToken) { t.TenantID = store.SystemTenantID }), "Bearer " + secret, ErrInvalidAPIToken},
		{"deleted tenant", 9, mod(func(t *store.APIToken) { t.TenantID = "gone" }), "Bearer " + secret, ErrInvalidAPIToken},
		{"suspended tenant", 9, mod(func(t *store.APIToken) { t.TenantID = "frozen" }), "Bearer " + secret, ErrTenantSuspended},
		{"truncated", 9, valid, "Bearer " + secret[:len(secret)-1], ErrInvalidAPIToken},
		{"padded", 9, valid, "Bearer " + secret + "A", ErrInvalidAPIToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeTokenState{level: tt.level, tenants: tenants, tokens: map[string]store.APIToken{tt.token.SecretHash: tt.token}}
			next := &nextAuth{}
			a := WithAPITokens(next, st, func() time.Time { return now })
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Authorization", tt.header)
			p, err := a.Authenticate(r)
			if next.called {
				t.Fatal("token request reached the next authenticator")
			}
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err = %v, want %v", err, tt.want)
				}
				if err.Error() != tt.want.Error() {
					t.Fatalf("message %q leaks the case, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !p.ViaToken || p.UserID != "tok_a" || p.TenantID != "acme" || !p.Has(RoleTenantEditor) || p.Has(RolePlatformAdmin) {
				t.Fatalf("principal = %+v", p)
			}
			if !p.ExpiresAt.Equal(time.UnixMilli(valid.ExpiresMs)) {
				t.Fatalf("ExpiresAt = %v", p.ExpiresAt)
			}
			if !errors.Is(RequireInteractive(p), ErrForbidden) {
				t.Fatal("RequireInteractive admits a token")
			}
		})
	}
}

func TestAPITokenAuthenticatorPassesJWTs(t *testing.T) {
	next := &nextAuth{}
	a := WithAPITokens(next, &fakeTokenState{level: 9}, nil)
	for _, h := range []string{"Bearer eyJhbGciOi.x.y", "", "Basic dXNlcg=="} {
		next.called = false
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		if _, err := a.Authenticate(r); err != nil || !next.called {
			t.Errorf("%q: err=%v, next called=%v", h, err, next.called)
		}
	}
}

func TestAPITokenLookupFailureIsUnavailable(t *testing.T) {
	_, secret, _, _ := NewAPIToken()
	a := WithAPITokens(&nextAuth{}, &fakeTokenState{level: 9, err: errors.New("disk")}, nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	if _, err := a.Authenticate(r); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if RequireInteractive(Principal{UserID: "u"}) != nil {
		t.Fatal("RequireInteractive rejects a user")
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package frontendcfg

import (
	"slices"
	"strings"
	"testing"
)

// env returns a get function over a map, with the default semantics of the
// host: unset or empty means def.
func env(m map[string]string) func(string, string) string {
	return func(key, def string) string {
		if v := m[key]; v != "" {
			return v
		}
		return def
	}
}

const validNodes = "https://n1.example.com,https://n2.example.com:8443"

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // substring of the error, empty for success
		check   func(t *testing.T, c Config)
	}{
		{
			name: "valid local defaults",
			env:  map[string]string{"FRONTEND_NODES": validNodes},
			check: func(t *testing.T, c Config) {
				if want := []string{"https://n1.example.com", "https://n2.example.com:8443"}; !slices.Equal(c.Nodes, want) {
					t.Errorf("Nodes = %v, want %v", c.Nodes, want)
				}
				if c.Auth.Mode != "local" || c.Auth.Issuer != "" || c.Auth.ClientID != "" || c.Auth.Scopes != nil {
					t.Errorf("Auth = %+v, want plain local", c.Auth)
				}
				if c.Listen != ":8080" {
					t.Errorf("Listen = %q, want :8080", c.Listen)
				}
				if c.HSTS {
					t.Error("HSTS = true without TLS in auto mode")
				}
			},
		},
		{
			name: "valid oidc with default scopes",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
				"OIDC_ISSUER":    "https://idp.example.com/realms/main",
				"OIDC_CLIENT_ID": "pulsequorum-spa",
			},
			check: func(t *testing.T, c Config) {
				if c.Auth.Mode != "oidc" || c.Auth.Issuer != "https://idp.example.com/realms/main" || c.Auth.ClientID != "pulsequorum-spa" {
					t.Errorf("Auth = %+v", c.Auth)
				}
				if want := []string{"openid", "profile", "offline_access"}; !slices.Equal(c.Auth.Scopes, want) {
					t.Errorf("Scopes = %v, want %v", c.Auth.Scopes, want)
				}
			},
		},
		{
			name: "oidc scopes comma or space separated",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
				"OIDC_ISSUER":    "https://idp.example.com",
				"OIDC_CLIENT_ID": "spa",
				"OIDC_SCOPES":    "openid, email  profile",
			},
			check: func(t *testing.T, c Config) {
				if want := []string{"openid", "email", "profile"}; !slices.Equal(c.Auth.Scopes, want) {
					t.Errorf("Scopes = %v, want %v", c.Auth.Scopes, want)
				}
			},
		},
		{
			name:    "missing nodes",
			env:     map[string]string{},
			wantErr: "FRONTEND_NODES: at least one node origin is required",
		},
		{
			name:    "invalid node",
			env:     map[string]string{"FRONTEND_NODES": "https://n1.example.com/app"},
			wantErr: "FRONTEND_NODES",
		},
		{
			name: "oidc missing issuer and client",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
			},
			wantErr: "OIDC_ISSUER: required",
		},
		{
			name: "oidc client missing",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
				"OIDC_ISSUER":    "https://idp.example.com",
			},
			wantErr: "OIDC_CLIENT_ID: required",
		},
		{
			name: "oidc issuer without host",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
				"OIDC_ISSUER":    "ftp://idp.example.com",
				"OIDC_CLIENT_ID": "spa",
			},
			wantErr: "OIDC_ISSUER: must be an absolute",
		},
		{
			name: "oidc scopes without openid",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"AUTH_MODE":      "oidc",
				"OIDC_ISSUER":    "https://idp.example.com",
				"OIDC_CLIENT_ID": "spa",
				"OIDC_SCOPES":    "profile email",
			},
			wantErr: "OIDC_SCOPES: must contain openid",
		},
		{
			name: "local with oidc set",
			env: map[string]string{
				"FRONTEND_NODES": validNodes,
				"OIDC_CLIENT_ID": "spa",
			},
			wantErr: "OIDC_CLIENT_ID: must be empty when AUTH_MODE is local",
		},
		{
			name:    "unknown auth mode",
			env:     map[string]string{"FRONTEND_NODES": validNodes, "AUTH_MODE": "basic"},
			wantErr: "AUTH_MODE: must be local or oidc",
		},
		{
			name:    "tls cert without key",
			env:     map[string]string{"FRONTEND_NODES": validNodes, "FRONTEND_TLS_CERT": "/c.pem"},
			wantErr: "must be set together",
		},
		{
			name: "tls pair enables hsts in auto mode",
			env: map[string]string{
				"FRONTEND_NODES":    validNodes,
				"FRONTEND_TLS_CERT": "/c.pem",
				"FRONTEND_TLS_KEY":  "/k.pem",
			},
			check: func(t *testing.T, c Config) {
				if !c.HSTS || c.TLSCert != "/c.pem" || c.TLSKey != "/k.pem" {
					t.Errorf("HSTS=%v TLSCert=%q TLSKey=%q", c.HSTS, c.TLSCert, c.TLSKey)
				}
			},
		},
		{
			name: "hsts on without tls",
			env:  map[string]string{"FRONTEND_NODES": validNodes, "FRONTEND_HSTS": "on"},
			check: func(t *testing.T, c Config) {
				if !c.HSTS {
					t.Error("HSTS = false, want true")
				}
			},
		},
		{
			name: "hsts off with tls",
			env: map[string]string{
				"FRONTEND_NODES":    validNodes,
				"FRONTEND_TLS_CERT": "/c.pem",
				"FRONTEND_TLS_KEY":  "/k.pem",
				"FRONTEND_HSTS":     "off",
			},
			check: func(t *testing.T, c Config) {
				if c.HSTS {
					t.Error("HSTS = true, want false")
				}
			},
		},
		{
			name:    "unknown hsts mode",
			env:     map[string]string{"FRONTEND_NODES": validNodes, "FRONTEND_HSTS": "yes"},
			wantErr: "FRONTEND_HSTS: must be auto, on or off",
		},
		{
			name:    "collects all errors",
			env:     map[string]string{"AUTH_MODE": "oidc", "FRONTEND_HSTS": "maybe"},
			wantErr: "at least one node origin is required\nOIDC_ISSUER: required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(env(tt.env))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestConfigOutputs(t *testing.T) {
	c, err := Load(env(map[string]string{
		"FRONTEND_NODES": "https://n1.example.com",
		"AUTH_MODE":      "oidc",
		"OIDC_ISSUER":    "https://idp.example.com/realms/main",
		"OIDC_CLIENT_ID": "spa",
	}))
	if err != nil {
		t.Fatal(err)
	}
	csp := c.CSP()
	for _, want := range []string{"https://n1.example.com", "wss://n1.example.com", "https://idp.example.com"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q does not contain %q", csp, want)
		}
	}
	if got := c.FrontendConfig(); got.Auth.ClientID != "spa" || !slices.Equal(got.Nodes, c.Nodes) {
		t.Errorf("FrontendConfig = %+v", got)
	}
	hdrs := c.Headers()
	if len(hdrs) == 0 {
		t.Fatal("Headers returned nothing")
	}
	for _, h := range hdrs {
		if h.Name == "Strict-Transport-Security" {
			t.Error("HSTS header present without HSTS")
		}
	}
	b, err := c.ConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"nodes"`, `"clientId": "spa"`, `"mode": "oidc"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.json %s does not contain %s", b, want)
		}
	}
}

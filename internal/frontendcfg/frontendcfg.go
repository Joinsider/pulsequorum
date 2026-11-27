// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package frontendcfg reads the configuration of the standalone frontend host
// (`pulsequorum frontend`, and the helper of the nginx image) from environment
// variables (ADR 0036). It does not import internal/webui, so that the static
// helper binary stays small.
package frontendcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/joinsider/pulsequorum/internal/webfront"
)

const (
	authLocal     = "local"
	authOIDC      = "oidc"
	defaultListen = ":8080"
)

// defaultOIDCScopes equals the default of the voter without the zitadel
// specific claims (internal/config, ADR 0027). The values are copied, not
// imported, to keep the dependency graph of the helper binary small.
var defaultOIDCScopes = []string{"openid", "profile", "offline_access"}

// Config is the validated configuration of the frontend host.
type Config struct {
	// Nodes are the normalized backend origins the SPA may use (FRONTEND_NODES).
	Nodes []string
	// Auth is written to config.json; the OIDC fields are set in oidc mode only.
	Auth webfront.AuthConfig
	// Listen is the address the host listens on (FRONTEND_LISTEN).
	Listen string
	// TLSCert and TLSKey are set together or not at all (FRONTEND_TLS_CERT/KEY).
	TLSCert, TLSKey string
	// HSTS adds Strict-Transport-Security to every response.
	HSTS bool
}

// Load reads and validates the configuration. get returns the value of key,
// or def if the variable is unset or empty. All problems are collected and
// returned joined, so the operator sees them at once.
func Load(get func(key, def string) string) (Config, error) {
	var (
		c    Config
		errs []error
	)

	nodes, err := webfront.ParseNodes(get("FRONTEND_NODES", ""))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("FRONTEND_NODES: %w", err))
	case len(nodes) == 0:
		errs = append(errs, errors.New("FRONTEND_NODES: at least one node origin is required"))
	}
	c.Nodes = nodes

	auth, authErrs := loadAuth(get)
	c.Auth = auth
	errs = append(errs, authErrs...)

	c.Listen = strings.TrimSpace(get("FRONTEND_LISTEN", defaultListen))
	if c.Listen == "" {
		c.Listen = defaultListen
	}

	c.TLSCert = strings.TrimSpace(get("FRONTEND_TLS_CERT", ""))
	c.TLSKey = strings.TrimSpace(get("FRONTEND_TLS_KEY", ""))
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("FRONTEND_TLS_CERT and FRONTEND_TLS_KEY must be set together"))
	}

	switch hsts := strings.TrimSpace(get("FRONTEND_HSTS", "auto")); hsts {
	case "auto":
		c.HSTS = c.TLSCert != ""
	case "on":
		c.HSTS = true
	case "off":
		c.HSTS = false
	default:
		errs = append(errs, fmt.Errorf("FRONTEND_HSTS: must be auto, on or off, got %q", hsts))
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// loadAuth validates AUTH_MODE and, in oidc mode, the OIDC_* variables.
func loadAuth(get func(key, def string) string) (webfront.AuthConfig, []error) {
	var errs []error
	switch mode := strings.TrimSpace(get("AUTH_MODE", authLocal)); mode {
	case authLocal:
		for _, key := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_SCOPES"} {
			if strings.TrimSpace(get(key, "")) != "" {
				errs = append(errs, fmt.Errorf("%s: must be empty when AUTH_MODE is %s", key, authLocal))
			}
		}
		return webfront.AuthConfig{Mode: authLocal}, errs
	case authOIDC:
		auth := webfront.AuthConfig{Mode: authOIDC}

		issuer := strings.TrimSpace(get("OIDC_ISSUER", ""))
		switch _, ok := webfront.IssuerOrigin(issuer); {
		case issuer == "":
			errs = append(errs, errors.New("OIDC_ISSUER: required in oidc mode"))
		case !ok:
			errs = append(errs, fmt.Errorf("OIDC_ISSUER: must be an absolute http(s) URL with a host, got %q", issuer))
		default:
			auth.Issuer = issuer
		}

		auth.ClientID = strings.TrimSpace(get("OIDC_CLIENT_ID", ""))
		if auth.ClientID == "" {
			errs = append(errs, errors.New("OIDC_CLIENT_ID: required in oidc mode"))
		}

		scopes := strings.FieldsFunc(get("OIDC_SCOPES", ""), func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		})
		if len(scopes) == 0 {
			scopes = slices.Clone(defaultOIDCScopes)
		}
		if !slices.Contains(scopes, "openid") {
			errs = append(errs, fmt.Errorf("OIDC_SCOPES: must contain openid, got %q", scopes))
		}
		auth.Scopes = scopes
		return auth, errs
	default:
		return webfront.AuthConfig{}, []error{fmt.Errorf("AUTH_MODE: must be %s or %s, got %q", authLocal, authOIDC, mode)}
	}
}

// FrontendConfig returns the content of config.json.
func (c Config) FrontendConfig() webfront.Config {
	return webfront.Config{Nodes: c.Nodes, Auth: c.Auth}
}

// ConfigJSON returns config.json as indented JSON with a trailing newline.
func (c Config) ConfigJSON() ([]byte, error) {
	b, err := json.MarshalIndent(c.FrontendConfig(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config.json: %w", err)
	}
	return append(b, '\n'), nil
}

// CSP returns the content security policy of the SPA for this configuration.
func (c Config) CSP() string {
	return webfront.SPACSP(c.Auth.Issuer, c.Nodes)
}

// Headers returns the security headers of every response of the frontend host.
func (c Config) Headers() []webfront.Header {
	return webfront.SPAHeaders(c.CSP(), c.HSTS)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package server

import (
	"net/http"

	"github.com/joinsider/pulsequorum/internal/config"
)

// FrontendConfig is the runtime configuration consumed by the SPA (FA-UI-02).
type FrontendConfig struct {
	// Nodes lists API base URLs; empty means "same origin as the page".
	Nodes []string           `json:"nodes"`
	Auth  FrontendAuthConfig `json:"auth"`
}

// FrontendAuthConfig tells the SPA how to authenticate.
type FrontendAuthConfig struct {
	Mode string `json:"mode"`
	// Issuer, ClientID and Scopes are set in oidc mode only (ADR 0027).
	Issuer   string   `json:"issuer,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	if s.readiness != nil {
		if err := s.readiness(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleFrontendConfig(w http.ResponseWriter, r *http.Request) {
	// The SPA fails over between the voters listed here (ADR 0036). Without
	// the shared source only this voter's PUBLIC_URL is listed.
	nodes := []string{}
	if s.api.PublicNodes != nil {
		nodes = append(nodes, s.api.PublicNodes.Origins(r.Context())...)
	} else if s.cfg.PublicURL != "" {
		nodes = append(nodes, s.cfg.PublicURL)
	}
	auth := FrontendAuthConfig{Mode: string(s.cfg.AuthMode)}
	if s.cfg.AuthMode == config.AuthOIDC {
		auth.Issuer = s.cfg.OIDC.Issuer
		if len(s.cfg.OIDC.ClientIDs) > 0 {
			auth.ClientID = s.cfg.OIDC.ClientIDs[0]
		}
		auth.Scopes = s.cfg.OIDC.Scopes
	}
	writeJSON(w, http.StatusOK, FrontendConfig{
		Nodes: nodes,
		Auth:  auth,
	})
}

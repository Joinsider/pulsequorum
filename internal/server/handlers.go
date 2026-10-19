// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package server

import "net/http"

// FrontendConfig is the runtime configuration consumed by the SPA (FA-UI-02).
type FrontendConfig struct {
	// Nodes lists API base URLs; empty means "same origin as the page".
	Nodes []string           `json:"nodes"`
	Auth  FrontendAuthConfig `json:"auth"`
}

// FrontendAuthConfig tells the SPA how to authenticate.
type FrontendAuthConfig struct {
	Mode string `json:"mode"`
}

func (s *Server) handleLiveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleFrontendConfig(w http.ResponseWriter, _ *http.Request) {
	nodes := []string{}
	if s.cfg.PublicURL != "" {
		nodes = append(nodes, s.cfg.PublicURL)
	}
	writeJSON(w, http.StatusOK, FrontendConfig{
		Nodes: nodes,
		Auth:  FrontendAuthConfig{Mode: string(s.cfg.AuthMode)},
	})
}

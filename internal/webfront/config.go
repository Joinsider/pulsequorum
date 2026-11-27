// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package webfront

import (
	"fmt"
	"slices"
	"strings"

	"github.com/joinsider/pulsequorum/internal/cluster"
)

// Config is the runtime configuration served as /config.json next to the SPA.
type Config struct {
	// Nodes lists the backend origins the SPA may use; empty means "same
	// origin as the page" without failover (ADR 0036).
	Nodes []string   `json:"nodes"`
	Auth  AuthConfig `json:"auth"`
}

// AuthConfig tells the SPA how to authenticate.
type AuthConfig struct {
	Mode string `json:"mode"`
	// Issuer, ClientID and Scopes are set in oidc mode only (ADR 0027).
	Issuer   string   `json:"issuer,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

// ParseNodes reads a comma-separated list of node URLs (FRONTEND_NODES) for
// an external frontend. Every entry must pass cluster.NormalizePublicOrigin;
// unlike the registry, an invalid entry is an error, because the operator
// wrote it. Duplicates are dropped, the order is kept.
func ParseNodes(raw string) ([]string, error) {
	out := []string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		o, err := cluster.NormalizePublicOrigin(entry)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", entry, err)
		}
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out, nil
}

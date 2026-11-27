// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package clusterhost

import (
	"testing"

	"github.com/joinsider/pulsequorum/internal/cluster"
	"github.com/joinsider/pulsequorum/internal/store"
)

// TestSelfNodePublicURL covers ADR 0036: the proposed entry carries the
// normalized origin of PUBLIC_URL, and an invalid value is left out.
func TestSelfNodePublicURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"normalized", "HTTPS://N1.Example.com:443/", "https://n1.example.com"},
		{"invalid http", "http://n1.example.com", ""},
		{"invalid path", "https://n1.example.com/path", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := selfNode(Options{NodeID: "node-1", PublicURL: tt.raw})
			if n.PublicURL != tt.want {
				t.Errorf("PublicURL = %q, want %q", n.PublicURL, tt.want)
			}
		})
	}
}

// TestEntryAtLevel checks that the public URL is proposed only from command
// level 11 on (ADR 0036), so that older clusters keep identical state.
func TestEntryAtLevel(t *testing.T) {
	t.Parallel()
	self := store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleVoter, Version: "1.0.0",
		MaxCommandVersion: cluster.CommandVersion, PublicURL: "https://n1.example.com"}
	tests := []struct {
		name  string
		level uint32
		want  string
	}{
		{"level 0 (unset)", 0, ""},
		{"level 10", 10, ""},
		{"level 11", 11, "https://n1.example.com"},
		{"level 12", 12, "https://n1.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := entryAtLevel(self, tt.level).PublicURL; got != tt.want {
				t.Errorf("PublicURL = %q, want %q", got, tt.want)
			}
		})
	}
	if self.PublicURL != "https://n1.example.com" {
		t.Error("entryAtLevel changed its argument")
	}
}

// TestRegistrationPublicURLChangesEntry checks that a change of the public URL
// changes the request ID and the content comparison, so that the registry is
// updated once the cluster reaches command level 11.
func TestRegistrationPublicURLChangesEntry(t *testing.T) {
	t.Parallel()
	base := store.Node{ID: "node-1", Name: "one", Role: store.NodeRoleVoter, Version: "1.0.0",
		MaxCommandVersion: cluster.CommandVersion}
	withURL := base
	withURL.PublicURL = "https://n1.example.com"

	if registerRequestID(base) == registerRequestID(withURL) {
		t.Error("request ID does not change with the public URL")
	}
	if sameRegistration(base, withURL) {
		t.Error("sameRegistration ignores the public URL")
	}
	if !sameRegistration(withURL, withURL) {
		t.Error("sameRegistration of equal entries is false")
	}
}

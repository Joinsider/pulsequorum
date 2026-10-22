// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package cluster

import (
	"errors"
	"strings"
	"testing"

	"github.com/joinsider/uptimecluster/internal/store"
)

// TestMonitorSecrets covers ADR 0015: the FSM stores secrets only as
// ciphertexts under JSON pointers into the config and rejects anything else
// without writing the monitor.
func TestMonitorSecrets(t *testing.T) {
	h := newAlertFSM(t, t.TempDir())
	h.putTenant(1000, "t1")

	withSecrets := func(sec map[string]string) MonitorDocument {
		doc := monitorDoc("secret monitor")
		doc.Secrets = sec
		return doc
	}
	tests := []struct {
		name    string
		id      string
		doc     MonitorDocument
		want    error
		wantMsg string
	}{
		{"encrypted secret", "m-ok", withSecrets(map[string]string{"/auth/password": encURL}), nil, ""},
		{"plaintext secret", "m-plain", withSecrets(map[string]string{"/auth/password": "hunter2"}), ErrInvalidCommand, "/auth/password is not encrypted"},
		{"pointer without slash", "m-pointer", withSecrets(map[string]string{"auth/password": encURL}), ErrInvalidCommand, `"auth/password" is invalid`},
		{"first plaintext in sorted order", "m-sorted", withSecrets(map[string]string{"/z": "x", "/a": "y"}), ErrInvalidCommand, "/a is not encrypted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := NewPutMonitor("monitor-"+tt.id, "t1", tt.id, tt.doc, 0)
			if err != nil {
				t.Fatal(err)
			}
			res := h.apply(cmd, 1001)
			if !errors.Is(res.Err, tt.want) {
				t.Fatalf("err = %v, want %v", res.Err, tt.want)
			}
			if tt.want != nil {
				if !strings.Contains(res.Err.Error(), tt.wantMsg) {
					t.Errorf("err = %q, want it to contain %q", res.Err, tt.wantMsg)
				}
				if _, err := h.st.GetMonitor(h.ctx, "t1", tt.id); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("rejected monitor stored: %v", err)
				}
				return
			}
			m, err := h.st.GetMonitor(h.ctx, "t1", tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Secrets) != 1 || m.Secrets["/auth/password"] != encURL {
				t.Errorf("stored secrets = %v", m.Secrets)
			}
		})
	}
}

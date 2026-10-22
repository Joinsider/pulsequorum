// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"testing"
	"time"
)

func TestDownDeadline(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		retries  int
		want     time.Duration
		wantErr  bool
	}{
		{"no retries", 20 * time.Second, 0, 50 * time.Second, false},
		{"one retry (AS-01 default)", 20 * time.Second, 1, 70 * time.Second, false},
		{"two retries, 60 s", 60 * time.Second, 2, 210 * time.Second, false},
		{"zero interval", 0, 1, 0, true},
		{"negative retries", 20 * time.Second, -1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := downDeadline(tt.interval, tt.retries)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("downDeadline = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestUpAndRecoveryDeadline(t *testing.T) {
	if got, want := upDeadline(20*time.Second), 90*time.Second; got != want {
		t.Errorf("upDeadline = %s, want %s", got, want)
	}
	if got, want := recoveryDeadline(60*time.Second), 210*time.Second; got != want {
		t.Errorf("recoveryDeadline = %s, want %s", got, want)
	}
}

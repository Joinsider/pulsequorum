// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"io"
	"testing"
	"time"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(t *testing.T, o options)
	}{
		{
			name: "defaults with setup token",
			args: []string{"-setup-token", "tok"},
			check: func(t *testing.T, o options) {
				if o.base != "http://127.0.0.1:8080" || o.user != "admin" || o.listen != ":18950" ||
					o.targetListen != ":18951" || o.callbackHost != "host.containers.internal" {
					t.Errorf("unexpected defaults: %+v", o)
				}
				if o.interval != 20*time.Second || o.retries != 1 {
					t.Errorf("interval/retries = %s/%d", o.interval, o.retries)
				}
			},
		},
		{
			name: "existing admin with password",
			args: []string{"-password", "pw-123456789012", "-interval", "30", "-retries", "0"},
			check: func(t *testing.T, o options) {
				if o.interval != 30*time.Second || o.retries != 0 {
					t.Errorf("interval/retries = %s/%d", o.interval, o.retries)
				}
			},
		},
		{name: "interval below API minimum", args: []string{"-setup-token", "t", "-interval", "19"}, wantErr: true},
		{name: "retries above maximum", args: []string{"-setup-token", "t", "-retries", "11"}, wantErr: true},
		{name: "relative base URL", args: []string{"-setup-token", "t", "-base", "127.0.0.1:8080"}, wantErr: true},
		{name: "no token and no password", args: nil, wantErr: true},
		{name: "positional argument", args: []string{"-setup-token", "t", "extra"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := parseOptions(tt.args, io.Discard)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil && err == nil {
				tt.check(t, o)
			}
		})
	}
}

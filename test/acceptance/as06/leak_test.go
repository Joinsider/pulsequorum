// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package main

import (
	"slices"
	"testing"
)

func TestFindLeaks(t *testing.T) {
	needles := []needle{
		{"monitor name", "as06-http-abcd1234"},
		{"tenant ID", "tenant-acme-1"},
		{"unset", ""},
	}
	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "empty body", body: "", want: nil},
		{name: "clean problem answer", body: `{"title":"Not Found"}`, want: nil},
		{name: "monitor name in detail", body: `{"detail":"as06-http-abcd1234"}`, want: []string{"monitor name"}},
		{name: "tenant ID in list", body: `{"tenantId":"tenant-acme-1"}`, want: []string{"tenant ID"}},
		{name: "both", body: `as06-http-abcd1234 tenant-acme-1`, want: []string{"monitor name", "tenant ID"}},
		{name: "empty value never matches", body: `anything`, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findLeaks(tt.body, needles)
			if !slices.Equal(got, tt.want) {
				t.Errorf("findLeaks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScrub(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		secrets []string
		want    string
	}{
		{name: "no secrets", msg: "HTTP 404", want: "HTTP 404"},
		{name: "token in text", msg: "bad token tok-a-secret here", secrets: []string{"tok-a-secret"},
			want: "bad token <redacted> here"},
		{name: "empty secret ignored", msg: "unchanged", secrets: []string{""}, want: "unchanged"},
		{name: "several secrets", msg: "a=1 b=2", secrets: []string{"1", "2"}, want: "a=<redacted> b=<redacted>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrub(tt.msg, tt.secrets...); got != tt.want {
				t.Errorf("scrub = %q, want %q", got, tt.want)
			}
		})
	}
}

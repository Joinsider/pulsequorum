// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"strings"
	"testing"
)

func TestAgreeLeader(t *testing.T) {
	tests := []struct {
		name    string
		answers map[int]string
		want    string
		wantErr string
	}{
		{name: "all agree", answers: map[int]string{1: "n1", 2: "n1", 3: "n1"}, want: "n1"},
		{name: "one reachable voter", answers: map[int]string{2: "n2"}, want: "n2"},
		{name: "disagreement", answers: map[int]string{1: "n1", 2: "n2"}, wantErr: "disagree"},
		{name: "a voter knows no leader", answers: map[int]string{1: "n1", 3: ""}, wantErr: "knows no leader"},
		{name: "nobody answered", answers: map[int]string{}, wantErr: "no voter answered"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := agreeLeader(tt.answers)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("leader = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetupTokenFromLogs(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want string
		ok   bool
	}{
		{
			name: "JSON log",
			logs: `{"time":"x","level":"WARN","msg":"no user exists yet","setup_token":"abc-DEF_123"}` + "\n",
			want: "abc-DEF_123", ok: true,
		},
		{
			name: "text log",
			logs: "time=x level=WARN msg=\"no user\" setup_token=tok42 expires=1h\n",
			want: "tok42", ok: true,
		},
		{name: "no token yet", logs: `{"msg":"listening"}` + "\n", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := setupTokenFromLogs(tt.logs)
			if ok != tt.ok || got != tt.want {
				t.Errorf("setupTokenFromLogs = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRandomPasswordIsLongAndDistinct(t *testing.T) {
	a, err := randomPassword()
	if err != nil {
		t.Fatal(err)
	}
	b, err := randomPassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 12 || a == b {
		t.Errorf("passwords %q and %q: want length >= 12 and distinct", a, b)
	}
}

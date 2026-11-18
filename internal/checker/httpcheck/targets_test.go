// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package httpcheck

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/joinsider/uptimecluster/internal/checker"
)

func TestTargets(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want []checker.Target
	}{
		{"domain with port", `{"url":"https://user.example.com:8443/x"}`, []checker.Target{{Field: "/url", Host: "user.example.com"}}},
		{"ipv4", `{"url":"https://8.8.8.8/"}`, []checker.Target{{Field: "/url", Host: "8.8.8.8"}}},
		{"ipv6 without zone", `{"url":"http://[2001:db8::1]:8080/"}`, []checker.Target{{Field: "/url", Host: "2001:db8::1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&Checker{}).Targets(json.RawMessage(tt.cfg))
			if err != nil {
				t.Fatalf("Targets: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Targets = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTargetsInvalidURL(t *testing.T) {
	if _, err := (&Checker{}).Targets(json.RawMessage(`{"url":"http://[::1"}`)); err == nil {
		t.Error("invalid URL accepted")
	}
}

func TestTargetNotation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	for _, cfg := range []string{`{"url":"https://example.com/"}`, `{"url":"https://8.8.8.8/"}`} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{`{"url":"http://2130706433/"}`, `{"url":"http://0x7f.1/"}`} {
		expectField(t, reg.Validate(Type, json.RawMessage(cfg)), "/url")
	}
}

func TestTargetPolicy(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	cfg := json.RawMessage(`{"url":"http://169.254.169.254/"}`)
	if err := reg.ValidateTargets(Type, cfg, nil); err != nil {
		t.Errorf("without policy: %v", err)
	}
	expectField(t, reg.ValidateTargets(Type, cfg, checker.BlockPrivate{}), "/url")
}

// expectField fails unless err is a *checker.ValidationError naming field.
func expectField(t *testing.T, err error, field string) {
	t.Helper()
	var verr *checker.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error = %v, want a validation error on %s", err, field)
	}
	for _, f := range verr.Fields {
		if f.Field == field {
			return
		}
	}
	t.Errorf("fields = %+v, want %s", verr.Fields, field)
}

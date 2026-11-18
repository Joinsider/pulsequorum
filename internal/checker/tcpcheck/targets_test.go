// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package tcpcheck

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
		{"domain", `{"host":"db.example.com","port":5432}`, []checker.Target{{Field: "/host", Host: "db.example.com"}}},
		{"ipv4", `{"host":"8.8.8.8","port":443}`, []checker.Target{{Field: "/host", Host: "8.8.8.8"}}},
		{"ipv6", `{"host":"2001:db8::1","port":443}`, []checker.Target{{Field: "/host", Host: "2001:db8::1"}}},
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

func TestTargetNotation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	for _, cfg := range []string{`{"host":"db.example.com","port":5432}`, `{"host":"8.8.8.8","port":443}`} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{`{"host":"0x7f.1","port":5432}`, `{"host":"0177.0.0.1","port":5432}`} {
		expectField(t, reg.Validate(Type, json.RawMessage(cfg)), "/host")
	}
}

func TestTargetPolicy(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	cfg := json.RawMessage(`{"host":"169.254.169.254","port":80}`)
	if err := reg.ValidateTargets(Type, cfg, nil); err != nil {
		t.Errorf("without policy: %v", err)
	}
	expectField(t, reg.ValidateTargets(Type, cfg, checker.BlockPrivate{}), "/host")
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

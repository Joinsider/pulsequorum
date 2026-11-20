// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package dnscheck

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker"
)

func TestTargets(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want []checker.Target
	}{
		{"system resolver is no target", `{"hostname":"example.com"}`, nil},
		{"queried name is no target", `{"hostname":"0x7f.1","resolver":"8.8.8.8:53"}`, []checker.Target{{Field: "/resolver", Host: "8.8.8.8"}}},
		{"resolver without port", `{"hostname":"example.com","resolver":"1.1.1.1"}`, []checker.Target{{Field: "/resolver", Host: "1.1.1.1"}}},
		{"bracketed ipv6 with port", `{"hostname":"example.com","resolver":"[2606:4700::1111]:53"}`, []checker.Target{{Field: "/resolver", Host: "2606:4700::1111"}}},
		{"bare ipv6", `{"hostname":"example.com","resolver":"2606:4700::1111"}`, []checker.Target{{Field: "/resolver", Host: "2606:4700::1111"}}},
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
	for _, cfg := range []string{
		`{"hostname":"example.com"}`,
		`{"hostname":"0x7f.1"}`, // the queried name is only looked up
		`{"hostname":"example.com","resolver":"8.8.8.8"}`,
		`{"hostname":"example.com","resolver":"8.8.8.8:53"}`,
	} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{
		`{"hostname":"example.com","resolver":"0177.0.0.1"}`,
		`{"hostname":"example.com","resolver":"0x7f.1:53"}`,
	} {
		expectField(t, reg.Validate(Type, json.RawMessage(cfg)), "/resolver")
	}
	// The notation rule itself, without the resolver syntax check of Validate.
	expectField(t, reg.ValidateTargets(Type, json.RawMessage(`{"hostname":"example.com","resolver":"0177.0.0.1"}`), nil), "/resolver")
}

func TestTargetPolicy(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	cfg := json.RawMessage(`{"hostname":"example.com","resolver":"169.254.169.254"}`)
	if err := reg.ValidateTargets(Type, cfg, nil); err != nil {
		t.Errorf("without policy: %v", err)
	}
	expectField(t, reg.ValidateTargets(Type, cfg, checker.BlockPrivate{}), "/resolver")
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

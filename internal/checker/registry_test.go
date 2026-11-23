// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package checker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type stubChecker struct {
	typ      string
	schema   string
	validate func(json.RawMessage) error
	targets  []Target
	caps     []string
}

func (s *stubChecker) Type() string                  { return s.typ }
func (s *stubChecker) Name() string                  { return s.typ }
func (s *stubChecker) ConfigSchema() json.RawMessage { return json.RawMessage(s.schema) }
func (s *stubChecker) Validate(cfg json.RawMessage) error {
	if s.validate != nil {
		return s.validate(cfg)
	}
	return nil
}
func (s *stubChecker) Check(context.Context, *Env, json.RawMessage) Result {
	return Result{Status: StatusUp}
}
func (s *stubChecker) Targets(json.RawMessage) ([]Target, error) {
	return s.targets, nil
}

type stubCapChecker struct {
	stubChecker
}

func (s *stubCapChecker) RequiredClusterCapabilities(json.RawMessage) []string {
	return s.caps
}

const testObjectSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["host"],
  "properties": {
    "host": {"type": "string", "minLength": 1},
    "nested": {
      "type": "object",
      "required": ["port"],
      "properties": {
        "port": {"type": "integer", "minimum": 1}
      }
    }
  }
}`

func TestValidationErrorString(t *testing.T) {
	if got := (&ValidationError{}).Error(); got != "invalid configuration" {
		t.Fatalf("empty ValidationError = %q", got)
	}
	one := &ValidationError{Fields: []FieldError{{Field: "/url", Message: "missing"}}}
	if got := one.Error(); got != "invalid configuration: /url: missing" {
		t.Fatalf("single field ValidationError = %q", got)
	}
	two := &ValidationError{Fields: []FieldError{
		{Field: "/url", Message: "missing"},
		{Field: "/port", Message: "too low"},
	}}
	if got := two.Error(); !strings.Contains(got, "(and 1 more)") {
		t.Fatalf("multi-field ValidationError = %q", got)
	}
}

func TestRegistryRegisterLookupCheckersAndCapabilities(t *testing.T) {
	r := NewRegistry()
	cB := &stubCapChecker{
		stubChecker: stubChecker{typ: "b", schema: testObjectSchema, caps: []string{"cap-b"}},
	}
	cA := &stubChecker{typ: "a", schema: testObjectSchema}
	r.Register(cB)
	r.Register(cA)

	list := r.Checkers()
	if len(list) != 2 || list[0].Type() != "a" || list[1].Type() != "b" {
		t.Fatalf("Checkers() = %v", list)
	}
	if _, err := r.Lookup("a"); err != nil {
		t.Fatalf("Lookup(a): %v", err)
	}
	if _, err := r.Lookup("unknown"); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Lookup(unknown) = %v, want ErrUnknownType", err)
	}
	if got := compareStrings("x", "x"); got != 0 {
		t.Fatalf("compareStrings equal = %d", got)
	}

	if caps := r.ClusterCapabilities("unknown", nil); caps != nil {
		t.Fatalf("ClusterCapabilities(unknown) = %v, want nil", caps)
	}
	if caps := r.ClusterCapabilities("a", nil); caps != nil {
		t.Fatalf("ClusterCapabilities(a) = %v, want nil", caps)
	}
	if caps := r.ClusterCapabilities("b", nil); len(caps) != 1 || caps[0] != "cap-b" {
		t.Fatalf("ClusterCapabilities(b) = %v, want [cap-b]", caps)
	}
}

func TestRegistryRegisterPanics(t *testing.T) {
	t.Run("invalid schema", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on invalid schema")
			}
		}()
		r := NewRegistry()
		r.Register(&stubChecker{typ: "bad", schema: `{not-json`})
	})

	t.Run("duplicate type", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic on duplicate registration")
			}
		}()
		r := NewRegistry()
		r.Register(&stubChecker{typ: "dup", schema: testObjectSchema})
		r.Register(&stubChecker{typ: "dup", schema: testObjectSchema})
	})
}

func TestRegistryValidate(t *testing.T) {
	r := NewRegistry()
	var customErr error
	r.Register(&stubChecker{
		typ:     "custom",
		schema:  testObjectSchema,
		targets: []Target{{Field: "/host", Host: "example.com"}},
		validate: func(json.RawMessage) error {
			return customErr
		},
	})

	if err := r.Validate("missing", json.RawMessage(`{}`)); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Validate unknown = %v", err)
	}
	if err := r.Validate("custom", json.RawMessage(`{invalid`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	// Empty config triggers missing required "/host".
	if err := r.Validate("custom", nil); err == nil {
		t.Fatal("expected required field error")
	}
	// Additional property and nested error.
	if err := r.Validate("custom", json.RawMessage(`{"host":"example.com","extra":1,"nested":{}}`)); err == nil {
		t.Fatal("expected additionalProperties / nested required error")
	}
	// Semantic ValidationError passthrough.
	customErr = &ValidationError{Fields: []FieldError{{Field: "/host", Message: "bad"}}}
	if err := r.Validate("custom", json.RawMessage(`{"host":"example.com"}`)); !errors.Is(err, customErr) {
		t.Fatalf("expected passthrough ValidationError, got %v", err)
	}
	// Semantic plain error wrapped into ValidationError.
	customErr = errors.New("plain failure")
	if err := r.Validate("custom", json.RawMessage(`{"host":"example.com"}`)); err == nil {
		t.Fatal("expected wrapped plain error")
	}
	// Valid configuration.
	customErr = nil
	if err := r.Validate("custom", json.RawMessage(`{"host":"example.com","nested":{"port":443}}`)); err != nil {
		t.Fatalf("Validate valid config: %v", err)
	}
}

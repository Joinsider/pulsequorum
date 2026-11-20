// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSchemaWithOrder(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // expected document, compared after canonical re-encoding
	}{
		{
			name: "flat object keeps source order, not alphabetical",
			in:   `{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"},"auth":{"type":"object"}}}`,
			want: `{"type":"object","x-order":["url","method","auth"],"properties":{"url":{"type":"string"},"method":{"type":"string"},"auth":{"type":"object"}}}`,
		},
		{
			name: "nested object gets its own x-order",
			in:   `{"properties":{"outer":{"properties":{"z":{},"a":{}}}}}`,
			want: `{"x-order":["outer"],"properties":{"outer":{"x-order":["z","a"],"properties":{"z":{},"a":{}}}}}`,
		},
		{
			name: "items object",
			in:   `{"type":"array","items":{"type":"object","properties":{"b":{},"a":{}}}}`,
			want: `{"type":"array","items":{"type":"object","x-order":["b","a"],"properties":{"b":{},"a":{}}}}`,
		},
		{
			name: "additionalProperties object",
			in:   `{"type":"object","additionalProperties":{"type":"object","properties":{"y":{},"x":{}}}}`,
			want: `{"type":"object","additionalProperties":{"type":"object","x-order":["y","x"],"properties":{"y":{},"x":{}}}}`,
		},
		{
			name: "additionalProperties boolean is left alone",
			in:   `{"type":"object","additionalProperties":false,"properties":{"k":{}}}`,
			want: `{"type":"object","additionalProperties":false,"x-order":["k"],"properties":{"k":{}}}`,
		},
		{
			name: "oneOf anyOf allOf entries",
			in: `{"oneOf":[{"properties":{"b":{},"a":{}}}],` +
				`"anyOf":[{"properties":{"d":{},"c":{}}}],` +
				`"allOf":[{"properties":{"f":{},"e":{}}}]}`,
			want: `{"oneOf":[{"x-order":["b","a"],"properties":{"b":{},"a":{}}}],` +
				`"anyOf":[{"x-order":["d","c"],"properties":{"d":{},"c":{}}}],` +
				`"allOf":[{"x-order":["f","e"],"properties":{"f":{},"e":{}}}]}`,
		},
		{
			name: "$defs and definitions entries",
			in:   `{"$defs":{"d":{"properties":{"q":{},"p":{}}}},"definitions":{"e":{"properties":{"s":{},"r":{}}}}}`,
			want: `{"$defs":{"d":{"x-order":["q","p"],"properties":{"q":{},"p":{}}}},"definitions":{"e":{"x-order":["s","r"],"properties":{"s":{},"r":{}}}}}`,
		},
		{
			name: "no properties means no x-order",
			in:   `{"type":"object","items":{"type":"string"}}`,
			want: `{"type":"object","items":{"type":"string"}}`,
		},
		{
			name: "empty properties gets an empty x-order",
			in:   `{"properties":{}}`,
			want: `{"x-order":[],"properties":{}}`,
		},
		{
			name: "existing x-order is kept",
			in:   `{"x-order":["a","b"],"properties":{"b":{},"a":{}}}`,
			want: `{"x-order":["a","b"],"properties":{"b":{},"a":{}}}`,
		},
		{
			name: "property named like a keyword is not treated as a schema keyword",
			in:   `{"properties":{"items":{"properties":{"m":{}}},"properties":{"n":{}}}}`,
			want: `{"x-order":["items","properties"],"properties":{"items":{"x-order":["m"],"properties":{"m":{}}},"properties":{"n":{}}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := schemaWithOrder(json.RawMessage(tt.in))
			if err != nil {
				t.Fatalf("schemaWithOrder: %v", err)
			}
			want := canonicalJSON(t, json.RawMessage(tt.want))
			if gotCanon := canonicalJSON(t, got); !reflect.DeepEqual(gotCanon, want) {
				t.Errorf("schema =\n%v\nwant\n%v", gotCanon, want)
			}
		})
	}
}

func TestSchemaWithOrderErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "invalid JSON", in: `{"properties":`},
		{name: "empty input", in: ``},
		{name: "trailing data", in: `{} {}`},
		{name: "top level is not an object", in: `[1,2]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := schemaWithOrder(json.RawMessage(tt.in)); err == nil {
				t.Errorf("schemaWithOrder(%q) returned no error", tt.in)
			}
		})
	}
}

// canonicalJSON round-trips v through encoding/json so that both sides of a
// comparison use the same generic types and sorted map keys.
func canonicalJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

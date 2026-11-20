// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// orderKey is the annotation that carries the property names of a schema in
// display order. JSON objects decode into Go maps, which lose the source order.
const orderKey = "x-order"

// object is a JSON object that remembers the order of its keys.
type object struct {
	keys   []string
	values map[string]any
}

// schemaWithOrder decodes a JSON Schema document into a generic map like
// json.Unmarshal does, and adds an "x-order" array with the property names
// in source order to every schema object that has a "properties" object.
// An existing "x-order" is kept. The walk covers properties, items,
// additionalProperties, oneOf, anyOf, allOf, $defs and definitions.
func schemaWithOrder(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	root, err := parseOrdered(dec)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse schema: trailing data after document")
	}
	obj, ok := root.(*object)
	if !ok {
		return nil, fmt.Errorf("parse schema: want object, got %T", root)
	}
	annotateSchema(obj)
	return obj.plain(), nil
}

// parseOrdered reads one JSON value from dec. Objects become *object, arrays
// become []any, and scalars keep the types of json.Decoder.Token.
func parseOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := &object{values: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("object key is %T, want string", kt)
			}
			v, err := parseOrdered(dec)
			if err != nil {
				return nil, err
			}
			if _, seen := obj.values[key]; !seen {
				obj.keys = append(obj.keys, key)
			}
			obj.values[key] = v
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := parseOrdered(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected delimiter %q", delim)
}

// annotateSchema adds x-order to obj when it has a properties object and
// recurses into the nested schemas.
func annotateSchema(obj *object) {
	if props, ok := obj.values["properties"].(*object); ok {
		for _, name := range props.keys {
			annotateAny(props.values[name])
		}
		if _, exists := obj.values[orderKey]; !exists {
			names := make([]any, 0, len(props.keys))
			for _, name := range props.keys {
				names = append(names, name)
			}
			obj.values[orderKey] = names
		}
	}
	for _, kw := range []string{"items", "additionalProperties"} {
		annotateAny(obj.values[kw])
	}
	for _, kw := range []string{"oneOf", "anyOf", "allOf"} {
		annotateAny(obj.values[kw])
	}
	for _, kw := range []string{"$defs", "definitions"} {
		if defs, ok := obj.values[kw].(*object); ok {
			for _, name := range defs.keys {
				annotateAny(defs.values[name])
			}
		}
	}
}

// annotateAny applies annotateSchema to a schema object, or to each element
// of an array of schemas. Other values are not schemas and are left alone.
func annotateAny(v any) {
	switch s := v.(type) {
	case *object:
		annotateSchema(s)
	case []any:
		for _, e := range s {
			annotateAny(e)
		}
	}
}

// plain converts the parsed tree into the generic form of json.Unmarshal.
func (o *object) plain() map[string]any {
	out := make(map[string]any, len(o.values))
	for k, v := range o.values {
		out[k] = plainValue(v)
	}
	return out
}

func plainValue(v any) any {
	switch t := v.(type) {
	case *object:
		return t.plain()
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainValue(e)
		}
		return out
	}
	return v
}

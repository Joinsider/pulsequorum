// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package httpcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// segment is one step of a path: an object key or an array index.
type segment struct {
	key   string
	index int
	isIdx bool
}

// parsePath parses a small JSONPath subset: $.a.b[0].c, a.b[0] or $["key"].
func parsePath(p string) ([]segment, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "$")
	var segs []segment
	for len(p) > 0 {
		switch p[0] {
		case '.':
			p = p[1:]
			end := strings.IndexAny(p, ".[")
			if end < 0 {
				end = len(p)
			}
			if end == 0 {
				return nil, errors.New("empty key in path")
			}
			segs = append(segs, segment{key: p[:end]})
			p = p[end:]
		case '[':
			end := strings.IndexByte(p, ']')
			if end < 0 {
				return nil, errors.New("missing ] in path")
			}
			inner := p[1:end]
			p = p[end+1:]
			if strings.HasPrefix(inner, `"`) || strings.HasPrefix(inner, `'`) {
				if len(inner) < 2 || inner[len(inner)-1] != inner[0] {
					return nil, errors.New("unterminated quoted key in path")
				}
				segs = append(segs, segment{key: inner[1 : len(inner)-1]})
				continue
			}
			idx, err := strconv.Atoi(inner)
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("invalid array index %q", inner)
			}
			segs = append(segs, segment{index: idx, isIdx: true})
		default:
			if len(segs) > 0 {
				return nil, fmt.Errorf("unexpected %q in path", p[0])
			}
			p = "." + p // allow "a.b" without leading "$."
		}
	}
	if len(segs) == 0 {
		return nil, errors.New("empty path")
	}
	return segs, nil
}

// lookup evaluates the path on a JSON document and renders the value as a
// string: strings as-is, numbers in their JSON form, true/false/null, and
// objects or arrays as compact JSON.
func lookup(body []byte, path string) (string, error) {
	segs, err := parsePath(path)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("response is not JSON: %w", err)
	}
	for _, s := range segs {
		switch cur := v.(type) {
		case map[string]any:
			if s.isIdx {
				return "", fmt.Errorf("index [%d] used on an object", s.index)
			}
			next, ok := cur[s.key]
			if !ok {
				return "", fmt.Errorf("key %q not found", s.key)
			}
			v = next
		case []any:
			if !s.isIdx {
				return "", fmt.Errorf("key %q used on an array", s.key)
			}
			if s.index >= len(cur) {
				return "", fmt.Errorf("index [%d] out of range (length %d)", s.index, len(cur))
			}
			v = cur[s.index]
		default:
			return "", errors.New("path continues below a scalar value")
		}
	}
	switch val := v.(type) {
	case string:
		return val, nil
	case json.Number:
		return val.String(), nil
	case bool:
		return strconv.FormatBool(val), nil
	case nil:
		return "null", nil
	default:
		b, err := json.Marshal(val)
		return string(b), err
	}
}

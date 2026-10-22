// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package configsecret protects secrets inside monitor configurations
// (ADR 0015). A field is secret if the type's JSON Schema marks it with
// `writeOnly: true`. Secret values are taken out of the configuration,
// encrypted one by one (ADR 0011) and stored by JSON pointer; the stored
// configuration holds Mask in their place. Plaintext exists only in API
// input and, while a check runs, in memory.
package configsecret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Mask stands for a stored secret in configurations and API responses. In
// an update it means "keep the stored value" (same as notification channels).
const Mask = "***"

// wildcard in a path pattern matches any map key or array index.
const wildcard = "*"

// Keyring encrypts and decrypts single values (implemented by secrets.Keyring).
type Keyring interface {
	Encrypt(plaintext []byte, aad string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aad string) ([]byte, error)
}

// Errors returned for invalid input; callers report them as 422.
var (
	// ErrMaskWithoutSecret is returned when the input contains Mask at a place
	// where no secret is stored, e.g. after a type change or for a new header.
	ErrMaskWithoutSecret = errors.New("masked value can only keep an existing secret")
	// ErrUnsupportedSchema is returned for schemas the walker cannot follow
	// safely ($ref); treating them as secret-free could leak secrets.
	ErrUnsupportedSchema = errors.New("unsupported JSON Schema construct for secret detection")
)

// MaskError reports Mask at a pointer without a stored secret.
type MaskError struct{ Pointer string }

func (e *MaskError) Error() string { return ErrMaskWithoutSecret.Error() + ": " + e.Pointer }

// Unwrap makes errors.Is(err, ErrMaskWithoutSecret) work.
func (e *MaskError) Unwrap() error { return ErrMaskWithoutSecret }

// Secrets maps JSON pointers into the configuration to ciphertexts.
type Secrets map[string]string

// Paths lists where a schema allows secret values, as patterns of path
// segments; "*" matches any map key or array index.
type Paths [][]string

// PathsOf collects the writeOnly locations of a JSON Schema.
func PathsOf(schema json.RawMessage) (Paths, error) {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	var out Paths
	if err := walkSchema(root, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkSchema(node any, path []string, out *Paths) error {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := obj["$ref"]; ok {
		return fmt.Errorf("%w at %s", ErrUnsupportedSchema, "/"+strings.Join(path, "/"))
	}
	if wo, _ := obj["writeOnly"].(bool); wo {
		// The whole value is secret; nothing below needs to be visited.
		*out = append(*out, slices.Clone(path))
		return nil
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if err := walkSchema(props[name], append(slices.Clone(path), name), out); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"additionalProperties", "items"} {
		if err := walkSchema(obj[key], append(slices.Clone(path), wildcard), out); err != nil {
			return err
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		subs, _ := obj[key].([]any)
		for _, sub := range subs {
			if err := walkSchema(sub, path, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// AAD binds a ciphertext to its monitor and field, so it cannot be copied to
// another monitor or position (ADR 0011).
func AAD(monitorID, pointer string) string {
	return "monitors/" + monitorID + "/config" + pointer
}

// Split takes the secrets out of an input configuration. Values at secret
// positions are encrypted for monitorID and replaced by Mask. Mask keeps the
// secret from previous at the same pointer; an absent fixed-name field keeps
// it too if its parent object is present (PUT without re-entering the
// password). An empty string removes the field. previous must only be passed
// when the monitor keeps its type.
func Split(kr Keyring, paths Paths, monitorID string, config json.RawMessage, previous Secrets) (json.RawMessage, Secrets, error) {
	doc, err := decode(config)
	if err != nil {
		return nil, nil, err
	}
	out := Secrets{}
	cleared := map[string]bool{}
	for _, loc := range locate(doc, paths) {
		ptr := pointer(loc.path)
		s, isString := loc.value.(string)
		switch {
		case isString && s == Mask:
			c, ok := previous[ptr]
			if !ok {
				return nil, nil, &MaskError{Pointer: ptr}
			}
			out[ptr] = c
		case isString && s == "":
			loc.remove()
			cleared[ptr] = true
		default:
			plain, err := json.Marshal(loc.value)
			if err != nil {
				return nil, nil, err
			}
			c, err := kr.Encrypt(plain, AAD(monitorID, ptr))
			if err != nil {
				return nil, nil, fmt.Errorf("encrypt %s: %w", ptr, err)
			}
			out[ptr] = c
			loc.set(Mask)
		}
	}
	// Keep secrets whose field was left out of an otherwise present object.
	for _, ptr := range sortedKeys(previous) {
		if _, done := out[ptr]; done || cleared[ptr] {
			continue
		}
		segs, err := parsePointer(ptr)
		if err != nil || !fixedName(paths, segs) {
			continue
		}
		if parent, ok := lookup(doc, segs[:len(segs)-1]).(map[string]any); ok {
			if _, present := parent[segs[len(segs)-1]]; !present {
				parent[segs[len(segs)-1]] = Mask
				out[ptr] = previous[ptr]
			}
		}
	}
	masked, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		out = nil
	}
	return masked, out, nil
}

// MaskConfig replaces every value at a secret position with Mask. Stored
// configurations already hold Mask; this guards API responses against
// configurations written before ADR 0015 that are not yet migrated.
func MaskConfig(paths Paths, config json.RawMessage) (json.RawMessage, error) {
	doc, err := decode(config)
	if err != nil {
		return nil, err
	}
	for _, loc := range locate(doc, paths) {
		loc.set(Mask)
	}
	return json.Marshal(doc)
}

// HasPlaintext reports whether a stored configuration still contains a
// secret value that is not masked (written before ADR 0015).
func HasPlaintext(paths Paths, config json.RawMessage) (bool, error) {
	doc, err := decode(config)
	if err != nil {
		return false, err
	}
	for _, loc := range locate(doc, paths) {
		if s, ok := loc.value.(string); !ok || s != Mask {
			return true, nil
		}
	}
	return false, nil
}

// Resolve returns the configuration with the secrets decrypted and put back,
// for running a check. The result must stay in memory and never be logged.
func Resolve(ctx context.Context, kr Keyring, monitorID string, config json.RawMessage, secrets Secrets) (json.RawMessage, error) {
	if len(secrets) == 0 {
		return config, nil
	}
	doc, err := decode(config)
	if err != nil {
		return nil, err
	}
	for _, ptr := range sortedKeys(secrets) {
		segs, err := parsePointer(ptr)
		if err != nil || len(segs) == 0 {
			return nil, fmt.Errorf("secret pointer %q: invalid", ptr)
		}
		plain, err := kr.Decrypt(ctx, secrets[ptr], AAD(monitorID, ptr))
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", ptr, err)
		}
		value, err := decode(plain)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", ptr, err)
		}
		if !setAt(doc, segs, value) {
			return nil, fmt.Errorf("secret %s has no place in the configuration", ptr)
		}
	}
	return json.Marshal(doc)
}

// Reencrypt binds the secrets of one monitor to another (clone).
func Reencrypt(ctx context.Context, kr Keyring, fromID, toID string, secrets Secrets) (Secrets, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	out := make(Secrets, len(secrets))
	for _, ptr := range sortedKeys(secrets) {
		plain, err := kr.Decrypt(ctx, secrets[ptr], AAD(fromID, ptr))
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", ptr, err)
		}
		c, err := kr.Encrypt(plain, AAD(toID, ptr))
		clear(plain)
		if err != nil {
			return nil, fmt.Errorf("encrypt %s: %w", ptr, err)
		}
		out[ptr] = c
	}
	return out, nil
}

// --- JSON documents ---------------------------------------------------------

// decode keeps numbers exact (json.Number), so masking does not alter them.
func decode(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return v, nil
}

// location is a value found at a secret position with its container.
type location struct {
	path   []string
	value  any
	set    func(any)
	remove func()
}

// locate finds all values in doc that match a secret path pattern, in a
// stable order.
func locate(doc any, paths Paths) []location {
	var out []location
	seen := map[string]bool{}
	for _, p := range paths {
		if len(p) == 0 {
			continue // a schema whose root is writeOnly has no addressable field
		}
		find(doc, p, nil, func(l location) {
			if ptr := pointer(l.path); !seen[ptr] {
				seen[ptr] = true
				out = append(out, l)
			}
		})
	}
	slices.SortFunc(out, func(a, b location) int { return strings.Compare(pointer(a.path), pointer(b.path)) })
	return out
}

func find(node any, pattern, at []string, emit func(location)) {
	seg, rest := pattern[0], pattern[1:]
	visit := func(key string, child any, set func(any), remove func()) {
		path := append(slices.Clone(at), key)
		if len(rest) == 0 {
			emit(location{path: path, value: child, set: set, remove: remove})
			return
		}
		find(child, rest, path, emit)
	}
	switch n := node.(type) {
	case map[string]any:
		keys := sortedKeys(n)
		for _, k := range keys {
			if seg != wildcard && seg != k {
				continue
			}
			visit(k, n[k], func(v any) { n[k] = v }, func() { delete(n, k) })
		}
	case []any:
		if seg != wildcard {
			return
		}
		for i := range n {
			visit(strconv.Itoa(i), n[i], func(v any) { n[i] = v }, func() { n[i] = "" })
		}
	}
}

// fixedName reports whether the pointer matches a pattern whose last
// segment is a property name (not a map key or index).
func fixedName(paths Paths, segs []string) bool {
	for _, p := range paths {
		if len(p) != len(segs) || p[len(p)-1] == wildcard {
			continue
		}
		match := true
		for i := range p {
			if p[i] != wildcard && p[i] != segs[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func lookup(node any, segs []string) any {
	for _, s := range segs {
		switch n := node.(type) {
		case map[string]any:
			node = n[s]
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(n) {
				return nil
			}
			node = n[i]
		default:
			return nil
		}
	}
	return node
}

func setAt(doc any, segs []string, value any) bool {
	parent := lookup(doc, segs[:len(segs)-1])
	last := segs[len(segs)-1]
	switch p := parent.(type) {
	case map[string]any:
		p[last] = value
		return true
	case []any:
		i, err := strconv.Atoi(last)
		if err != nil || i < 0 || i >= len(p) {
			return false
		}
		p[i] = value
		return true
	}
	return false
}

// pointer encodes path segments as an RFC 6901 JSON pointer.
func pointer(segs []string) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(s))
	}
	return b.String()
}

func parsePointer(p string) ([]string, error) {
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("pointer %q must start with /", p)
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		segs[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
	}
	return segs, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

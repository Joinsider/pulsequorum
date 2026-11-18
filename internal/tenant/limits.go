// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

// Package tenant holds the rules for tenants, plans and their limits (ADR 0029).
// It has no I/O, so the Raft FSM and the API use the same rules.
package tenant

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reserved IDs (ADR 0025, 0027, 0029).
const (
	// SystemTenantID mirrors store.SystemTenantID; it is repeated here so the
	// package stays free of store dependencies.
	SystemTenantID = "_system"
	// DefaultTenantID is the tenant created at bootstrap in every auth mode.
	DefaultTenantID = "default"
	// DefaultPlanID is the built-in plan without limits; it cannot be deleted.
	DefaultPlanID = "default"
	// SystemPlanID belongs to the system tenant only; it cannot be changed,
	// deleted or assigned to another tenant.
	SystemPlanID = "system"
)

// Tenant sources (tenants.source).
const (
	SourceAdmin     = "admin"
	SourceAuto      = "auto"
	SourceBootstrap = "bootstrap"
)

// Limit names as they appear in limits_json.
const (
	LimitMonitors      = "maxMonitors"
	LimitMinInterval   = "minIntervalSeconds"
	LimitChannels      = "maxChannels"
	LimitStatusPages   = "maxStatusPages"
	LimitPrivateProbes = "maxPrivateProbes"
)

// limitNames lists the known limits in a fixed order.
var limitNames = []string{LimitMonitors, LimitMinInterval, LimitChannels, LimitStatusPages, LimitPrivateProbes}

// maxLimitValue bounds every limit, so that the values fit any integer column
// and a typo like 1e12 is rejected instead of meaning "unlimited".
const maxLimitValue = 1_000_000

// ErrInvalidLimits is wrapped by every validation error of ParseLimits and ParseOverrides.
var ErrInvalidLimits = errors.New("invalid limits")

// Limits are the limits of a plan. A nil field is unlimited.
type Limits struct {
	MaxMonitors        *int `json:"maxMonitors,omitempty"`
	MinIntervalSeconds *int `json:"minIntervalSeconds,omitempty"`
	MaxChannels        *int `json:"maxChannels,omitempty"`
	MaxStatusPages     *int `json:"maxStatusPages,omitempty"`
	MaxPrivateProbes   *int `json:"maxPrivateProbes,omitempty"`
}

// field returns a pointer to the field of the limit name, or nil.
func (l *Limits) field(name string) **int {
	switch name {
	case LimitMonitors:
		return &l.MaxMonitors
	case LimitMinInterval:
		return &l.MinIntervalSeconds
	case LimitChannels:
		return &l.MaxChannels
	case LimitStatusPages:
		return &l.MaxStatusPages
	case LimitPrivateProbes:
		return &l.MaxPrivateProbes
	}
	return nil
}

// Overrides are per-tenant changes to the plan limits. A name that is absent
// keeps the plan value; a name mapped to nil lifts the limit for the tenant.
type Overrides map[string]*int

// ParseLimits parses and validates the limits of a plan. Empty input and null
// fields mean unlimited. Unknown fields, non-integers and out-of-range values
// are rejected.
func ParseLimits(raw []byte) (Limits, error) {
	o, err := parse(raw)
	if err != nil {
		return Limits{}, err
	}
	var l Limits
	for name, v := range o {
		*l.field(name) = v
	}
	return l, nil
}

// ParseOverrides parses and validates per-tenant overrides. Unlike plan limits,
// a null value is kept: it lifts the plan limit for the tenant.
func ParseOverrides(raw []byte) (Overrides, error) {
	return parse(raw)
}

func parse(raw []byte) (Overrides, error) {
	out := Overrides{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: limits must be a JSON object", ErrInvalidLimits)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the limits object", ErrInvalidLimits)
	}
	if m == nil {
		return out, nil // literal null
	}
	for name, v := range m {
		if (&Limits{}).field(name) == nil {
			return nil, fmt.Errorf("%w: unknown limit %q; known limits are %s", ErrInvalidLimits, name, strings.Join(limitNames, ", "))
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			out[name] = nil
			continue
		}
		// json.Number also accepts a quoted number; only a bare number counts.
		if t := bytes.TrimSpace(v); len(t) == 0 || t[0] == '"' {
			return nil, fmt.Errorf("%w: %s must be an integer or null", ErrInvalidLimits, name)
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, fmt.Errorf("%w: %s must be an integer or null", ErrInvalidLimits, name)
		}
		i, err := n.Int64()
		if err != nil {
			return nil, fmt.Errorf("%w: %s must be an integer or null", ErrInvalidLimits, name)
		}
		lo := int64(0)
		if name == LimitMinInterval {
			lo = 1
		}
		if i < lo || i > maxLimitValue {
			return nil, fmt.Errorf("%w: %s must be between %d and %d", ErrInvalidLimits, name, lo, maxLimitValue)
		}
		val := int(i)
		out[name] = &val
	}
	return out, nil
}

// Effective returns the limits of a tenant: each override replaces the plan
// value, a nil override lifts it.
func Effective(plan Limits, o Overrides) Limits {
	eff := plan
	for name, v := range o {
		if f := eff.field(name); f != nil {
			*f = v
		}
	}
	return eff
}

// Canonical returns the limits as compact JSON with fields in a fixed order,
// so that every replica stores the same bytes.
func (l Limits) Canonical() []byte {
	b, _ := json.Marshal(l) // struct of *int; cannot fail
	return b
}

// Canonical returns the overrides as compact JSON with sorted keys. Nil values
// are kept as null.
func (o Overrides) Canonical() []byte {
	if len(o) == 0 {
		return []byte("{}")
	}
	b, _ := json.Marshal(map[string]*int(o)) // map keys are sorted by encoding/json
	return b
}

// maxNameLength is the longest tenant or plan name, in characters.
const maxNameLength = 100

// ValidName reports whether name is 1 to 100 characters of valid UTF-8 without
// control characters and without leading or trailing space.
func ValidName(name string) bool {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxNameLength {
		return false
	}
	if strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == ' ' || r == ' ' || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// SanitizeName turns an untrusted display name (e.g. an organization name from
// a token) into a valid tenant name: control and format characters are
// dropped, white space is collapsed and the result is cut to 100 characters.
// It returns fallback if nothing usable is left.
func SanitizeName(name, fallback string) string {
	if !utf8.ValidString(name) {
		return fallback
	}
	var b strings.Builder
	space := false
	n := 0
	for _, r := range name {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ':
			continue
		}
		if n >= maxNameLength {
			break
		}
		if space {
			if n+1 >= maxNameLength {
				break
			}
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	if out := b.String(); ValidName(out) {
		return out
	}
	return fallback
}

// maxIDLength is the longest tenant or plan ID, in bytes.
const maxIDLength = 64

// ValidID reports whether id is a usable tenant or plan ID: 1 to 64 bytes of
// lower-case ASCII letters, digits and '-', starting with a letter or digit.
// UUIDs qualify. Reserved IDs such as "_system" do not.
func ValidID(id string) bool {
	if id == "" || len(id) > maxIDLength || id[0] == '-' {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// maxOrgIDLength is the longest accepted identity-provider organization ID, in bytes.
const maxOrgIDLength = 128

// ValidOrgID reports whether id is a usable organization ID: 1 to 128 bytes of
// ASCII letters, digits, '.', '_', '@' and '-' (ADR 0027).
func ValidOrgID(id string) bool {
	if id == "" || len(id) > maxOrgIDLength {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '_', c == '@', c == '-':
		default:
			return false
		}
	}
	return true
}

// OrgIDRule describes ValidOrgID for error messages.
const OrgIDRule = "organization ID must be 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', '@' or '-'"

// Protected reports whether a tenant may never be suspended or deleted.
func Protected(tenantID string) bool {
	return tenantID == SystemTenantID || tenantID == DefaultTenantID
}

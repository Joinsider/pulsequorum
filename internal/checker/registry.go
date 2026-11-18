// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package checker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// ErrUnknownType is returned for a monitor type without a registered checker.
var ErrUnknownType = errors.New("unknown monitor type")

// ValidationError lists configuration problems by JSON pointer (FA-MON-02).
type ValidationError struct {
	Fields []FieldError
}

// FieldError is one configuration problem.
type FieldError struct {
	Field   string // JSON pointer, e.g. "/url"
	Message string
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "invalid configuration"
	}
	f := e.Fields[0]
	msg := fmt.Sprintf("invalid configuration: %s: %s", f.Field, f.Message)
	if len(e.Fields) > 1 {
		msg += fmt.Sprintf(" (and %d more)", len(e.Fields)-1)
	}
	return msg
}

// Registry holds the available checkers. Default is the process-wide registry
// the built-in checkers register with at init time; it is the only global
// state allowed by the conventions (CLAUDE.md).
type Registry struct {
	mu       sync.RWMutex
	checkers map[string]entry
}

type entry struct {
	checker Checker
	schema  *jsonschema.Schema
}

// Default is the process-wide registry.
var Default = NewRegistry()

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{checkers: map[string]entry{}}
}

// Register adds a checker and compiles its schema. It panics on a duplicate
// type or an invalid schema, which are programming errors.
func (r *Registry) Register(c Checker) {
	schema, err := compileSchema(c.Type(), c.ConfigSchema())
	if err != nil {
		panic(fmt.Sprintf("checker %s: invalid config schema: %v", c.Type(), err))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checkers[c.Type()]; dup {
		panic("checker registered twice: " + c.Type())
	}
	r.checkers[c.Type()] = entry{checker: c, schema: schema}
}

// Lookup returns the checker for a type.
func (r *Registry) Lookup(typ string) (Checker, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.checkers[typ]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, typ)
	}
	return e.checker, nil
}

// Checkers returns all checkers ordered by type.
func (r *Registry) Checkers() []Checker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Checker, 0, len(r.checkers))
	for _, e := range r.checkers {
		out = append(out, e.checker)
	}
	slices.SortFunc(out, func(a, b Checker) int { return compareStrings(a.Type(), b.Type()) })
	return out
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Validate checks cfg against the type's JSON Schema, then its semantic
// rules, then the notation of its destination hosts (ValidateHost without a
// policy). Problems are reported as *ValidationError.
func (r *Registry) Validate(typ string, cfg json.RawMessage) error {
	r.mu.RLock()
	e, ok := r.checkers[typ]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownType, typ)
	}
	if len(bytes.TrimSpace(cfg)) == 0 {
		cfg = json.RawMessage("{}")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(cfg))
	if err != nil {
		return &ValidationError{Fields: []FieldError{{Field: "", Message: "configuration is not valid JSON"}}}
	}
	if err := e.schema.Validate(doc); err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			return &ValidationError{Fields: flatten(verr)}
		}
		return err
	}
	if err := e.checker.Validate(cfg); err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			return err
		}
		return &ValidationError{Fields: []FieldError{{Message: err.Error()}}}
	}
	// Notation rules for destination hosts apply even with SSRF protection
	// off (ADR 0030); the policy itself is checked by ValidateTargets.
	return validateTargets(e.checker, cfg, nil)
}

func compileSchema(typ string, raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	url := "uptimecluster:checker/" + typ + ".json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

// printer renders schema error messages; LocalizedString needs a non-nil printer.
var printer = message.NewPrinter(language.English)

// flatten turns the schema error tree into one entry per failing leaf.
func flatten(err *jsonschema.ValidationError) []FieldError {
	if len(err.Causes) == 0 {
		base := "/" + joinPointer(err.InstanceLocation)
		// Point at the affected property, so the UI can mark the right field.
		var props []string
		switch k := err.ErrorKind.(type) {
		case *kind.Required:
			props = k.Missing
		case *kind.AdditionalProperties:
			props = k.Properties
		}
		if len(props) > 0 {
			out := make([]FieldError, 0, len(props))
			for _, p := range props {
				out = append(out, FieldError{Field: strings.TrimSuffix(base, "/") + "/" + p, Message: err.ErrorKind.LocalizedString(printer)})
			}
			return out
		}
		return []FieldError{{Field: base, Message: err.ErrorKind.LocalizedString(printer)}}
	}
	var out []FieldError
	for _, c := range err.Causes {
		out = append(out, flatten(c)...)
	}
	return out
}

func joinPointer(parts []string) string {
	var b bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('/')
		}
		b.WriteString(p)
	}
	return b.String()
}

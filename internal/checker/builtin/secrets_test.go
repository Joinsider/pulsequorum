// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package builtin_test

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker"
	_ "github.com/joinsider/pulsequorum/internal/checker/builtin"
	"github.com/joinsider/pulsequorum/internal/configsecret"
)

// secretName matches property names that hold credentials.
var secretName = regexp.MustCompile(`(?i)password|passwd|token|secret|api_?key|private_?key|credential`)

// ADR 0015: a secret field the schema does not mark writeOnly would be stored
// and returned in plaintext. Every built-in type must mark such fields.
func TestSecretFieldsAreWriteOnly(t *testing.T) {
	for _, c := range checker.Default.Checkers() {
		paths, err := configsecret.PathsOf(c.ConfigSchema())
		if err != nil {
			t.Errorf("%s: %v", c.Type(), err)
			continue
		}
		marked := map[string]bool{}
		for _, p := range paths {
			marked[strings.Join(p, "/")] = true
		}
		var schema any
		if err := json.Unmarshal(c.ConfigSchema(), &schema); err != nil {
			t.Fatal(err)
		}
		for _, path := range propertyPaths(schema, nil) {
			if secretName.MatchString(path[len(path)-1]) && !marked[strings.Join(path, "/")] {
				t.Errorf("%s: field %s looks like a secret but is not writeOnly", c.Type(), strings.Join(path, "/"))
			}
		}
	}
}

func propertyPaths(node any, at []string) [][]string {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var out [][]string
	if props, ok := obj["properties"].(map[string]any); ok {
		for name, sub := range props {
			p := append(slices.Clone(at), name)
			out = append(out, p)
			out = append(out, propertyPaths(sub, p)...)
		}
	}
	for _, key := range []string{"additionalProperties", "items"} {
		out = append(out, propertyPaths(obj[key], append(slices.Clone(at), "*"))...)
	}
	return out
}

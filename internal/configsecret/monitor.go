// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package configsecret

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/store"
)

// Schemas returns the secret paths of each monitor type of a registry. The
// paths are derived once per type.
type Schemas struct {
	Registry *checker.Registry
	cache    sync.Map // type -> Paths
}

// Paths returns the secret paths of a type; unknown types have none.
func (s *Schemas) Paths(typ string) (Paths, error) {
	if p, ok := s.cache.Load(typ); ok {
		return p.(Paths), nil
	}
	c, err := s.Registry.Lookup(typ)
	if err != nil {
		return nil, nil //nolint:nilerr // an unknown type cannot be validated or run; it has no secret paths
	}
	p, err := PathsOf(c.ConfigSchema())
	if err != nil {
		return nil, err
	}
	s.cache.Store(typ, p)
	return p, nil
}

// ResolveMonitor returns the configuration of a stored monitor with its
// secrets decrypted, for running a check.
func ResolveMonitor(ctx context.Context, kr Keyring, m store.Monitor) (json.RawMessage, error) {
	if len(m.Secrets) == 0 {
		return m.Config, nil
	}
	return Resolve(ctx, kr, m.ID, m.Config, m.Secrets)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package cluster

import "github.com/joinsider/pulsequorum/internal/publicorigin"

// MaxPublicURLLen is the longest public node URL the registry accepts, in
// bytes, before normalization (ADR 0036).
const MaxPublicURLLen = publicorigin.MaxLen

// NormalizePublicOrigin checks the public URL of a voter and returns its
// canonical origin (ADR 0036); see publicorigin.Normalize for the rule.
func NormalizePublicOrigin(raw string) (string, error) { return publicorigin.Normalize(raw) }

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"context"
	"fmt"
	"log/slog"
)

// BuildPolicy says when EnsureImage builds an image.
type BuildPolicy string

const (
	// BuildAuto builds only when the image is missing.
	BuildAuto BuildPolicy = "auto"
	// BuildAlways builds even when the image exists.
	BuildAlways BuildPolicy = "always"
	// BuildNever never builds; a missing image is an error.
	BuildNever BuildPolicy = "never"
)

// ParseBuildPolicy validates the -build flag value.
func ParseBuildPolicy(s string) (BuildPolicy, error) {
	switch BuildPolicy(s) {
	case BuildAuto, BuildAlways, BuildNever:
		return BuildPolicy(s), nil
	}
	return "", fmt.Errorf("-build must be auto, always or never, got %q", s)
}

// EnsureImage builds spec.Tag according to policy and reports whether it built.
func EnsureImage(ctx context.Context, rt *Runtime, spec BuildSpec, policy BuildPolicy, log *slog.Logger) (bool, error) {
	exists, err := rt.ImageExists(ctx, spec.Tag)
	if err != nil {
		return false, fmt.Errorf("check image %s: %w", spec.Tag, err)
	}
	switch {
	case policy == BuildNever:
		if !exists {
			return false, fmt.Errorf("image %s is missing and -build=never", spec.Tag)
		}
		return false, nil
	case policy == BuildAuto && exists:
		return false, nil
	}
	if log != nil {
		log.Info("building image", "tag", spec.Tag, "context", spec.Context)
	}
	if err := rt.Build(ctx, spec); err != nil {
		return false, fmt.Errorf("build %s: %w", spec.Tag, err)
	}
	return true, nil
}

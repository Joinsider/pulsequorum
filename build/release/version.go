// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Command release holds the pure helpers used by the release pipeline:
// SemVer tag selection and next-version computation (version.go), and
// Conventional Commits changelog rendering (changelog.go).
package main

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// semverPattern matches MAJOR.MINOR.PATCH with an optional rc, alpha or beta
// pre-release suffix. The leading "v" is optional here; parseVersion enforces
// it where a tag is required.
var semverPattern = regexp.MustCompile(`^(v?)(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(rc|alpha|beta)\.(0|[1-9][0-9]*))?$`)

// version is a parsed SemVer value restricted to the supported pre-release set.
type version struct {
	major, minor, patch int
	pre                 string // "", "alpha", "beta" or "rc"
	preNum              int
}

// parseVersion parses s strictly. If requireV is true, the "v" prefix is mandatory.
func parseVersion(s string, requireV bool) (version, error) {
	m := semverPattern.FindStringSubmatch(s)
	if m == nil {
		return version{}, fmt.Errorf("invalid version %q: want vMAJOR.MINOR.PATCH with optional -rc.N, -alpha.N or -beta.N", s)
	}
	if requireV && m[1] != "v" {
		return version{}, fmt.Errorf("invalid version %q: missing v prefix", s)
	}
	var v version
	nums := []*int{&v.major, &v.minor, &v.patch}
	for i, dst := range nums {
		n, err := strconv.Atoi(m[i+2])
		if err != nil {
			return version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
		*dst = n
	}
	if m[5] != "" {
		n, err := strconv.Atoi(m[6])
		if err != nil {
			return version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
		v.pre = m[5]
		v.preNum = n
	}
	return v, nil
}

// compare returns -1, 0 or 1 following SemVer 2.0 precedence for the supported
// pre-release identifiers. A release outranks any pre-release of the same base.
// Pre-release names compare lexically (alpha < beta < rc), numbers numerically.
func (v version) compare(o version) int {
	if c := cmp.Compare(v.major, o.major); c != 0 {
		return c
	}
	if c := cmp.Compare(v.minor, o.minor); c != 0 {
		return c
	}
	if c := cmp.Compare(v.patch, o.patch); c != 0 {
		return c
	}
	switch {
	case v.pre == "" && o.pre == "":
		return 0
	case v.pre == "":
		return 1
	case o.pre == "":
		return -1
	}
	if c := strings.Compare(v.pre, o.pre); c != 0 {
		return c
	}
	return cmp.Compare(v.preNum, o.preNum)
}

// String renders the version as a tag, e.g. "v0.2.0-rc.1".
func (v version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
	if v.pre != "" {
		s += fmt.Sprintf("-%s.%d", v.pre, v.preNum)
	}
	return s
}

// LatestTag returns the highest release or pre-release tag of the form
// vMAJOR.MINOR.PATCH[-rc.N|-alpha.N|-beta.N]. Anything else is ignored.
// It returns "" if no tag qualifies.
func LatestTag(tags []string) string {
	best := ""
	var bestVersion version
	for _, tag := range tags {
		v, err := parseVersion(tag, true)
		if err != nil {
			continue
		}
		if best == "" || v.compare(bestVersion) > 0 {
			best = tag
			bestVersion = v
		}
	}
	return best
}

// NextVersion computes the next release tag ("vX.Y.Z[-pre]") after latest.
//
// latest is "" (no release yet) or a tag as returned by LatestTag. If explicit
// is set, it wins over bump and must be a version (with or without "v") that is
// strictly greater than latest; bump is then not checked. Otherwise bump must be
// "patch", "minor" or "major".
//
// A major bump is refused while the major version is 0, so that 1.0.0 is only
// reached on purpose through an explicit version. A pre-release latest is
// finalized by the bump: its base version is the pending release.
func NextVersion(latest, bump, explicit string) (string, error) {
	var base *version
	if latest != "" {
		v, err := parseVersion(latest, true)
		if err != nil {
			return "", fmt.Errorf("latest tag: %w", err)
		}
		base = &v
	}

	if explicit != "" {
		want, err := parseVersion(explicit, false)
		if err != nil {
			return "", fmt.Errorf("explicit version: %w", err)
		}
		if base != nil && want.compare(*base) <= 0 {
			return "", fmt.Errorf("explicit version %s must be greater than latest %s", want, *base)
		}
		return want.String(), nil
	}

	switch bump {
	case "patch", "minor", "major":
	default:
		return "", fmt.Errorf("invalid bump %q: want patch, minor or major", bump)
	}

	if base == nil {
		switch bump {
		case "patch":
			return "v0.0.1", nil
		case "minor":
			return "v0.1.0", nil
		default:
			return "", errors.New("major bump without a previous release is refused: pass an explicit version 1.0.0")
		}
	}

	if base.major == 0 && bump == "major" {
		return "", fmt.Errorf("major bump from %s is refused: pass an explicit version 1.0.0 to leave the 0.x line", *base)
	}
	return bumpFromBase(*base, bump).String(), nil
}

func bumpFromBase(b version, bump string) version {
	if b.pre == "" {
		switch bump {
		case "patch":
			return version{major: b.major, minor: b.minor, patch: b.patch + 1}
		case "minor":
			return version{major: b.major, minor: b.minor + 1}
		default:
			return version{major: b.major + 1}
		}
	}
	// The pre-release is the pending release of its base version, so the
	// bump finalizes it and only increments where the base has not moved yet.
	switch bump {
	case "patch":
		return version{major: b.major, minor: b.minor, patch: b.patch}
	case "minor":
		if b.patch == 0 {
			return version{major: b.major, minor: b.minor}
		}
		return version{major: b.major, minor: b.minor + 1}
	default:
		if b.minor == 0 && b.patch == 0 {
			return version{major: b.major}
		}
		return version{major: b.major + 1}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import "testing"

func TestLatestTag(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string
	}{
		{name: "no tags", tags: nil, want: ""},
		{name: "only non-matching tags", tags: []string{"junk", "1.2.3", "v1.0", "release-1"}, want: ""},
		{name: "numeric comparison v0.10.0 beats v0.9.9", tags: []string{"v0.9.9", "v0.10.0"}, want: "v0.10.0"},
		{name: "ignores invalid among valid", tags: []string{"v0.9.9", "v0.10.0", "junk", "1.2.3", "v1.0"}, want: "v0.10.0"},
		{name: "rc below release", tags: []string{"v1.0.0-rc.2", "v1.0.0"}, want: "v1.0.0"},
		{name: "release above rc of higher base loses to rc of next", tags: []string{"v1.0.0", "v1.0.1-rc.1"}, want: "v1.0.1-rc.1"},
		{name: "pre-release of higher base beats lower release", tags: []string{"v0.9.9", "v1.0.0-rc.1"}, want: "v1.0.0-rc.1"},
		{name: "rc numbers compared numerically", tags: []string{"v1.0.0-rc.2", "v1.0.0-rc.10"}, want: "v1.0.0-rc.10"},
		{name: "rc beats beta", tags: []string{"v1.0.0-beta.9", "v1.0.0-rc.1"}, want: "v1.0.0-rc.1"},
		{name: "beta beats alpha", tags: []string{"v1.0.0-alpha.9", "v1.0.0-beta.2"}, want: "v1.0.0-beta.2"},
		{name: "major beats minor", tags: []string{"v1.9.9", "v2.0.0-alpha.1"}, want: "v2.0.0-alpha.1"},
		{name: "uppercase V rejected", tags: []string{"V1.0.0", "v0.1.0"}, want: "v0.1.0"},
		{name: "leading zero rejected", tags: []string{"v01.0.0", "v0.1.0"}, want: "v0.1.0"},
		{name: "build metadata rejected", tags: []string{"v1.0.0-rc.1+build"}, want: ""},
		{name: "shell injection rejected", tags: []string{"v1.0.0;rm -rf"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LatestTag(tt.tags); got != tt.want {
				t.Errorf("LatestTag(%q) = %q, want %q", tt.tags, got, tt.want)
			}
		})
	}
}

func TestNextVersion(t *testing.T) {
	tests := []struct {
		name     string
		latest   string
		bump     string
		explicit string
		want     string
		wantErr  bool
	}{
		// No previous release.
		{name: "no tag patch", latest: "", bump: "patch", want: "v0.0.1"},
		{name: "no tag minor", latest: "", bump: "minor", want: "v0.1.0"},
		{name: "no tag major refused", latest: "", bump: "major", wantErr: true},
		{name: "no tag explicit 1.0.0", latest: "", explicit: "1.0.0", want: "v1.0.0"},

		// Release latest.
		{name: "patch", latest: "v0.0.1", bump: "patch", want: "v0.0.2"},
		{name: "minor resets patch", latest: "v0.3.4", bump: "minor", want: "v0.4.0"},
		{name: "patch on 0.x", latest: "v0.3.4", bump: "patch", want: "v0.3.5"},
		{name: "major on 0.x refused", latest: "v0.3.4", bump: "major", wantErr: true},
		{name: "major from 1.x", latest: "v1.2.3", bump: "major", want: "v2.0.0"},
		{name: "minor from 1.x", latest: "v1.2.3", bump: "minor", want: "v1.3.0"},
		{name: "patch from 1.x", latest: "v1.2.3", bump: "patch", want: "v1.2.4"},
		{name: "minor carries patch 9 over", latest: "v0.9.9", bump: "minor", want: "v0.10.0"},

		// Pre-release latest: the bump finalizes the pending base version.
		{name: "patch from rc of 0.2.0 drops suffix", latest: "v0.2.0-rc.1", bump: "patch", want: "v0.2.0"},
		{name: "minor from rc of 0.2.0 drops suffix", latest: "v0.2.0-rc.1", bump: "minor", want: "v0.2.0"},
		{name: "minor from rc of 0.2.1 increments minor", latest: "v0.2.1-rc.1", bump: "minor", want: "v0.3.0"},
		{name: "patch from rc of 0.2.1 drops suffix", latest: "v0.2.1-rc.1", bump: "patch", want: "v0.2.1"},
		{name: "major from rc of 1.0.0 drops suffix", latest: "v1.0.0-rc.1", bump: "major", want: "v1.0.0"},
		{name: "major from rc of 1.1.0 increments major", latest: "v1.1.0-rc.2", bump: "major", want: "v2.0.0"},
		{name: "major from rc of 0.x refused", latest: "v0.9.0-rc.1", bump: "major", wantErr: true},
		{name: "patch from rc.10", latest: "v0.2.0-rc.10", bump: "patch", want: "v0.2.0"},

		// Explicit version wins over bump.
		{name: "explicit 1.0.0 over 0.9.2", latest: "v0.9.2", explicit: "1.0.0", want: "v1.0.0"},
		{name: "explicit with v prefix", latest: "v0.9.2", explicit: "v1.0.0", want: "v1.0.0"},
		{name: "explicit wins over bump", latest: "v0.9.2", bump: "patch", explicit: "1.0.0", want: "v1.0.0"},
		{name: "explicit ignores invalid bump", latest: "v0.9.2", bump: "bogus", explicit: "1.0.0", want: "v1.0.0"},
		{name: "explicit major from 0.x allowed", latest: "v0.9.2", bump: "major", explicit: "1.0.0", want: "v1.0.0"},
		{name: "explicit rc over release base", latest: "v0.9.1", explicit: "0.9.2-rc.1", want: "v0.9.2-rc.1"},
		{name: "explicit 0.1.0 below v0.2.0", latest: "v0.2.0", explicit: "0.1.0", wantErr: true},
		{name: "explicit equal to latest", latest: "v0.2.0", explicit: "0.2.0", wantErr: true},
		{name: "explicit equal to latest with v", latest: "v0.2.0", explicit: "v0.2.0", wantErr: true},
		{name: "explicit release below its own rc", latest: "v1.0.0-rc.1", explicit: "1.0.0-rc.1", wantErr: true},
		{name: "explicit rc below release of same base", latest: "v1.0.0", explicit: "1.0.0-rc.1", wantErr: true},
		{name: "explicit rc.2 over rc.1", latest: "v1.0.0-rc.1", explicit: "1.0.0-rc.2", want: "v1.0.0-rc.2"},
		{name: "explicit rc.10 over rc.9", latest: "v1.0.0-rc.9", explicit: "1.0.0-rc.10", want: "v1.0.0-rc.10"},
		{name: "explicit release over rc of same base", latest: "v1.0.0-rc.1", explicit: "1.0.0", want: "v1.0.0"},
		{name: "explicit with no latest", latest: "", explicit: "0.5.0-beta.1", want: "v0.5.0-beta.1"},

		// Invalid latest.
		{name: "latest without v rejected", latest: "1.0.0", bump: "patch", wantErr: true},
		{name: "latest garbage rejected", latest: "garbage", bump: "patch", wantErr: true},

		// Invalid bump.
		{name: "unknown bump", latest: "v0.1.0", bump: "Patch", wantErr: true},
		{name: "empty bump without explicit", latest: "v0.1.0", bump: "", wantErr: true},

		// Invalid explicit versions.
		{name: "explicit 1.0 incomplete", latest: "", explicit: "1.0", wantErr: true},
		{name: "explicit with injection", latest: "", explicit: "v1.0.0;rm -rf", wantErr: true},
		{name: "explicit leading zero", latest: "", explicit: "01.2.3", wantErr: true},
		{name: "explicit leading zero in pre number", latest: "", explicit: "1.0.0-rc.01", wantErr: true},
		{name: "explicit leading space", latest: "", explicit: " 1.0.0", wantErr: true},
		{name: "explicit trailing space", latest: "", explicit: "1.0.0 ", wantErr: true},
		{name: "explicit unsupported pre-release name", latest: "", explicit: "1.0.0-dev.1", wantErr: true},
		{name: "explicit pre-release without number", latest: "", explicit: "1.0.0-rc", wantErr: true},
		{name: "explicit build metadata", latest: "", explicit: "1.0.0+build", wantErr: true},
		{name: "explicit uppercase V", latest: "", explicit: "V1.0.0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NextVersion(tt.latest, tt.bump, tt.explicit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NextVersion(%q, %q, %q) error = %v, wantErr %v", tt.latest, tt.bump, tt.explicit, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NextVersion(%q, %q, %q) = %q, want %q", tt.latest, tt.bump, tt.explicit, got, tt.want)
			}
		})
	}
}

func TestVersionCompareOrdering(t *testing.T) {
	// Strictly ascending by SemVer 2.0 precedence.
	ascending := []string{
		"v0.0.1",
		"v0.1.0-alpha.1",
		"v0.1.0-beta.2",
		"v0.1.0-rc.1",
		"v0.1.0",
		"v0.10.0",
		"v1.0.0-rc.9",
		"v1.0.0-rc.10",
		"v1.0.0",
	}
	for i, a := range ascending {
		for j, b := range ascending {
			va, err := parseVersion(a, true)
			if err != nil {
				t.Fatalf("parseVersion(%q): %v", a, err)
			}
			vb, err := parseVersion(b, true)
			if err != nil {
				t.Fatalf("parseVersion(%q): %v", b, err)
			}
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := va.compare(vb); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

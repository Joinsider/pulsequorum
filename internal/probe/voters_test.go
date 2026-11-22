// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

func TestParseSeed(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "host port", raw: "voter1.example.com:8443", want: "voter1.example.com:8443"},
		{name: "host port trailing slash", raw: "voter1.example.com:8443/", want: "voter1.example.com:8443"},
		{name: "https host port", raw: "https://voter1.example.com:8443", want: "voter1.example.com:8443"},
		{name: "https host port trailing slash", raw: "https://voter1.example.com:8443/", want: "voter1.example.com:8443"},
		{name: "trimmed whitespace", raw: "  https://10.0.0.1:9443/  ", want: "10.0.0.1:9443"},
		{name: "ipv6", raw: "[::1]:8443", want: "[::1]:8443"},
		{name: "https ipv6 trailing slash", raw: "https://[::1]:8443/", want: "[::1]:8443"},
		{name: "empty", raw: "", wantErr: true},
		{name: "http rejected", raw: "http://voter1.example.com:8443", wantErr: true},
		{name: "other scheme rejected", raw: "grpc://voter1.example.com:8443", wantErr: true},
		{name: "path rejected", raw: "voter1.example.com:8443/api", wantErr: true},
		{name: "https path rejected", raw: "https://voter1.example.com:8443/api", wantErr: true},
		{name: "double slash rejected", raw: "https://voter1.example.com:8443//", wantErr: true},
		{name: "query rejected", raw: "https://voter1.example.com:8443?foo=bar", wantErr: true},
		{name: "bare query rejected", raw: "voter1.example.com:8443?", wantErr: true},
		{name: "fragment rejected", raw: "https://voter1.example.com:8443#frag", wantErr: true},
		{name: "userinfo rejected", raw: "https://user@voter1.example.com:8443", wantErr: true},
		{name: "missing port", raw: "voter1.example.com", wantErr: true},
		{name: "empty host", raw: ":8443", wantErr: true},
		{name: "empty port", raw: "voter1.example.com:", wantErr: true},
		{name: "non numeric port", raw: "voter1.example.com:abc", wantErr: true},
		{name: "port zero", raw: "voter1.example.com:0", wantErr: true},
		{name: "port too high", raw: "voter1.example.com:65536", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSeed(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSeed(%q) = %q, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSeed(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSeed(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func noShuffle(n int) int {
	return n - 1
}

func TestVoterListDeduplicationUnionSeedsFirst(t *testing.T) {
	dir := t.TempDir()
	votersPath := filepath.Join(dir, "probe", "voters.json")
	if err := os.MkdirAll(filepath.Dir(votersPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(votersPath, []byte(`{"voters":["v2:8443","v3:8443","v3:8443",""]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	vl, err := NewVoterList(dir, []string{"https://v1:8443/", "v2:8443", "v1:8443"}, noShuffle)
	if err != nil {
		t.Fatalf("NewVoterList: %v", err)
	}

	got := vl.Candidates()
	want := []string{"v1:8443", "v2:8443", "v3:8443"}
	if !slices.Equal(got, want) {
		t.Fatalf("Candidates() = %v, want %v", got, want)
	}

	// Mutating the returned slice must not affect VoterList.
	got[0] = "mutated"
	if !slices.Equal(vl.Candidates(), want) {
		t.Fatalf("Candidates() mutated internal state: %v", vl.Candidates())
	}
}

func TestVoterListStartupShuffleDeterministic(t *testing.T) {
	dir := t.TempDir()
	seeds := []string{"v1:8443", "v2:8443", "v3:8443", "v4:8443"}

	rng1 := rand.New(rand.NewPCG(42, 99)) //nolint:gosec // G404: deterministic shuffle test
	vl1, err := NewVoterList(dir, seeds, rng1.IntN)
	if err != nil {
		t.Fatalf("NewVoterList 1: %v", err)
	}

	rng2 := rand.New(rand.NewPCG(42, 99)) //nolint:gosec // G404: deterministic shuffle test
	vl2, err := NewVoterList(dir, seeds, rng2.IntN)
	if err != nil {
		t.Fatalf("NewVoterList 2: %v", err)
	}

	c1 := vl1.Candidates()
	c2 := vl2.Candidates()
	if !slices.Equal(c1, c2) {
		t.Fatalf("deterministic shuffle mismatch: %v vs %v", c1, c2)
	}
	if slices.Equal(c1, seeds) {
		t.Fatalf("expected shuffled order to differ from input %v", seeds)
	}
}

func TestVoterListPersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	vl, err := NewVoterList(dir, []string{"seed1:8443"}, noShuffle)
	if err != nil {
		t.Fatalf("NewVoterList: %v", err)
	}

	err = vl.Update([]*pb.NodeInfo{
		{AdvertiseAddr: "voter-b:8443"},
		{AdvertiseAddr: ""},
		nil,
		{AdvertiseAddr: "voter-a:8443"},
		{AdvertiseAddr: "seed1:8443"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Existing rotation preserved ("seed1:8443" first), new voters appended.
	want := []string{"seed1:8443", "voter-a:8443", "voter-b:8443"}
	if got := vl.Candidates(); !slices.Equal(got, want) {
		t.Fatalf("Candidates() after Update = %v, want %v", got, want)
	}

	votersPath := filepath.Join(dir, "probe", "voters.json")
	raw, err := os.ReadFile(votersPath) //nolint:gosec // G304: test path under TempDir
	if err != nil {
		t.Fatalf("read voters.json: %v", err)
	}
	var vf votersFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("unmarshal voters.json: %v", err)
	}
	wantPersisted := []string{"seed1:8443", "voter-a:8443", "voter-b:8443"}
	if !slices.Equal(vf.Voters, wantPersisted) {
		t.Fatalf("persisted voters = %v, want %v", vf.Voters, wantPersisted)
	}

	// Reload from disk with a new VoterList.
	vl2, err := NewVoterList(dir, []string{"seed1:8443"}, noShuffle)
	if err != nil {
		t.Fatalf("NewVoterList reload: %v", err)
	}
	if got := vl2.Candidates(); !slices.Equal(got, want) {
		t.Fatalf("reloaded Candidates() = %v, want %v", got, want)
	}
}

func TestVoterListNoRewriteWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	vl, err := NewVoterList(dir, []string{"seed1:8443"}, noShuffle)
	if err != nil {
		t.Fatalf("NewVoterList: %v", err)
	}

	voters := []*pb.NodeInfo{
		{AdvertiseAddr: "voter-b:8443"},
		{AdvertiseAddr: "voter-a:8443"},
	}
	if err := vl.Update(voters); err != nil {
		t.Fatalf("first Update: %v", err)
	}

	votersPath := filepath.Join(dir, "probe", "voters.json")
	// Set an older mtime so we can detect any rewrite reliably.
	past := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(votersPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Same set in different order + duplicates + empty should NOT rewrite voters.json.
	err = vl.Update([]*pb.NodeInfo{
		{AdvertiseAddr: "voter-a:8443"},
		{AdvertiseAddr: "voter-b:8443"},
		{AdvertiseAddr: "voter-a:8443"},
		{AdvertiseAddr: ""},
	})
	if err != nil {
		t.Fatalf("second Update: %v", err)
	}

	info, err := os.Stat(votersPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("voters.json was rewritten: modtime = %v, want %v", info.ModTime(), past)
	}
}

func TestVoterListUpdatePreservesRotationAndDropsRemovedLearned(t *testing.T) {
	dir := t.TempDir()
	// Start with shuffled order [v2, v1]
	vl, err := NewVoterList(dir, []string{"seed1:8443", "seed2:8443"}, func(int) int { return 0 })
	if err != nil {
		t.Fatalf("NewVoterList: %v", err)
	}
	initial := vl.Candidates()
	if len(initial) != 2 {
		t.Fatalf("expected 2 candidates, got %v", initial)
	}

	if err := vl.Update([]*pb.NodeInfo{
		{AdvertiseAddr: "learned-1:8443"},
		{AdvertiseAddr: "learned-2:8443"},
	}); err != nil {
		t.Fatalf("Update 1: %v", err)
	}
	want1 := append(slices.Clone(initial), "learned-1:8443", "learned-2:8443")
	if got := vl.Candidates(); !slices.Equal(got, want1) {
		t.Fatalf("Candidates() after Update 1 = %v, want %v", got, want1)
	}

	// Remove learned-1, add learned-3.
	if err := vl.Update([]*pb.NodeInfo{
		{AdvertiseAddr: "learned-2:8443"},
		{AdvertiseAddr: "learned-3:8443"},
	}); err != nil {
		t.Fatalf("Update 2: %v", err)
	}
	want2 := append(slices.Clone(initial), "learned-2:8443", "learned-3:8443")
	if got := vl.Candidates(); !slices.Equal(got, want2) {
		t.Fatalf("Candidates() after Update 2 = %v, want %v", got, want2)
	}
}

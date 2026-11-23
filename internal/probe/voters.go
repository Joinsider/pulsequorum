// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	pb "github.com/joinsider/pulsequorum/internal/pb/pulsequorum/v1"
)

const votersFileName = "voters.json"

type votersFile struct {
	Voters []string `json:"voters"`
}

// ParseSeed validates and normalizes a seed address.
// It accepts "host:port" or "https://host:port" (with an optional trailing "/"),
// and rejects "http://", paths, queries, fragments, and empty hosts or ports.
func ParseSeed(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty seed address")
	}

	if strings.Contains(s, "?") || strings.Contains(s, "#") {
		return "", fmt.Errorf("seed %q must not contain query or fragment", raw)
	}

	var candidate string
	if idx := strings.Index(s, "://"); idx >= 0 {
		scheme := strings.ToLower(s[:idx])
		if scheme != "https" {
			return "", fmt.Errorf("seed %q has unsupported scheme %q (only https or host:port allowed)", raw, s[:idx])
		}
		candidate = "https://" + s[idx+3:]
	} else {
		candidate = "https://" + s
	}

	u, err := url.Parse(candidate)
	if err != nil {
		return "", fmt.Errorf("invalid seed %q: %w", raw, err)
	}
	if u.User != nil {
		return "", fmt.Errorf("seed %q must not contain userinfo", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("seed %q must not contain a path", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("seed %q has empty host", raw)
	}

	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("seed %q must be host:port: %w", raw, err)
	}
	if host == "" {
		return "", fmt.Errorf("seed %q has empty host", raw)
	}
	if port == "" {
		return "", fmt.Errorf("seed %q has empty port", raw)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("seed %q has invalid port %q", raw, port)
	}

	return net.JoinHostPort(host, port), nil
}

// VoterList manages the deduplicated union of configured seeds and learned voter
// addresses, persisted to DATA_DIR/probe/voters.json.
type VoterList struct {
	mu         sync.RWMutex
	path       string
	seeds      []string
	learned    []string
	candidates []string
}

// NewVoterList creates a VoterList by parsing seeds, loading any persisted
// voters.json from dataDir/probe/voters.json, merging the union (seeds first,
// then learned voters, deduplicated), and shuffling once at startup.
func NewVoterList(dataDir string, seeds []string, shuffleRand func(n int) int) (*VoterList, error) {
	if shuffleRand == nil {
		shuffleRand = rand.IntN
	}

	parsedSeeds := make([]string, 0, len(seeds))
	seen := make(map[string]struct{}, len(seeds))
	for _, raw := range seeds {
		addr, err := ParseSeed(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		parsedSeeds = append(parsedSeeds, addr)
	}

	var path string
	var learned []string
	if dataDir != "" {
		path = filepath.Join(dataDir, "probe", votersFileName)
		var err error
		learned, err = loadVotersFile(path)
		if err != nil {
			return nil, err
		}
	}

	candidates := make([]string, 0, len(parsedSeeds)+len(learned))
	candidates = append(candidates, parsedSeeds...)
	for _, addr := range learned {
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		candidates = append(candidates, addr)
	}

	for i := len(candidates) - 1; i > 0; i-- {
		j := shuffleRand(i + 1)
		candidates[i], candidates[j] = candidates[j], candidates[i]
	}

	return &VoterList{
		path:       path,
		seeds:      parsedSeeds,
		learned:    learned,
		candidates: candidates,
	}, nil
}

// Candidates returns a copy of the current candidate addresses in rotation order.
func (v *VoterList) Candidates() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return slices.Clone(v.candidates)
}

// Update updates the learned voter set from voters. It only writes voters.json
// when the sorted set of learned voter addresses changes. New voters are
// appended to the candidate list while preserving the existing rotation order.
func (v *VoterList) Update(voters []*pb.NodeInfo) error {
	newLearned := extractSortedAddrs(voters)

	v.mu.Lock()
	defer v.mu.Unlock()

	if !slices.Equal(v.learned, newLearned) {
		if v.path != "" {
			if err := writeVotersFile(v.path, newLearned); err != nil {
				return err
			}
		}
		v.learned = newLearned
	}

	v.candidates = mergeCandidateAddrs(v.candidates, v.seeds, v.learned)
	return nil
}

func mergeCandidateAddrs(current, seeds, learned []string) []string {
	allowed := make(map[string]struct{}, len(seeds)+len(learned))
	for _, s := range seeds {
		allowed[s] = struct{}{}
	}
	for _, l := range learned {
		allowed[l] = struct{}{}
	}

	next := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	appendUnique := func(items []string, checkAllowed bool) {
		for _, item := range items {
			if checkAllowed {
				if _, ok := allowed[item]; !ok {
					continue
				}
			}
			if _, dup := seen[item]; dup {
				continue
			}
			seen[item] = struct{}{}
			next = append(next, item)
		}
	}
	appendUnique(current, true)
	appendUnique(seeds, false)
	appendUnique(learned, false)
	return next
}

func extractSortedAddrs(voters []*pb.NodeInfo) []string {
	seen := make(map[string]struct{}, len(voters))
	addrs := make([]string, 0, len(voters))
	for _, node := range voters {
		if node == nil {
			continue
		}
		addr := strings.TrimSpace(node.GetAdvertiseAddr())
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addrs = append(addrs, addr)
	}
	slices.Sort(addrs)
	return addrs
}

func loadVotersFile(path string) ([]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is under configured DataDir/probe/voters.json
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var vf votersFile
	if err := json.Unmarshal(data, &vf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := make(map[string]struct{}, len(vf.Voters))
	addrs := make([]string, 0, len(vf.Voters))
	for _, raw := range vf.Voters {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addrs = append(addrs, addr)
	}
	slices.Sort(addrs)
	return addrs, nil
}

func writeVotersFile(path string, voters []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create voter dir %s: %w", dir, err)
	}
	if voters == nil {
		voters = []string{}
	}
	data, err := json.MarshalIndent(votersFile{Voters: voters}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal voters: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, "voters-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp voters file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp voters file: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp voters file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp voters file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename voters file: %w", err)
	}
	cleanup = false
	return nil
}

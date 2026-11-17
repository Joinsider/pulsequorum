// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Limits of a cluster run.
const (
	MaxVoters          = 5
	minMonitorInterval = 20 * time.Second
	maxMonitorInterval = 24 * time.Hour
	voterHTTPPort      = 8080
	voterClusterPort   = 9443
)

// DefaultImage is the voter image the harness starts when none is given.
const DefaultImage = "uptimecluster:chaos"

// DefaultNetemImage is the fault sidecar image (test/chaos/netem).
const DefaultNetemImage = "uc-chaos-netem:latest"

var prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

// Config describes one cluster run. Two runs on the same machine must use
// different prefixes and disjoint port ranges.
type Config struct {
	// Runtime is "podman", "docker" or empty (see ResolveRuntime).
	Runtime string
	// Prefix names all containers, volumes and the network: <prefix>-v<i>,
	// <prefix>-v<i>-netem, <prefix>-v<i>-data, <prefix>-net.
	Prefix string
	// Image is the voter image.
	Image string
	// NetemImage is the fault sidecar image.
	NetemImage string
	// Voters is the number of voter slots, 1 to MaxVoters.
	Voters int
	// HostPortBase: voter i publishes its HTTP port on 127.0.0.1:HostPortBase+i.
	HostPortBase int
	// Interval is the monitor interval used by scenarios (20 s to 24 h).
	Interval time.Duration
	// CallbackHost is the name under which containers reach the host
	// (empty: DefaultCallbackHost of the runtime).
	CallbackHost string
	// Keep leaves containers, volumes, the network and the temp directory in
	// place after the run, for inspection.
	Keep bool
	// SelfCheckTargets are passed to every voter as SELF_CHECK_TARGETS (ADR 0022):
	// host:port or http(s) URLs. Empty leaves the self-check off.
	SelfCheckTargets []string
	// AlertBundleWindow is passed as ALERT_BUNDLE_WINDOW when set (a duration,
	// "0s" disables bundling, ADR 0023). Empty keeps the product default.
	AlertBundleWindow string
}

// DefaultConfig returns the defaults of the chaos driver.
func DefaultConfig() Config {
	return Config{
		Prefix:       "ucchaos",
		Image:        DefaultImage,
		NetemImage:   DefaultNetemImage,
		Voters:       3,
		HostPortBase: 18100,
		Interval:     minMonitorInterval,
	}
}

// DefaultCallbackHost returns the host name containers use to reach the host
// for the given runtime.
func DefaultCallbackHost(runtime string) string {
	if runtime == RuntimeDocker {
		return "host.docker.internal"
	}
	return "host.containers.internal"
}

// Validate checks the configuration and returns the first problem.
func (c Config) Validate() error {
	if !prefixPattern.MatchString(c.Prefix) {
		return fmt.Errorf("prefix %q: use 1 to 20 characters a-z, 0-9, '-', starting with a letter", c.Prefix)
	}
	if c.Prefix[len(c.Prefix)-1] == '-' {
		return fmt.Errorf("prefix %q must not end with '-'", c.Prefix)
	}
	if c.Image == "" {
		return errors.New("image must not be empty")
	}
	if c.NetemImage == "" {
		return errors.New("netem image must not be empty")
	}
	if c.Voters < 1 || c.Voters > MaxVoters {
		return fmt.Errorf("voters must be between 1 and %d, got %d", MaxVoters, c.Voters)
	}
	if c.HostPortBase < 1024 || c.HostPortBase+c.Voters > 65535 {
		return fmt.Errorf("port base %d with %d voters must lie in 1024..65535", c.HostPortBase, c.Voters)
	}
	if c.Interval < minMonitorInterval || c.Interval > maxMonitorInterval {
		return fmt.Errorf("interval must be between %s and %s, got %s", minMonitorInterval, maxMonitorInterval, c.Interval)
	}
	if c.AlertBundleWindow != "" {
		d, err := time.ParseDuration(c.AlertBundleWindow)
		if err != nil || d < 0 {
			return fmt.Errorf("alert bundle window %q must be a duration of 0 or more", c.AlertBundleWindow)
		}
	}
	for _, t := range c.SelfCheckTargets {
		if strings.TrimSpace(t) == "" || strings.Contains(t, ",") {
			return fmt.Errorf("self-check target %q must be one non-empty entry without commas", t)
		}
	}
	return nil
}

// Names derives the object names of one voter from the prefix.
type Names struct {
	prefix string
}

// NamesFor returns the name helper of a prefix.
func NamesFor(prefix string) Names { return Names{prefix: prefix} }

// Network is the user network all voters and sidecars share.
func (n Names) Network() string { return n.prefix + "-net" }

// Voter is the container and node name of voter i.
func (n Names) Voter(i int) string { return fmt.Sprintf("%s-v%d", n.prefix, i) }

// Sidecar is the fault container of voter i.
func (n Names) Sidecar(i int) string { return fmt.Sprintf("%s-v%d-netem", n.prefix, i) }

// Volume is the data volume of voter i.
func (n Names) Volume(i int) string { return fmt.Sprintf("%s-v%d-data", n.prefix, i) }

// ClusterAddress is the advertised Raft address of voter i (name and port).
func (n Names) ClusterAddress(i int) string {
	return fmt.Sprintf("%s:%d", n.Voter(i), voterClusterPort)
}

// HostPort returns the host port of voter i's HTTP listener.
func (c Config) HostPort(i int) int { return c.HostPortBase + i }

// BaseURL returns the URL of voter i's HTTP API as seen from the host.
func (c Config) BaseURL(i int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.HostPort(i))
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package harness

import (
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "defaults are valid", mutate: func(*Config) {}},
		{name: "prefix with uppercase", mutate: func(c *Config) { c.Prefix = "UcChaos" }, wantErr: true},
		{name: "prefix ends with dash", mutate: func(c *Config) { c.Prefix = "uc-" }, wantErr: true},
		{name: "prefix starts with digit", mutate: func(c *Config) { c.Prefix = "1uc" }, wantErr: true},
		{name: "prefix too long", mutate: func(c *Config) { c.Prefix = "abcdefghijklmnopqrstu" }, wantErr: true},
		{name: "no voters", mutate: func(c *Config) { c.Voters = 0 }, wantErr: true},
		{name: "six voters", mutate: func(c *Config) { c.Voters = 6 }, wantErr: true},
		{name: "five voters", mutate: func(c *Config) { c.Voters = 5 }},
		{name: "privileged port base", mutate: func(c *Config) { c.HostPortBase = 80 }, wantErr: true},
		{name: "port range overflow", mutate: func(c *Config) { c.HostPortBase = 65534 }, wantErr: true},
		{name: "interval below API minimum", mutate: func(c *Config) { c.Interval = 19 * time.Second }, wantErr: true},
		{name: "interval above API maximum", mutate: func(c *Config) { c.Interval = 25 * time.Hour }, wantErr: true},
		{name: "empty image", mutate: func(c *Config) { c.Image = "" }, wantErr: true},
		{name: "empty netem image", mutate: func(c *Config) { c.NetemImage = "" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DefaultConfig()
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNames(t *testing.T) {
	n := NamesFor("ucchaos")
	checks := map[string]string{
		"network":   n.Network(),
		"voter":     n.Voter(2),
		"sidecar":   n.Sidecar(2),
		"volume":    n.Volume(2),
		"clusterAd": n.ClusterAddress(2),
	}
	want := map[string]string{
		"network":   "ucchaos-net",
		"voter":     "ucchaos-v2",
		"sidecar":   "ucchaos-v2-netem",
		"volume":    "ucchaos-v2-data",
		"clusterAd": "ucchaos-v2:9443",
	}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s = %q, want %q", k, checks[k], v)
		}
	}
}

func TestBaseURLAndHostPort(t *testing.T) {
	c := DefaultConfig()
	c.HostPortBase = 18100
	if got := c.BaseURL(1); got != "http://127.0.0.1:18101" {
		t.Errorf("BaseURL(1) = %q", got)
	}
}

func TestDefaultCallbackHost(t *testing.T) {
	if got := DefaultCallbackHost(RuntimePodman); got != "host.containers.internal" {
		t.Errorf("podman = %q", got)
	}
	if got := DefaultCallbackHost(RuntimeDocker); got != "host.docker.internal" {
		t.Errorf("docker = %q", got)
	}
}

func TestStalePatternsMatchOnlyThisPrefix(t *testing.T) {
	cre, vre, netName := stalePatterns("ucchaos")
	if netName != "ucchaos-net" {
		t.Errorf("network = %q", netName)
	}
	containers := []string{
		"ucchaos-v1", "ucchaos-v1-netem", "ucchaos-v12-netem",
		"ucchaos-v1-other", "ucchaos-b-v1", "ucchaos-chown", "uptime-v1",
	}
	got := matchNames(containers, cre)
	want := []string{"ucchaos-v1", "ucchaos-v1-netem", "ucchaos-v12-netem"}
	if !equalStrings(got, want) {
		t.Errorf("container matches = %v, want %v", got, want)
	}
	vols := matchNames([]string{"ucchaos-v1-data", "ucchaos-v1", "ucchaos-b-v1-data", "ucchaos-v2-data"}, vre)
	if !equalStrings(vols, []string{"ucchaos-v1-data", "ucchaos-v2-data"}) {
		t.Errorf("volume matches = %v", vols)
	}
}

func TestPrefixRegexAcceptsDashes(t *testing.T) {
	if !prefixPattern.MatchString("uc-chaos-2") {
		t.Error("prefix with dashes and digits rejected")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

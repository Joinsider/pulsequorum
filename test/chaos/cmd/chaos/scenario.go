// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joinsider/pulsequorum/test/chaos/harness"
	"github.com/joinsider/pulsequorum/test/internal/fake"
)

// Scenario is one acceptance or chaos scenario. Run writes its steps to
// env.Rep and returns an error only when it cannot continue.
type Scenario struct {
	// ID selects the scenario on the command line, lower case (as02).
	ID string
	// Label is the name used in the report (AS-02). Defaults to upper-case ID.
	Label string
	Title string
	Run   func(ctx context.Context, env *Env) error
}

// reportLabel returns the name of the scenario in the report.
func (s Scenario) reportLabel() string {
	if s.Label != "" {
		return s.Label
	}
	return strings.ToUpper(s.ID)
}

// Env is what a scenario gets from the driver.
type Env struct {
	Cfg        harness.Config
	Runtime    *harness.Runtime
	Log        *slog.Logger
	Ntfy       *fake.Recorder
	Target     *fake.Target
	NtfyPort   int
	TargetPort int
	// RefTarget is a second, always healthy HTTP endpoint for the node
	// self-checks (SELF_CHECK_TARGETS, ADR 0022).
	RefTarget *fake.Target
	RefPort   int
	Rep       *harness.Report
	// FromRef is the git ref the old voter image of AS-09 is built from.
	FromRef string

	clusters []*harness.Cluster
}

// registry holds the scenarios by ID. Scenario files add to it in init.
var registry = map[string]Scenario{}

// register adds a scenario. Duplicate IDs are a programming error.
func register(s Scenario) {
	if s.ID == "" || s.Run == nil {
		panic("chaos: scenario needs an ID and a Run function")
	}
	if _, dup := registry[s.ID]; dup {
		panic("chaos: duplicate scenario " + s.ID)
	}
	registry[s.ID] = s
}

// registeredScenarios returns all scenarios sorted by ID.
func registeredScenarios() []Scenario {
	out := make([]Scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// NewCluster creates a cluster for this scenario. The driver tears it down
// after the scenario, also when the scenario fails.
func (e *Env) NewCluster() (*harness.Cluster, error) {
	cl, err := harness.NewCluster(e.Cfg, e.Runtime, e.Log)
	if err != nil {
		return nil, err
	}
	e.clusters = append(e.clusters, cl)
	return cl, nil
}

// NtfyURL returns the shoutrrr ntfy URL of a topic as containers see it.
func (e *Env) NtfyURL(topic string) string {
	return "ntfy://" + net.JoinHostPort(e.Cfg.CallbackHost, strconv.Itoa(e.NtfyPort)) + "/" + topic + "?scheme=http"
}

// TargetURL returns the URL of a path on the fake HTTP target as containers
// see it.
func (e *Env) TargetURL(path string) string {
	return "http://" + net.JoinHostPort(e.Cfg.CallbackHost, strconv.Itoa(e.TargetPort)) + path
}

// RefURL returns the URL of a path on the reference target as containers see it.
func (e *Env) RefURL(path string) string {
	return "http://" + net.JoinHostPort(e.Cfg.CallbackHost, strconv.Itoa(e.RefPort)) + path
}

// teardown tears down all clusters of the scenario, newest first.
func (e *Env) teardown(ctx context.Context) error {
	var first error
	for i := len(e.clusters) - 1; i >= 0; i-- {
		if err := e.clusters[i].Teardown(ctx); err != nil && first == nil {
			first = err
		}
	}
	e.clusters = nil
	return first
}

// randomSuffix returns 4 random bytes as hex, to keep names of one run apart.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	return hex.EncodeToString(b)
}

// pollUntil calls fn every `every` until it reports done, timeout elapses or
// ctx ends. The last error of fn is returned with a false result.
func pollUntil(ctx context.Context, timeout, every time.Duration, fn func() (bool, error)) (bool, error) {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		done, err := fn()
		if err != nil {
			last = err
		} else if done {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, last
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-t.C:
		}
	}
}

// medianDuration returns the median of ds (0 for an empty input).
func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
	return s[len(s)/2]
}

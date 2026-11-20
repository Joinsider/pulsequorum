// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

// Package checker defines the monitor type plugins (Konzept 11.2, FA-CHK-01).
// Voters and probes run the same plugins. All network access goes through the
// Dialer in Env, so the SSRF policy applies to every check (FA-CHK-04).
package checker

import (
	"context"
	"encoding/json"
	"time"
)

// Status is the outcome of a single check on a single node.
type Status int

// Check outcomes; the values match store.CheckStatus and the protobuf enum.
const (
	StatusUp       Status = 1
	StatusDown     Status = 2
	StatusDegraded Status = 3
)

func (s Status) String() string {
	switch s {
	case StatusUp:
		return "UP"
	case StatusDown:
		return "DOWN"
	case StatusDegraded:
		return "DEGRADED"
	}
	return "UNKNOWN"
}

// ErrorClass tells whether a failure came from the target or from the way to
// it (FA-CHK-03, PF-14).
type ErrorClass string

// Error classes; the values are part of the API and the protobuf contract.
const (
	ClassNone        ErrorClass = ""
	ClassDNS         ErrorClass = "dns"
	ClassConnect     ErrorClass = "connect"
	ClassTLS         ErrorClass = "tls"
	ClassTimeout     ErrorClass = "timeout"
	ClassHTTPStatus  ErrorClass = "http_status"
	ClassAssertion   ErrorClass = "assertion"
	ClassSSRFBlocked ErrorClass = "ssrf_blocked"
	ClassInternal    ErrorClass = "internal"
)

// maxMessageLen limits stored messages (Pflichtenheft 11.2: max. 1 KB).
const maxMessageLen = 1024

// Result is the outcome of one check.
type Result struct {
	Status     Status
	LatencyMs  int
	Message    string
	ErrorClass ErrorClass
	// Details holds type-specific data such as the status code, the resolved
	// IP, phase timings or the TLS expiry.
	Details map[string]any
}

// Down returns a DOWN result for err, classified automatically.
func Down(err error, latencyMs int, details map[string]any) Result {
	return Result{Status: StatusDown, LatencyMs: latencyMs, Message: truncate(err.Error()), ErrorClass: Classify(err), Details: details}
}

func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen-3] + "..."
}

// Env carries the dependencies of a check.
type Env struct {
	// Dialer is the only way a checker may open network connections.
	Dialer *Dialer
	// UserAgent identifies the monitor, e.g. "Pulsequorum/1.2.0" (FA-CHK-13).
	UserAgent string
	// ICMPMode is the ICMP socket mode detected at startup: "privileged",
	// "unprivileged" or "off" (FA-CHK-31).
	ICMPMode string
	// Now is the wall clock of the node, used for certificate validity and
	// expiry decisions; nil means time.Now. Latencies stay monotonic (ADR 0020).
	Now func() time.Time
}

// Checker is a monitor type plugin.
type Checker interface {
	// Type is the stable key, e.g. "http".
	Type() string
	// Name is the display name, e.g. "HTTP(S)".
	Name() string
	// ConfigSchema is the JSON Schema (draft 2020-12) of the configuration; the
	// UI renders its form from it (Konzept 11.2).
	ConfigSchema() json.RawMessage
	// Validate checks semantic rules the schema cannot express. It runs after
	// schema validation.
	Validate(cfg json.RawMessage) error
	// Check runs once. ctx carries the monitor timeout; when it ends, the check
	// must return and leave no goroutines behind (FA-CHK-02).
	Check(ctx context.Context, env *Env, cfg json.RawMessage) Result
}

// CapabilityRequirer is implemented by checkers that need a node capability,
// e.g. ICMP (FA-CHK-31).
type CapabilityRequirer interface {
	RequiredCapability() string
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/probepolicy"
)

// TargetPolicyCapability is announced when PROBE_ALLOW_TARGETS restricts the
// targets; the cluster only displays it (ADR 0044, section 5).
const TargetPolicyCapability = "target-policy"

// Reasons of a local policy block (ADR 0044, section 4); they are metric
// label values.
const (
	BlockedTarget = "target"
	BlockedMethod = "method"
	BlockedType   = "type"
)

// GuardOptions is the local check policy of a probe (ADR 0044).
type GuardOptions struct {
	// Types are the allowed monitor types; nil allows every registered type.
	Types []string
	// Methods are the allowed HTTP methods; nil allows every method.
	Methods *probepolicy.Methods
	// Detail is probepolicy.DetailFull or probepolicy.DetailMinimal.
	Detail string
	// HideTargets replaces the message of results blocked by the address
	// policy, so neither address nor port of an internal target leaves a
	// private probe.
	HideTargets bool
	// OnBlocked is called with BlockedTarget, BlockedMethod or BlockedType
	// for every blocked check; nil disables it.
	OnBlocked func(reason string)
}

// GuardRegistry returns a registry with every checker of base wrapped in the
// local policy. Types and methods are checked before a check runs, so a
// blocked check opens no connection; results are reduced before they reach
// the local buffer, so what is dropped is never stored or sent.
func GuardRegistry(base *checker.Registry, o GuardOptions) (*checker.Registry, error) {
	if o.Detail != probepolicy.DetailFull && o.Detail != probepolicy.DetailMinimal {
		return nil, fmt.Errorf("result detail must be %q or %q, got %q", probepolicy.DetailFull, probepolicy.DetailMinimal, o.Detail)
	}
	for _, t := range o.Types {
		if _, err := base.Lookup(t); err != nil {
			return nil, fmt.Errorf("PROBE_ALLOW_MONITOR_TYPES: %w", err)
		}
	}
	out := checker.NewRegistry()
	for _, c := range base.Checkers() {
		allowed := o.Types == nil || slices.Contains(o.Types, c.Type())
		out.Register(&guarded{Checker: c, allowed: allowed, opts: o})
	}
	return out, nil
}

type guarded struct {
	checker.Checker
	allowed bool
	opts    GuardOptions
}

// RequiredCapability passes the capability of the wrapped checker through
// (FA-CHK-31); "" means none, as for checkers without one.
func (g *guarded) RequiredCapability() string {
	if req, ok := g.Checker.(checker.CapabilityRequirer); ok {
		return req.RequiredCapability()
	}
	return ""
}

func (g *guarded) Check(ctx context.Context, env *checker.Env, cfg json.RawMessage) checker.Result {
	if !g.allowed {
		return g.blocked(BlockedType)
	}
	if g.opts.Methods != nil && g.Type() == "http" && !g.opts.Methods.Allows(httpMethod(cfg)) {
		return g.blocked(BlockedMethod)
	}
	res := g.Checker.Check(ctx, env, cfg)
	if res.ErrorClass == checker.ClassSSRFBlocked {
		if g.opts.OnBlocked != nil {
			g.opts.OnBlocked(BlockedTarget)
		}
		if g.opts.HideTargets {
			res.Message = blockedMessage(BlockedTarget)
			res.Details = nil
		}
	}
	if g.opts.Detail == probepolicy.DetailMinimal {
		res = minimal(res)
	}
	return res
}

func (g *guarded) blocked(reason string) checker.Result {
	if g.opts.OnBlocked != nil {
		g.opts.OnBlocked(reason)
	}
	return checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassSSRFBlocked, Message: blockedMessage(reason)}
}

const blockedPrefix = "blocked by local probe policy: "

func blockedMessage(reason string) string {
	return blockedPrefix + reason
}

// httpMethod returns the method of an HTTP monitor configuration; the
// checker defaults an empty method to GET. An unreadable configuration
// yields "", which no method list allows, so the check is blocked rather
// than run with an unknown method.
func httpMethod(cfg json.RawMessage) string {
	var c struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return ""
	}
	if c.Method == "" {
		return "GET"
	}
	return c.Method
}

// minimalDetails are the detail keys a probe reports with
// PROBE_RESULT_DETAIL=minimal: status codes, expiry and timings, but no
// content, answers, addresses or names (ADR 0044, section 3).
var minimalDetails = []string{
	"statusCode", "rcode", "tlsExpiresAt", "timings",
	"packetsSent", "packetsReceived", "packetLossPercent", "rttMinMs", "rttAvgMs", "rttMaxMs",
}

func minimal(res checker.Result) checker.Result {
	var details map[string]any
	for _, k := range minimalDetails {
		if v, ok := res.Details[k]; ok {
			if details == nil {
				details = map[string]any{}
			}
			details[k] = v
		}
	}
	res.Details = details
	res.Message = minimalMessage(res)
	return res
}

func minimalMessage(res checker.Result) string {
	switch res.ErrorClass {
	case checker.ClassNone:
		if res.Status == checker.StatusUp {
			return "ok"
		}
		return strings.ToLower(res.Status.String())
	case checker.ClassSSRFBlocked:
		if strings.HasPrefix(res.Message, blockedPrefix) {
			return res.Message
		}
		return blockedMessage(BlockedTarget)
	case checker.ClassHTTPStatus:
		if code, ok := res.Details["statusCode"].(int); ok {
			return "HTTP " + strconv.Itoa(code)
		}
		return "unexpected HTTP status"
	case checker.ClassAssertion:
		return "assertion failed"
	case checker.ClassTimeout:
		return "timeout"
	case checker.ClassInternal:
		return "internal error"
	default:
		return string(res.ErrorClass) + " failed"
	}
}

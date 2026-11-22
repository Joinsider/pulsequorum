// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/probepolicy"
)

// fakeChecker is a monitor type that records calls and returns a fixed result.
type fakeChecker struct {
	typ        string
	capability string
	result     checker.Result
	calls      int
}

func (f *fakeChecker) Type() string                   { return f.typ }
func (f *fakeChecker) Name() string                   { return "fake " + f.typ }
func (f *fakeChecker) ConfigSchema() json.RawMessage  { return json.RawMessage(`{"type":"object"}`) }
func (f *fakeChecker) Validate(json.RawMessage) error { return nil }

func (f *fakeChecker) Check(context.Context, *checker.Env, json.RawMessage) checker.Result {
	f.calls++
	return f.result
}

// capabilityFake additionally requires a node capability.
type capabilityFake struct {
	fakeChecker
}

func (c *capabilityFake) RequiredCapability() string { return c.capability }

func upResult() checker.Result {
	return checker.Result{Status: checker.StatusUp, LatencyMs: 12, Message: "200 OK"}
}

// newBase returns a registry with a fake http and a fake tcp checker.
func newBase(httpRes, tcpRes checker.Result) (*checker.Registry, *fakeChecker, *fakeChecker) {
	h := &fakeChecker{typ: "http", result: httpRes}
	tcp := &fakeChecker{typ: "tcp", result: tcpRes}
	base := checker.NewRegistry()
	base.Register(h)
	base.Register(tcp)
	return base, h, tcp
}

func guardedCheck(t *testing.T, o GuardOptions, base *checker.Registry, typ string, cfg string) checker.Result {
	t.Helper()
	reg, err := GuardRegistry(base, o)
	if err != nil {
		t.Fatalf("GuardRegistry: %v", err)
	}
	c, err := reg.Lookup(typ)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", typ, err)
	}
	return c.Check(context.Background(), &checker.Env{}, json.RawMessage(cfg))
}

func TestGuardBlocksDisallowedType(t *testing.T) {
	base, h, tcp := newBase(upResult(), upResult())
	var reasons []string
	o := GuardOptions{
		Types:     []string{"tcp"},
		Detail:    probepolicy.DetailFull,
		OnBlocked: func(r string) { reasons = append(reasons, r) },
	}
	res := guardedCheck(t, o, base, "http", `{"url":"https://example.com"}`)
	if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassSSRFBlocked {
		t.Errorf("status/class = %v/%q, want DOWN/ssrf_blocked", res.Status, res.ErrorClass)
	}
	if res.Message != "blocked by local probe policy: type" {
		t.Errorf("message = %q", res.Message)
	}
	if h.calls != 0 {
		t.Error("inner http Check was called for a disallowed type")
	}
	if len(reasons) != 1 || reasons[0] != BlockedType {
		t.Errorf("OnBlocked = %v, want [type]", reasons)
	}

	res = guardedCheck(t, o, base, "tcp", `{}`)
	if res.Status != checker.StatusUp || tcp.calls != 1 {
		t.Errorf("allowed type: status %v, inner calls %d", res.Status, tcp.calls)
	}
}

func TestGuardHTTPMethods(t *testing.T) {
	methods, err := probepolicy.ParseMethods("GET,HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		cfg         string
		wantBlocked bool
	}{
		{"POST blocked", `{"url":"https://example.com","method":"POST"}`, true},
		{"GET allowed", `{"url":"https://example.com","method":"GET"}`, false},
		{"lower case HEAD allowed", `{"url":"https://example.com","method":"head"}`, false},
		{"empty method counts as GET", `{"url":"https://example.com"}`, false},
		{"unreadable config blocked", `{not json`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, h, _ := newBase(upResult(), upResult())
			var reasons []string
			o := GuardOptions{
				Methods:   methods,
				Detail:    probepolicy.DetailFull,
				OnBlocked: func(r string) { reasons = append(reasons, r) },
			}
			res := guardedCheck(t, o, base, "http", tt.cfg)
			if !tt.wantBlocked {
				if h.calls != 1 || res.Status != checker.StatusUp {
					t.Errorf("inner calls %d, status %v; want the check to run", h.calls, res.Status)
				}
				return
			}
			if h.calls != 0 {
				t.Error("inner http Check was called for a blocked method")
			}
			if res.ErrorClass != checker.ClassSSRFBlocked || res.Message != "blocked by local probe policy: method" {
				t.Errorf("result = %q/%q", res.ErrorClass, res.Message)
			}
			if len(reasons) != 1 || reasons[0] != BlockedMethod {
				t.Errorf("OnBlocked = %v, want [method]", reasons)
			}
		})
	}
}

func TestGuardNilMethodsAllowsPost(t *testing.T) {
	base, h, _ := newBase(upResult(), upResult())
	res := guardedCheck(t, GuardOptions{Detail: probepolicy.DetailFull}, base, "http", `{"url":"https://example.com","method":"POST"}`)
	if h.calls != 1 || res.Status != checker.StatusUp {
		t.Errorf("POST with nil Methods: inner calls %d, status %v", h.calls, res.Status)
	}
}

func TestGuardHideTargets(t *testing.T) {
	blockedRes := checker.Result{
		Status:     checker.StatusDown,
		ErrorClass: checker.ClassSSRFBlocked,
		Message:    "connection to 10.1.1.1:80 blocked by SSRF protection (private)",
		Details:    map[string]any{"resolvedIp": "10.1.1.1"},
	}
	var reasons []string
	base, _, _ := newBase(blockedRes, blockedRes)
	o := GuardOptions{
		Detail:      probepolicy.DetailFull,
		HideTargets: true,
		OnBlocked:   func(r string) { reasons = append(reasons, r) },
	}
	res := guardedCheck(t, o, base, "http", `{"url":"https://10.1.1.1"}`)
	if res.Message != "blocked by local probe policy: target" {
		t.Errorf("message = %q", res.Message)
	}
	if res.Details != nil {
		t.Errorf("details = %v, want nil", res.Details)
	}
	if len(reasons) != 1 || reasons[0] != BlockedTarget {
		t.Errorf("OnBlocked = %v, want [target]", reasons)
	}

	// Without HideTargets the original message stays.
	o.HideTargets = false
	res = guardedCheck(t, o, base, "http", `{"url":"https://10.1.1.1"}`)
	if res.Message != blockedRes.Message {
		t.Errorf("message without HideTargets = %q, want original", res.Message)
	}
}

func TestGuardMinimalDropsDetails(t *testing.T) {
	full := checker.Result{
		Status:     checker.StatusUp,
		LatencyMs:  30,
		Message:    "200 OK",
		ErrorClass: checker.ClassNone,
		Details: map[string]any{
			"statusCode":        200,
			"tlsExpiresAt":      "2027-01-01T00:00:00Z",
			"timings":           map[string]any{"dnsMs": 1},
			"answers":           []string{"10.0.0.1"},
			"resolvedIp":        "10.0.0.1",
			"resolver":          "10.0.0.53",
			"redirects":         []string{"https://a.example/"},
			"tlsVersion":        "TLS1.3",
			"rttAvgMs":          1.5,
			"packetLossPercent": 0,
		},
	}
	base, _, _ := newBase(full, full)
	res := guardedCheck(t, GuardOptions{Detail: probepolicy.DetailMinimal}, base, "http", `{"url":"https://example.com"}`)

	for _, k := range []string{"statusCode", "tlsExpiresAt", "timings", "rttAvgMs", "packetLossPercent"} {
		if _, ok := res.Details[k]; !ok {
			t.Errorf("minimal dropped %q", k)
		}
	}
	for _, k := range []string{"answers", "resolvedIp", "resolver", "redirects", "tlsVersion"} {
		if _, ok := res.Details[k]; ok {
			t.Errorf("minimal kept %q", k)
		}
	}
	if res.Message != "ok" {
		t.Errorf("message = %q, want ok", res.Message)
	}
}

func TestGuardMinimalMessages(t *testing.T) {
	tests := []struct {
		name string
		res  checker.Result
		want string
	}{
		{"up", checker.Result{Status: checker.StatusUp, Message: "200 OK"}, "ok"},
		{"http status", checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassHTTPStatus, Message: "unexpected status", Details: map[string]any{"statusCode": 503}}, "HTTP 503"},
		{"http status without code", checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassHTTPStatus, Message: "unexpected status"}, "unexpected HTTP status"},
		{"assertion", checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassAssertion, Message: "body mismatch"}, "assertion failed"},
		{"dns", checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassDNS, Message: "no such host"}, "dns failed"},
		{"timeout", checker.Result{Status: checker.StatusDown, ErrorClass: checker.ClassTimeout, Message: "context deadline exceeded"}, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, _, _ := newBase(tt.res, tt.res)
			res := guardedCheck(t, GuardOptions{Detail: probepolicy.DetailMinimal}, base, "http", `{"url":"https://example.com"}`)
			if res.Message != tt.want {
				t.Errorf("message = %q, want %q", res.Message, tt.want)
			}
		})
	}
}

func TestGuardRegistryErrors(t *testing.T) {
	base, _, _ := newBase(upResult(), upResult())
	if _, err := GuardRegistry(base, GuardOptions{Types: []string{"nope"}, Detail: probepolicy.DetailFull}); err == nil {
		t.Error("unknown type in Types accepted")
	} else if !strings.Contains(err.Error(), "PROBE_ALLOW_MONITOR_TYPES") {
		t.Errorf("error %q does not name PROBE_ALLOW_MONITOR_TYPES", err)
	}
	if _, err := GuardRegistry(base, GuardOptions{Detail: "verbose"}); err == nil {
		t.Error("invalid detail accepted")
	}
	if _, err := GuardRegistry(base, GuardOptions{Detail: probepolicy.DetailMinimal}); err != nil {
		t.Errorf("minimal detail rejected: %v", err)
	}
}

func TestGuardRequiredCapability(t *testing.T) {
	base := checker.NewRegistry()
	base.Register(&capabilityFake{fakeChecker: fakeChecker{typ: "icmp", capability: "icmp"}})
	base.Register(&fakeChecker{typ: "tcp"})
	reg, err := GuardRegistry(base, GuardOptions{Detail: probepolicy.DetailFull})
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[string]string{"icmp": "icmp", "tcp": ""} {
		c, err := reg.Lookup(typ)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if req, ok := c.(checker.CapabilityRequirer); ok {
			got = req.RequiredCapability()
		}
		if got != want {
			t.Errorf("RequiredCapability(%s) = %q, want %q", typ, got, want)
		}
	}
}

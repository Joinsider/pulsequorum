// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors

package dnscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/goleak"

	"github.com/joinsider/uptimecluster/internal/checker"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var zone = map[string][]string{
	"a.test.":     {"a.test. 60 IN A 192.0.2.1", "a.test. 60 IN A 192.0.2.2"},
	"v6.test.":    {"v6.test. 60 IN AAAA 2001:db8:0:0::1"},
	"mx.test.":    {"mx.test. 60 IN MX 10 Mail.Test."},
	"txt.test.":   {`txt.test. 60 IN TXT "v=spf1 -all"`},
	"alias.test.": {"alias.test. 60 IN CNAME a.test.", "a.test. 60 IN A 192.0.2.1"},
	"srv.test.":   {"srv.test. 60 IN SRV 10 5 5060 sip.test."},
	"caa.test.":   {`caa.test. 60 IN CAA 0 issue "letsencrypt.org"`},
	// Internal names that must not leak through the system resolver.
	"internal.test.": {"internal.test. 60 IN A 10.1.2.3", "internal.test. 60 IN A 192.0.2.1"},
	"meta.test.":     {"meta.test. 60 IN CNAME md.test.", "md.test. 60 IN A 169.254.169.254"},
	"ula.test.":      {"ula.test. 60 IN AAAA fd00::1"},
	"mapped.test.":   {"mapped.test. 60 IN AAAA ::ffff:127.0.0.1"},
}

// startServer runs a UDP and TCP DNS server on the same port. Answers for
// big.test do not fit into UDP and are sent truncated there.
func startServer(t *testing.T) string {
	t.Helper()
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		q := req.Question[0]
		switch {
		case q.Name == "big.test.":
			if _, udp := w.RemoteAddr().(*net.UDPAddr); udp {
				m.Truncated = true
				break
			}
			for i := range 40 {
				rr, _ := dns.NewRR("big.test. 60 IN TXT \"" + strings.Repeat("x", 100) + string(rune('a'+i%26)) + "\"")
				m.Answer = append(m.Answer, rr)
			}
		case zone[q.Name] == nil:
			m.Rcode = dns.RcodeNameError
		default:
			for _, s := range zone[q.Name] {
				rr, _ := dns.NewRR(s)
				if rr.Header().Rrtype == q.Qtype || rr.Header().Rrtype == dns.TypeCNAME {
					m.Answer = append(m.Answer, rr)
				}
			}
		}
		_ = w.WriteMsg(m)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	udp := &dns.Server{PacketConn: pc, Handler: handler}
	tcp := &dns.Server{Listener: ln, Handler: handler}
	go func() { _ = udp.ActivateAndServe() }()
	go func() { _ = tcp.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
	})
	return pc.LocalAddr().String()
}

func check(t *testing.T, c *Checker, env *checker.Env, cfg map[string]any) checker.Result {
	t.Helper()
	raw, _ := json.Marshal(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Check(ctx, env, raw)
}

func TestDNS(t *testing.T) {
	server := startServer(t)
	env := &checker.Env{Dialer: &checker.Dialer{}}
	c := &Checker{}
	tests := []struct {
		name  string
		cfg   map[string]any
		want  checker.Status
		class checker.ErrorClass
	}{
		{"any A", map[string]any{"hostname": "a.test"}, checker.StatusUp, ""},
		{"equals in other order", map[string]any{"hostname": "a.test", "expected": []string{"192.0.2.2", "192.0.2.1"}, "matchMode": "equals"}, checker.StatusUp, ""},
		{"equals with extra record", map[string]any{"hostname": "a.test", "expected": []string{"192.0.2.1"}, "matchMode": "equals"}, checker.StatusDown, checker.ClassAssertion},
		{"contains subset", map[string]any{"hostname": "a.test", "expected": []string{"192.0.2.2"}}, checker.StatusUp, ""},
		{"missing value", map[string]any{"hostname": "a.test", "expected": []string{"192.0.2.9"}}, checker.StatusDown, checker.ClassAssertion},
		{"AAAA normalized", map[string]any{"hostname": "v6.test", "recordType": "AAAA", "expected": []string{"2001:DB8::1"}}, checker.StatusUp, ""},
		{"MX case and dot", map[string]any{"hostname": "mx.test", "recordType": "MX", "expected": []string{"10 mail.test"}}, checker.StatusUp, ""},
		{"TXT exact", map[string]any{"hostname": "txt.test", "recordType": "TXT", "expected": []string{"v=spf1 -all"}}, checker.StatusUp, ""},
		{"A via CNAME", map[string]any{"hostname": "alias.test", "expected": []string{"192.0.2.1"}, "matchMode": "equals"}, checker.StatusUp, ""},
		{"CNAME", map[string]any{"hostname": "alias.test", "recordType": "CNAME", "expected": []string{"a.test."}}, checker.StatusUp, ""},
		{"SRV", map[string]any{"hostname": "srv.test", "recordType": "SRV", "expected": []string{"10 5 5060 sip.test"}}, checker.StatusUp, ""},
		{"CAA", map[string]any{"hostname": "caa.test", "recordType": "CAA", "expected": []string{"0 issue letsencrypt.org"}}, checker.StatusUp, ""},
		{"no records of type", map[string]any{"hostname": "a.test", "recordType": "MX"}, checker.StatusDown, checker.ClassDNS},
		{"NXDOMAIN", map[string]any{"hostname": "missing.test"}, checker.StatusDown, checker.ClassDNS},
		{"TCP transport", map[string]any{"hostname": "a.test", "transport": "tcp"}, checker.StatusUp, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg["resolver"] = server
			res := check(t, c, env, tt.cfg)
			if res.Status != tt.want || res.ErrorClass != tt.class {
				t.Fatalf("result = %s/%q (%s), want %s/%q", res.Status, res.ErrorClass, res.Message, tt.want, tt.class)
			}
		})
	}
}

func TestTruncatedAnswerRetriesOverTCP(t *testing.T) {
	server := startServer(t)
	res := check(t, &Checker{}, &checker.Env{Dialer: &checker.Dialer{}},
		map[string]any{"hostname": "big.test", "recordType": "TXT", "resolver": server})
	if res.Status != checker.StatusUp || res.Details["truncatedRetry"] != true || len(res.Details["answers"].([]string)) != 26 || len(res.Message) > 1024 {
		t.Fatalf("result = %s, %d answers, retry %v", res.Status, len(res.Details["answers"].([]string)), res.Details["truncatedRetry"])
	}
}

func TestResolverSSRF(t *testing.T) {
	server := startServer(t)
	guarded := &checker.Env{Dialer: &checker.Dialer{Policy: checker.BlockPrivate{}}}

	// A tenant-supplied private resolver is blocked ...
	res := check(t, &Checker{}, guarded, map[string]any{"hostname": "a.test", "resolver": server})
	if res.ErrorClass != checker.ClassSSRFBlocked {
		t.Errorf("tenant resolver = %s/%s, want ssrf_blocked", res.Status, res.ErrorClass)
	}
	// ... the operator's system resolver is not.
	res = check(t, &Checker{SystemResolvers: []string{server}}, guarded, map[string]any{"hostname": "a.test"})
	if res.Status != checker.StatusUp {
		t.Errorf("system resolver = %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}
}

// TestBlockedAnswers checks that answers in blocked ranges fail the check
// without showing the addresses, also through the operator's resolver
// (ADR 0030).
func TestBlockedAnswers(t *testing.T) {
	server := startServer(t)
	sys := &Checker{SystemResolvers: []string{server}}
	guarded := &checker.Env{Dialer: &checker.Dialer{Policy: checker.BlockPrivate{}}}
	open := &checker.Env{Dialer: &checker.Dialer{}}

	for _, tt := range []struct{ host, typ, reason string }{
		{"internal.test", "A", "private"},
		{"meta.test", "A", "link-local / cloud metadata"},
		{"ula.test", "AAAA", "unique local"},
		{"mapped.test", "AAAA", "loopback"},
	} {
		cfg := map[string]any{"hostname": tt.host, "recordType": tt.typ}
		res := check(t, sys, guarded, cfg)
		if res.Status != checker.StatusDown || res.ErrorClass != checker.ClassSSRFBlocked || res.Message != "answer in a blocked range: "+tt.reason {
			t.Errorf("%s: result = %s/%s %q", tt.host, res.Status, res.ErrorClass, res.Message)
		}
		if _, ok := res.Details["answers"]; ok {
			t.Errorf("%s: details show answers: %v", tt.host, res.Details)
		}
		for _, leak := range []string{"10.1.2.3", "169.254", "fd00", "127.0.0.1", "192.0.2.1"} {
			if strings.Contains(res.Message, leak) || strings.Contains(fmt.Sprint(res.Details), leak) {
				t.Errorf("%s: result leaks %s: %q %v", tt.host, leak, res.Message, res.Details)
			}
		}
		if res.Details["resolver"] != "system" {
			t.Errorf("%s: resolver detail = %v, want system", tt.host, res.Details["resolver"])
		}
		// Without protection the answers are shown as before.
		if res := check(t, sys, open, cfg); res.Status != checker.StatusUp {
			t.Errorf("%s without protection: %s/%s %s", tt.host, res.Status, res.ErrorClass, res.Message)
		}
	}
	// A failing system resolver does not reveal its address either.
	dead := listenDead(t)
	res := check(t, &Checker{SystemResolvers: []string{dead}}, guarded, map[string]any{"hostname": "a.test", "transport": "tcp"})
	if res.Status != checker.StatusDown || strings.Contains(res.Message, "127.0.0.1") || res.Details["resolver"] != "system" {
		t.Errorf("dead system resolver: %s/%s %q %v", res.Status, res.ErrorClass, res.Message, res.Details)
	}

	// Public answers still pass with protection on.
	if res := check(t, sys, guarded, map[string]any{"hostname": "a.test"}); res.Status != checker.StatusUp {
		t.Errorf("public answer: %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}
	// Record types without addresses are not affected.
	if res := check(t, sys, guarded, map[string]any{"hostname": "meta.test", "recordType": "CNAME"}); res.Status != checker.StatusUp {
		t.Errorf("CNAME: %s/%s %s", res.Status, res.ErrorClass, res.Message)
	}
}

// listenDead returns a TCP address that refuses connections.
func listenDead(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestValidation(t *testing.T) {
	reg := checker.NewRegistry()
	reg.Register(&Checker{})
	for _, cfg := range []string{
		`{"hostname":"example.com"}`,
		`{"hostname":"_sip._tcp.example.com","recordType":"SRV","resolver":"1.1.1.1"}`,
		`{"hostname":"example.com.","resolver":"[2606:4700::1111]:53","expected":["1.2.3.4"],"matchMode":"equals"}`,
	} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{
		`{}`,
		`{"hostname":"bad host"}`,
		`{"hostname":"example.com","resolver":"dns.google"}`,
		`{"hostname":"example.com","resolver":"0x08080808"}`,
		`{"hostname":"example.com","matchMode":"equals"}`,
		`{"hostname":"example.com","recordType":"PTR"}`,
	} {
		if err := reg.Validate(Type, json.RawMessage(cfg)); err == nil {
			t.Errorf("%s accepted", cfg)
		}
	}
}

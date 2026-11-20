// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package main

import (
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/miekg/dns"
)

// zone is the only zone the test DNS server answers for.
const zone = "ssrf.test."

// The addresses of the test topology (see deploy/ssrf-dev/README.md).
var (
	publicIP   = netip.MustParseAddr("203.0.113.10") // "public" network, not blocked by the SSRF filter
	internalIP = netip.MustParseAddr("10.99.0.10")   // "internal" network, blocked by the SSRF filter
)

// staticRecords holds the fixed names of the zone, keyed by lower-case FQDN.
// Names in the zone that are not listed here and not rebind.ssrf.test. answer NXDOMAIN.
var staticRecords = map[string]struct {
	a    []netip.Addr
	aaaa []netip.Addr
}{
	"public.ssrf.test.":   {a: []netip.Addr{publicIP}},
	"internal.ssrf.test.": {a: []netip.Addr{internalIP}},
	"loop.ssrf.test.":     {a: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	"meta.ssrf.test.":     {a: []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
	"v6loop.ssrf.test.":   {aaaa: []netip.Addr{netip.MustParseAddr("::1")}},
}

const (
	rebindName = "rebind.ssrf.test."
	staticTTL  = 60
)

// dnsHandler answers queries authoritatively for zone.
type dnsHandler struct {
	log *slog.Logger
	// rebindA counts A queries for rebindName; odd queries get the public
	// address, even queries the internal one.
	rebindA atomic.Uint64
}

// ServeDNS implements dns.Handler.
func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(req)
	resp.Authoritative = true

	if len(req.Question) != 1 {
		resp.Rcode = dns.RcodeFormatError
		resp.Authoritative = false
		h.logQuery(dns.Question{Name: "-", Qtype: 0}, resp.Rcode, nil)
		_ = w.WriteMsg(resp)
		return
	}
	q := req.Question[0]
	rcode, rrs := h.resolve(q)
	resp.Rcode = rcode
	if rcode == dns.RcodeRefused {
		resp.Authoritative = false
	}
	resp.Answer = rrs
	h.logQuery(q, rcode, rrs)
	_ = w.WriteMsg(resp)
}

// resolve returns the response code and answers for one question.
func (h *dnsHandler) resolve(q dns.Question) (int, []dns.RR) {
	name := strings.ToLower(q.Name)
	if !dns.IsSubDomain(zone, name) {
		return dns.RcodeRefused, nil
	}

	if name == rebindName {
		if q.Qtype != dns.TypeA {
			return dns.RcodeSuccess, nil // NODATA for every other type
		}
		ip := publicIP
		if h.rebindA.Add(1)%2 == 0 {
			ip = internalIP
		}
		return dns.RcodeSuccess, []dns.RR{aRR(q.Name, ip, 0)}
	}

	rec, ok := staticRecords[name]
	if !ok {
		return dns.RcodeNameError, nil
	}
	var rrs []dns.RR
	switch q.Qtype {
	case dns.TypeA:
		for _, ip := range rec.a {
			rrs = append(rrs, aRR(q.Name, ip, staticTTL))
		}
	case dns.TypeAAAA:
		for _, ip := range rec.aaaa {
			rrs = append(rrs, aaaaRR(q.Name, ip, staticTTL))
		}
	}
	return dns.RcodeSuccess, rrs
}

func (h *dnsHandler) logQuery(q dns.Question, rcode int, rrs []dns.RR) {
	answers := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		switch v := rr.(type) {
		case *dns.A:
			answers = append(answers, v.A.String())
		case *dns.AAAA:
			answers = append(answers, v.AAAA.String())
		}
	}
	answer := "-"
	if len(answers) > 0 {
		answer = strings.Join(answers, ",")
	}
	h.log.Info("dns query",
		"name", q.Name,
		"type", dns.TypeToString[q.Qtype],
		"rcode", dns.RcodeToString[rcode],
		"answer", answer,
	)
}

func aRR(name string, ip netip.Addr, ttl uint32) dns.RR {
	return &dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   ip.AsSlice(),
	}
}

func aaaaRR(name string, ip netip.Addr, ttl uint32) dns.RR {
	return &dns.AAAA{
		Hdr:  dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl},
		AAAA: ip.AsSlice(),
	}
}

// serveDNS serves zone on UDP and TCP port 53 until one listener fails.
func serveDNS(log *slog.Logger) error {
	h := &dnsHandler{log: log}
	errc := make(chan error, 2)
	for _, network := range []string{"udp", "tcp"} {
		srv := &dns.Server{Addr: ":53", Net: network, Handler: h}
		log.Info("dns listening", "network", network, "addr", srv.Addr, "zone", zone)
		go func() { errc <- srv.ListenAndServe() }()
	}
	return <-errc
}

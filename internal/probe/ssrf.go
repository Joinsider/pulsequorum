// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"net/netip"
	"sync/atomic"

	"github.com/joinsider/pulsequorum/internal/checker"
)

// Policy enforces SSRF protection for probe checks (ADR 0035 section 4).
// Private probes use only their local policy (remote ProbeMonitor.ssrf_protection
// is ignored). Public probes enforce the union of local and remote policies so
// neither side can weaken protection.
type Policy struct {
	localSSRF    bool
	privateProbe bool
	remoteSSRF   atomic.Bool
	block        checker.BlockPrivate
}

// NewPolicy builds an address policy for a probe.
func NewPolicy(localSSRF, privateProbe bool) *Policy {
	return &Policy{
		localSSRF:    localSSRF,
		privateProbe: privateProbe,
	}
}

// SetRemote updates whether any assigned monitor requires SSRF protection.
// Private probes ignore this flag.
func (p *Policy) SetRemote(enabled bool) {
	p.remoteSSRF.Store(enabled)
}

// Check implements checker.AddressPolicy.
func (p *Policy) Check(ip netip.Addr) error {
	if p.privateProbe {
		if p.localSSRF {
			return p.block.Check(ip)
		}
		return nil
	}
	if p.localSSRF || p.remoteSSRF.Load() {
		return p.block.Check(ip)
	}
	return nil
}

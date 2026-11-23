// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package probe

import (
	"net/netip"
	"sync/atomic"

	"github.com/joinsider/pulsequorum/internal/checker"
	"github.com/joinsider/pulsequorum/internal/probepolicy"
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
	// targets is the local target policy (ADR 0044); nil means unset.
	targets *probepolicy.Targets
}

// reasonTargetPolicy is the BlockedError reason of the local target policy.
const reasonTargetPolicy = "local probe target policy"

// NewPolicy builds an address policy for a probe.
func NewPolicy(localSSRF, privateProbe bool) *Policy {
	return &Policy{
		localSSRF:    localSSRF,
		privateProbe: privateProbe,
	}
}

// WithTargets adds the local target policy (PROBE_ALLOW_TARGETS, ADR 0044).
// It only narrows the policy: a connection must pass both. Call it before
// the policy is used.
func (p *Policy) WithTargets(t *probepolicy.Targets) *Policy {
	p.targets = t
	return p
}

// SetRemote updates whether any assigned monitor requires SSRF protection.
// Private probes ignore this flag.
func (p *Policy) SetRemote(enabled bool) {
	p.remoteSSRF.Store(enabled)
}

// Check implements checker.AddressPolicy. Without a port it serves ICMP and
// DNS answers: the target policy then accepts an address on any port.
func (p *Policy) Check(ip netip.Addr) error {
	if err := p.checkSSRF(ip); err != nil {
		return err
	}
	if p.targets != nil && !p.targets.AllowIP(ip) {
		return &checker.BlockedError{Addr: ip, Reason: reasonTargetPolicy}
	}
	return nil
}

// CheckAddrPort implements checker.AddrPortPolicy for TCP and UDP connections.
func (p *Policy) CheckAddrPort(ap netip.AddrPort) error {
	if err := p.checkSSRF(ap.Addr()); err != nil {
		return err
	}
	if p.targets != nil && !p.targets.AllowAddrPort(ap) {
		return &checker.BlockedError{Addr: ap.Addr(), Reason: reasonTargetPolicy}
	}
	return nil
}

func (p *Policy) checkSSRF(ip netip.Addr) error {
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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// netIface is the interface of the voter's network namespace that netem
// shapes. Podman and docker both name the container interface eth0.
const netIface = "eth0"

// Direction selects which traffic a one-way partition drops.
type Direction int

const (
	// Inbound drops traffic from the other voters into voter i.
	Inbound Direction = iota
	// Outbound drops traffic from voter i to the other voters.
	Outbound
	// Both drops traffic in both directions (Isolate).
	Both
)

func (d Direction) String() string {
	switch d {
	case Inbound:
		return "inbound"
	case Outbound:
		return "outbound"
	default:
		return "both"
	}
}

// LatencySpec describes a netem shaping of egress traffic.
type LatencySpec struct {
	Delay       time.Duration
	Jitter      time.Duration
	LossPercent float64
}

// Isolate drops all traffic between voter i and the other running voters,
// in both directions. Traffic from the host (the published HTTP port) is not
// affected, because it does not come from another voter.
func (c *Cluster) Isolate(ctx context.Context, i int) error {
	return c.partition(ctx, i, Both)
}

// IsolateOneWay drops only the given direction between voter i and the other
// running voters.
func (c *Cluster) IsolateOneWay(ctx context.Context, i int, dir Direction) error {
	return c.partition(ctx, i, dir)
}

// Heal removes all partition rules of voter i.
func (c *Cluster) Heal(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return err
	}
	for _, args := range flushCommands() {
		if _, err := c.rt.Exec(ctx, c.names.Sidecar(i), args...); err != nil {
			return fmt.Errorf("heal %s: %w", c.names.Voter(i), err)
		}
	}
	return nil
}

// partition installs the DROP rules for voter i. Existing rules are flushed
// first, so calling it twice does not stack rules.
func (c *Cluster) partition(ctx context.Context, i int, dir Direction) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return err
	}
	peers, err := c.peerIPs(ctx, i)
	if err != nil {
		return err
	}
	sidecar := c.names.Sidecar(i)
	for _, args := range flushCommands() {
		if _, err := c.rt.Exec(ctx, sidecar, args...); err != nil {
			return fmt.Errorf("partition %s: %w", c.names.Voter(i), err)
		}
	}
	for _, args := range dropCommands(dir, peers) {
		if _, err := c.rt.Exec(ctx, sidecar, args...); err != nil {
			return fmt.Errorf("partition %s (%s): %w", c.names.Voter(i), dir, err)
		}
	}
	c.log.Info("partition installed", "node", c.names.Voter(i), "direction", dir.String(), "peers", len(peers))
	return nil
}

// peerIPs returns the container-network addresses of the other running voters.
func (c *Cluster) peerIPs(ctx context.Context, i int) ([]string, error) {
	var ips []string
	for j := 1; j <= c.cfg.Voters; j++ {
		if j == i {
			continue
		}
		running, err := c.rt.Running(ctx, c.names.Voter(j))
		if err != nil {
			return nil, err
		}
		if !running {
			continue
		}
		nets, err := c.rt.NetworkIPs(ctx, c.names.Voter(j))
		if err != nil {
			return nil, err
		}
		ip, ok := nets[c.names.Network()]
		if !ok {
			return nil, fmt.Errorf("%s has no address on %s", c.names.Voter(j), c.names.Network())
		}
		ips = append(ips, ip)
	}
	return ips, nil
}

// flushCommands are the iptables calls that remove all rules of the sidecar's
// namespace (the voter's), in both chains the partition uses.
func flushCommands() [][]string {
	return [][]string{
		{"iptables", "-F", "INPUT"},
		{"iptables", "-F", "OUTPUT"},
	}
}

// dropCommands returns the iptables calls that drop traffic to and from ips.
// Inbound drops from the peers (INPUT), outbound drops to the peers (OUTPUT).
func dropCommands(dir Direction, ips []string) [][]string {
	var out [][]string
	for _, ip := range ips {
		if dir == Inbound || dir == Both {
			out = append(out, []string{"iptables", "-I", "INPUT", "-s", ip, "-j", "DROP"})
		}
		if dir == Outbound || dir == Both {
			out = append(out, []string{"iptables", "-I", "OUTPUT", "-d", ip, "-j", "DROP"})
		}
	}
	return out
}

// Latency adds delay, jitter and loss to all egress traffic of voter i (the
// shared network namespace of the voter). It replaces an earlier setting.
func (c *Cluster) Latency(ctx context.Context, i int, spec LatencySpec) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return err
	}
	args, err := netemArgs(spec)
	if err != nil {
		return err
	}
	if _, err := c.rt.Exec(ctx, c.names.Sidecar(i), args...); err != nil {
		return fmt.Errorf("latency %s: %w", c.names.Voter(i), err)
	}
	return nil
}

// ClearLatency removes the netem qdisc of voter i. It does nothing when no
// netem qdisc is installed.
func (c *Cluster) ClearLatency(ctx context.Context, i int) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return err
	}
	active, err := c.LatencyActive(ctx, i)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}
	if _, err := c.rt.Exec(ctx, c.names.Sidecar(i), "tc", "qdisc", "del", "dev", netIface, "root"); err != nil {
		return fmt.Errorf("clear latency %s: %w", c.names.Voter(i), err)
	}
	return nil
}

// LatencyActive reports whether a netem qdisc is installed on voter i.
func (c *Cluster) LatencyActive(ctx context.Context, i int) (bool, error) {
	if err := c.checkIndex(i); err != nil {
		return false, err
	}
	out, err := c.rt.Exec(ctx, c.names.Sidecar(i), "tc", "qdisc", "show", "dev", netIface)
	if err != nil {
		return false, fmt.Errorf("show qdisc %s: %w", c.names.Voter(i), err)
	}
	return strings.Contains(out, "netem"), nil
}

// Reachable reports whether voter from can open a TCP connection to port of
// voter to, using a connect from the sidecar of from. A failed connect is
// false, not an error; errors are reserved for a broken sidecar.
func (c *Cluster) Reachable(ctx context.Context, from, to, port int) (bool, error) {
	if err := c.checkIndex(from); err != nil {
		return false, err
	}
	if err := c.checkIndex(to); err != nil {
		return false, err
	}
	if err := c.requireSidecar(ctx, from); err != nil {
		return false, err
	}
	nets, err := c.rt.NetworkIPs(ctx, c.names.Voter(to))
	if err != nil {
		return false, err
	}
	ip, ok := nets[c.names.Network()]
	if !ok {
		return false, fmt.Errorf("%s has no address on %s", c.names.Voter(to), c.names.Network())
	}
	_, err = c.rt.Exec(ctx, c.names.Sidecar(from), "nc", "-z", "-w", "3", ip, strconv.Itoa(port))
	return err == nil, nil
}

// requireSidecar checks that the sidecar of voter i runs; faults need it.
func (c *Cluster) requireSidecar(ctx context.Context, i int) error {
	running, err := c.rt.Running(ctx, c.names.Sidecar(i))
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("sidecar %s is not running", c.names.Sidecar(i))
	}
	return nil
}

// netemArgs builds the tc command that installs spec on eth0.
func netemArgs(spec LatencySpec) ([]string, error) {
	if spec.Delay < 0 || spec.Jitter < 0 {
		return nil, errors.New("delay and jitter must not be negative")
	}
	if spec.Jitter > 0 && spec.Delay == 0 {
		return nil, errors.New("jitter needs a delay")
	}
	if spec.LossPercent < 0 || spec.LossPercent > 100 {
		return nil, fmt.Errorf("loss must be between 0 and 100 percent, got %g", spec.LossPercent)
	}
	if spec.Delay == 0 && spec.LossPercent == 0 {
		return nil, errors.New("latency needs a delay or a loss")
	}
	args := []string{"tc", "qdisc", "replace", "dev", netIface, "root", "netem"}
	if spec.Delay > 0 {
		args = append(args, "delay", msString(spec.Delay))
		if spec.Jitter > 0 {
			args = append(args, msString(spec.Jitter))
		}
	}
	if spec.LossPercent > 0 {
		args = append(args, "loss", strconv.FormatFloat(spec.LossPercent, 'f', -1, 64)+"%")
	}
	return args, nil
}

func msString(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// TimeHTTP measures the wall time of one GET on the API of voter to, issued
// from the network namespace of voter from through its sidecar. The time
// includes the exec overhead of the runtime, so compare it with a baseline
// taken before the fault. Container-to-container traffic is what netem
// shapes; see Latency.
func (c *Cluster) TimeHTTP(ctx context.Context, from, to int, path string) (time.Duration, error) {
	if err := c.checkIndex(from); err != nil {
		return 0, err
	}
	if err := c.checkIndex(to); err != nil {
		return 0, err
	}
	if err := c.requireSidecar(ctx, from); err != nil {
		return 0, err
	}
	nets, err := c.rt.NetworkIPs(ctx, c.names.Voter(to))
	if err != nil {
		return 0, err
	}
	ip, ok := nets[c.names.Network()]
	if !ok {
		return 0, fmt.Errorf("%s has no address on %s", c.names.Voter(to), c.names.Network())
	}
	url := fmt.Sprintf("http://%s:%d%s", ip, voterHTTPPort, path)
	start := time.Now()
	if _, err := c.rt.Exec(ctx, c.names.Sidecar(from), "wget", "-T", "10", "-q", "-O", "/dev/null", url); err != nil {
		return 0, fmt.Errorf("GET %s from %s: %w", path, c.names.Voter(from), err)
	}
	return time.Since(start), nil
}

// HostIP resolves host (for example host.containers.internal) as seen from the
// network namespace of voter i, using the sidecar's resolver. The address is
// the one the voter's own connections to host use.
func (c *Cluster) HostIP(ctx context.Context, i int, host string) (string, error) {
	if err := c.checkIndex(i); err != nil {
		return "", err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return "", err
	}
	out, err := c.rt.Exec(ctx, c.names.Sidecar(i), "nslookup", host)
	if err != nil {
		return "", fmt.Errorf("resolve %s on %s: %w", host, c.names.Voter(i), err)
	}
	return parseNslookupAddress(out)
}

// parseNslookupAddress returns the IPv4 address of the answer section of
// busybox nslookup output. The resolver's own "Address" line (its server
// address, with a port) comes before the "Name:" line and is skipped.
func parseNslookupAddress(out string) (string, error) {
	inAnswer := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name:") {
			inAnswer = true
			continue
		}
		if !inAnswer || !strings.HasPrefix(line, "Address:") {
			continue
		}
		addr := strings.TrimSpace(strings.TrimPrefix(line, "Address:"))
		if i := strings.Index(addr, "#"); i >= 0 { // "addr#port" form of some resolvers
			addr = addr[:i]
		}
		if ip := net.ParseIP(addr); ip != nil && ip.To4() != nil {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 answer in nslookup output %q", strings.TrimSpace(out))
}

// BlockEgress drops the TCP traffic of voter i to ip on the given ports, in
// its OUTPUT chain. Other traffic, in particular the cluster traffic between
// the voters, is not affected. Each call adds one rule per port; UnblockEgress
// removes them again.
func (c *Cluster) BlockEgress(ctx context.Context, i int, ip string, ports []int) error {
	return c.egress(ctx, i, ip, ports, true)
}

// UnblockEgress removes the rules that BlockEgress installed for the same
// address and ports.
func (c *Cluster) UnblockEgress(ctx context.Context, i int, ip string, ports []int) error {
	return c.egress(ctx, i, ip, ports, false)
}

func (c *Cluster) egress(ctx context.Context, i int, ip string, ports []int, block bool) error {
	if err := c.checkIndex(i); err != nil {
		return err
	}
	if err := c.requireSidecar(ctx, i); err != nil {
		return err
	}
	cmds, err := egressCommands(ip, ports, block)
	if err != nil {
		return err
	}
	verb := "unblock"
	if block {
		verb = "block"
	}
	for _, args := range cmds {
		if _, err := c.rt.Exec(ctx, c.names.Sidecar(i), args...); err != nil {
			return fmt.Errorf("%s egress of %s: %w", verb, c.names.Voter(i), err)
		}
	}
	c.log.Info("egress "+verb+"ed", "node", c.names.Voter(i), "ip", ip, "ports", ports)
	return nil
}

// egressCommands returns the iptables calls that add (block) or remove the
// OUTPUT rules for ip and each port.
func egressCommands(ip string, ports []int, block bool) ([][]string, error) {
	if net.ParseIP(ip) == nil {
		return nil, fmt.Errorf("egress: %q is not an IP address", ip)
	}
	if len(ports) == 0 {
		return nil, errors.New("egress: no ports")
	}
	action := "-D"
	if block {
		action = "-I"
	}
	var out [][]string
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("egress: port %d out of range", p)
		}
		out = append(out, []string{"iptables", action, "OUTPUT", "-d", ip, "-p", "tcp", "--dport", strconv.Itoa(p), "-j", "DROP"})
	}
	return out, nil
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// hardDeny is a k8s runner's in-cluster deny set (design §5.3): the cluster's
// DNS suffixes, the relay and runnerd addresses, the API server and its peers,
// and the operator's CIDRs. egressTarget checks it on the name and on every
// resolved address BEFORE the internal_hosts lift, so no declaration reaches an
// in-cluster service. Agent egress only: control-plane calls never pass here.
type hardDeny struct {
	suffixes []string
	names    map[string]bool
	ips      []netip.Addr
	cidrs    []netip.Prefix
}

// blockHardDeny is a hard-deny refusal: never liftable, whatever its range.
const blockHardDeny blockKind = 0x80

func init() {
	neverLiftableClass[blockHardDeny] = "an in-cluster name or address this runner denies to every run"
}

func normalizeHost(h string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(h)), ".")
}

// compileHardDeny parses c; nil or empty config compiles to nil (no check).
func compileHardDeny(c *HardDenyConfig) (*hardDeny, error) {
	if c == nil || len(c.Suffixes)+len(c.Hosts)+len(c.CIDRs) == 0 {
		return nil, nil
	}
	h := &hardDeny{names: map[string]bool{}}
	for i, s := range c.Suffixes {
		if s = normalizeHost(s); s == "" {
			return nil, fmt.Errorf("hard_deny.suffixes[%d] is empty", i)
		}
		h.suffixes = append(h.suffixes, s)
	}
	for i, s := range c.Hosts {
		s = normalizeHost(s)
		if s == "" {
			return nil, fmt.Errorf("hard_deny.hosts[%d] is empty", i)
		}
		if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
			h.ips = append(h.ips, a.Unmap())
			continue
		}
		h.names[s] = true
	}
	for i, s := range c.CIDRs {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("hard_deny.cidrs[%d]: %w", i, err)
		}
		h.cidrs = append(h.cidrs, p.Masked())
	}
	return h, nil
}

// name reports whether host (a name or an IP literal) is denied as spelled.
func (h *hardDeny) name(host string) bool {
	if h == nil {
		return false
	}
	host = normalizeHost(host)
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return h.ip(ip)
	}
	if h.names[host] {
		return true
	}
	for _, s := range h.suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

// ip reports whether a resolved or literal address is denied.
func (h *hardDeny) ip(ip net.IP) bool {
	if h == nil {
		return false
	}
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true // an address that cannot be read is refused
	}
	a = a.Unmap()
	for _, d := range h.ips {
		if d == a {
			return true
		}
	}
	for _, p := range h.cidrs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// errHardDenied is the hostBlockedError message for a denied name.
func errHardDenied(host string) error {
	return &hostBlockedError{kind: blockHardDeny, msg: fmt.Sprintf("host %q denied: an in-cluster name or address on this runner", host)}
}

// validateHardDeny is the boot-time parse check.
//
// An upstream proxy named by an address inside the deny set is refused: the proxy would reach the
// in-cluster address on every run's behalf, which the deny set exists to prevent.
func (c *Config) validateHardDeny() error {
	h, err := compileHardDeny(c.HardDeny)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if up, err := parseUpstreamProxy(c.UpstreamProxyURL); err == nil && up != nil {
		if ip := net.ParseIP(up.host); ip != nil && h.ip(ip) {
			return fmt.Errorf("config: upstream_proxy_url names an address inside hard_deny, so every run would reach it through the proxy")
		}
	}
	return nil
}

// fixedResolver replays one lookup's answers, so the hard-deny check and the
// guard judge the same addresses with no second query.
type fixedResolver []net.IP

func (f fixedResolver) LookupIP(string) ([]net.IP, error) { return f, nil }

// hardDenyVet resolves host once and refuses it when its name or any answer is
// in the deny set. ok=false carries the refusal; otherwise res replays the
// answers to the guard. With no deny set it returns the proxy's own resolver.
func (p *Proxy) hardDenyVet(host string) (res resolver, refused IPGuardResult, ok bool) {
	if p.hardDeny == nil {
		return p.res, IPGuardResult{}, true
	}
	denied := IPGuardResult{Denied: true, Reason: "an in-cluster name or address on this runner", kind: blockHardDeny}
	if p.hardDeny.name(host) {
		return nil, denied, false
	}
	name := normalizeHost(host)
	if net.ParseIP(strings.Trim(name, "[]")) != nil {
		return p.res, IPGuardResult{}, true
	}
	r := p.res
	if r == nil {
		r = netResolver{}
	}
	ips, err := r.LookupIP(name)
	if err != nil {
		return nil, IPGuardResult{Denied: true, Unresolved: true, Reason: fmt.Sprintf("resolve failed: %v", err)}, false
	}
	if len(ips) == 0 {
		return nil, IPGuardResult{Denied: true, Unresolved: true, Reason: "no addresses"}, false
	}
	for _, ip := range ips {
		if p.hardDeny.ip(ip) {
			denied.Reason = fmt.Sprintf("blocked address %s: an in-cluster address on this runner", ip)
			return nil, denied, false
		}
	}
	return fixedResolver(ips), IPGuardResult{}, true
}

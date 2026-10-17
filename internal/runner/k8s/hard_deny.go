// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// HardDeny is the hard_deny every runner-mode proxy config carries (design K1 §5.3; the key shape
// is lane K9's contract): the names and addresses agent egress may never reach, which the proxy
// checks on the name and every resolved IP before the internal_hosts lift. It covers the cluster's
// service suffixes, the relay ClusterIP and runnerd's pod IP, the apiserver and its peers, and the
// operator's extra CIDRs (recommended: the pod and service CIDRs). Consequence (N5): a run on an org
// runner reaches no in-cluster service, declared or not.
type HardDeny struct {
	Suffixes []string `json:"suffixes"`
	Hosts    []string `json:"hosts,omitempty"`
	CIDRs    []string `json:"cidrs,omitempty"`
}

// NewHardDeny validates and assembles a HardDeny. clusterDomain must be a DNS name; every host must
// be an IP literal or a DNS name with no port, and every CIDR must parse. A malformed value is an
// error: a security knob is never silently dropped.
func NewHardDeny(clusterDomain string, hosts, cidrs []string) (*HardDeny, error) {
	clusterDomain = strings.TrimSuffix(strings.TrimSpace(clusterDomain), ".")
	if !dnsName(clusterDomain) {
		return nil, fmt.Errorf("k8s: hard deny: cluster domain %q is not a DNS name", clusterDomain)
	}
	h := &HardDeny{Suffixes: []string{"svc", clusterDomain}}
	if err := h.addHosts(hosts...); err != nil {
		return nil, err
	}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return nil, fmt.Errorf("k8s: hard deny: CIDR %q: %w", c, err)
		}
		h.CIDRs = append(h.CIDRs, p.Masked().String())
	}
	slices.Sort(h.CIDRs)
	h.CIDRs = slices.Compact(h.CIDRs)
	return h, nil
}

func dnsName(s string) bool {
	return s != "" && !strings.ContainsAny(s, " /:@[]")
}

func (h *HardDeny) addHosts(hosts ...string) error {
	for _, raw := range hosts {
		v := strings.TrimSpace(raw)
		if ip, err := netip.ParseAddr(v); err == nil {
			v = ip.Unmap().String()
		} else if !dnsName(v) {
			return fmt.Errorf("k8s: hard deny: host %q is neither an IP nor a DNS name", raw)
		}
		h.Hosts = append(h.Hosts, strings.ToLower(v))
	}
	slices.Sort(h.Hosts)
	h.Hosts = slices.Compact(h.Hosts)
	return nil
}

// withAPIServer adds the substrate's own apiserver (apiserverHostPort): its host, and for a name
// every address it resolves to now.
func (h *HardDeny) withAPIServer(ctx context.Context, hostPort string, resolve func(context.Context, string) ([]string, error)) (*HardDeny, error) {
	out := &HardDeny{Suffixes: slices.Clone(h.Suffixes), Hosts: slices.Clone(h.Hosts), CIDRs: slices.Clone(h.CIDRs)}
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, fmt.Errorf("k8s: hard deny: apiserver %q: %w", hostPort, err)
	}
	add := []string{host}
	if _, perr := netip.ParseAddr(host); perr != nil {
		addrs, rerr := resolve(ctx, host)
		if rerr != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("k8s: hard deny: resolve apiserver %q: %v", host, rerr)
		}
		add = append(add, addrs...)
	}
	if err := out.addHosts(add...); err != nil {
		return nil, err
	}
	return out, nil
}

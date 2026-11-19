// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMetadataEndpointsAreNeverLiftableKind: the AWS and Google IPv6 metadata addresses
// sit inside the ULA range an operator may lift, so the guard must class them
// with the never-liftable kinds or an internal_hosts entry could reach them.
func TestMetadataEndpointsAreNeverLiftableKind(t *testing.T) {
	for _, ip := range []string{"fd00:ec2::254", "fd00:ec2::23", "fd20:ce::254", "100.100.100.200"} {
		if kind, why := isBlockedIP(net.ParseIP(ip)); kind != blockReservedOther {
			t.Errorf("isBlockedIP(%s) = (%v, %q), want blockReservedOther", ip, kind, why)
		}
	}
	if kind, _ := isBlockedIP(net.ParseIP("fd00::1")); kind != blockPrivate {
		t.Errorf("isBlockedIP(fd00::1) = %v, want blockPrivate (still liftable)", kind)
	}
}

// TestAzureWireServerIsRefused: Azure's WireServer is public address space, so no range check holds it; the
// guard refuses it as a metadata address (never liftable) whether typed, embedded or resolved from a name,
// on its host agent plugin port 32526 as on any other, even under allow-all, while public hosts still pass.
func TestAzureWireServerIsRefused(t *testing.T) {
	for _, ip := range []string{"168.63.129.16", "::ffff:168.63.129.16", "64:ff9b::a83f:8110", "::168.63.129.16"} {
		if kind, why := isBlockedIP(net.ParseIP(ip)); kind == blockNone || kind == blockPrivate {
			t.Errorf("isBlockedIP(%s) = (%v, %q), want a never-liftable denial", ip, kind, why)
		}
	}
	res := fakeResolver{m: map[string][]net.IP{
		"wire.example.com": ips("168.63.129.16"),
		"good.example.com": ips("93.184.216.34"),
	}}
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"168.63.129.16"}}, "127.0.0.1:1", nil, nil)
	p.res = res
	for _, host := range []string{"168.63.129.16", "wire.example.com"} {
		for _, port := range []int{80, 443, 32526} {
			if d, _, log := p.evaluate(context.Background(), host, port, "GET", "/"); d != egress.Deny || log.RuleSource != "builtin:private-ip" {
				t.Errorf("evaluate(%s:%d) = %q/%q, want deny/builtin:private-ip", host, port, d, log.RuleSource)
			}
			if _, _, err := p.egressTarget(host, port); err == nil {
				t.Errorf("egressTarget(%s:%d) admitted Azure WireServer", host, port)
			}
		}
	}
	for _, host := range []string{"good.example.com", "168.63.129.17"} {
		if got := VetHost(host, res); got.Denied {
			t.Errorf("VetHost(%q) denied (%q), want allowed", host, got.Reason)
		}
		if d, _, log := p.evaluate(context.Background(), host, 32526, "GET", "/"); d == egress.Deny {
			t.Errorf("evaluate(%s:32526) denied by %q, want a public host to pass", host, log.RuleSource)
		}
	}
}

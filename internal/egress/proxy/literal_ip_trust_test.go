// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// literalIPProxy builds a proxy whose policy allows every host in allowed
// EXACTLY (the shape substituteArtifactEgress writes for an egress-redirect To),
// on a box whose own subnet is 172.18.0.0/16 and whose control plane is
// 172.18.0.2 — the docker-compose shape where the sidecar shares a network with
// postgres/dex/registry.
func literalIPProxy(t *testing.T, allowed ...string) *Proxy {
	t.Helper()
	_, ownSubnet, err := net.ParseCIDR("172.18.0.0/16")
	if err != nil {
		t.Fatalf("parse own subnet: %v", err)
	}
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: allowed}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		LocalSubnets:    []*net.IPNet{ownSubnet},
		ControlPlaneIPs: []net.IP{net.ParseIP("172.18.0.2")},
	})
}

// TestTrustsExactLiteralIP_OwnSubnetRefusalIsScoped pins BOTH halves of the
// own-subnet refusal, because a fix that only ever denies is indistinguishable
// from breaking the feature. The refusal must reach exactly the addresses the
// InternalHosts lift already withholds — the proxy's own interface subnets and
// its control-plane host — and no further: an ordinary private-endpoint mirror
// on some other RFC1918/CGNAT network is the case egress redirects exist for,
// and it must still be trusted through the same exact-allowlist door, on
// egressTarget (which the MITM/token-injection and broker legs re-vet through)
// as well as on evaluate.
func TestTrustsExactLiteralIP_OwnSubnetRefusalIsScoped(t *testing.T) {
	const (
		neighbour = "172.18.0.5" // e.g. postgres on wardyn-internal
		control   = "172.18.0.2"
		mirror    = "10.40.2.11" // a corp mirror on a different private network
		cgnat     = "100.64.5.7" // the other half of the liftable ceiling
	)
	p := literalIPProxy(t, neighbour, control, mirror, cgnat)

	for _, host := range []string{neighbour, control} {
		if tgt, src, err := p.egressTarget(host, 5432); err == nil {
			t.Errorf("egressTarget(%s:5432) = %q via %q, want refused: an exact literal allow entry must not reach the proxy's own subnet or its control-plane host",
				host, tgt, src)
		}
		if decision, _, log := p.evaluate(context.Background(), host, 5432, "CONNECT", ""); decision != egress.Deny {
			t.Errorf("evaluate(CONNECT %s:5432) = %q rule=%q, want deny", host, decision, log.RuleSource)
		}
	}
	for _, host := range []string{mirror, cgnat} {
		tgt, src, err := p.egressTarget(host, 8443)
		if err != nil || tgt != net.JoinHostPort(host, "8443") || src != ruleSourceEgressRedirect {
			t.Errorf("egressTarget(%s:8443) = %q/%q/%v, want %s:8443/%q/nil: an off-subnet private endpoint is exactly what the literal-IP trust is for",
				host, tgt, src, host, err, ruleSourceEgressRedirect)
		}
		if decision, _, log := p.evaluate(context.Background(), host, 8443, "CONNECT", ""); decision != egress.Allow {
			t.Errorf("evaluate(CONNECT %s:8443) = %q rule=%q, want allow", host, decision, log.RuleSource)
		}
	}
}

// TestTrustsExactLiteralIP_UnconfiguredProxyIsUnchanged: a proxy that knows
// neither its own subnets nor its control-plane address (every host-mode and
// unit-test construction) must behave exactly as before the refusal existed —
// onOwnSubnetOrControlPlane answers false for everything, so the new clause can
// never turn a working literal-IP redirect into a deny on a deployment that
// simply never populated those two Options.
func TestTrustsExactLiteralIP_UnconfiguredProxyIsUnchanged(t *testing.T) {
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"172.18.0.5"}}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver: publicResolver{},
	})
	if tgt, src, err := p.egressTarget("172.18.0.5", 5432); err != nil || src != ruleSourceEgressRedirect {
		t.Fatalf("egressTarget = %q/%q/%v, want the address trusted via %q when no own-subnet/control-plane is configured",
			tgt, src, err, ruleSourceEgressRedirect)
	}
}

// TestLeafFor_SANKindFollowsTheCONNECTHost pins that leafFor picks ONE SAN kind
// by the shape of the host, and the right one. The IP arm is the F7 finding (a
// DNS-only leaf for a literal-IP mirror fails every TLS client); the hostname
// arm is the non-regression half — an ordinary MITM'd LLM host must keep a DNS
// SAN and must NOT grow a bogus IP SAN.
func TestLeafFor_SANKindFollowsTheCONNECTHost(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	cases := []struct {
		host string
		isIP bool
	}{
		{"api.anthropic.com", false},
		{"mirror.corp.internal", false},
		{"10.40.2.11", true},
		{"100.64.5.7", true},
		{"fd00::1", true},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			leaf, lerr := ca.leafFor(c.host)
			if lerr != nil {
				t.Fatalf("leafFor: %v", lerr)
			}
			if leaf.Leaf == nil {
				t.Fatal("no parsed Leaf")
			}
			if verr := leaf.Leaf.VerifyHostname(c.host); verr != nil {
				t.Fatalf("leaf does not verify for its own CONNECT host: %v (DNSNames=%v IPAddresses=%v)",
					verr, leaf.Leaf.DNSNames, leaf.Leaf.IPAddresses)
			}
			gotIP := len(leaf.Leaf.IPAddresses) == 1 && len(leaf.Leaf.DNSNames) == 0
			gotDNS := len(leaf.Leaf.DNSNames) == 1 && len(leaf.Leaf.IPAddresses) == 0
			if c.isIP && !gotIP {
				t.Errorf("literal-IP host: DNSNames=%v IPAddresses=%v, want exactly one IP SAN and no DNS SAN",
					leaf.Leaf.DNSNames, leaf.Leaf.IPAddresses)
			}
			if !c.isIP && !gotDNS {
				t.Errorf("hostname host: DNSNames=%v IPAddresses=%v, want exactly one DNS SAN and no IP SAN",
					leaf.Leaf.DNSNames, leaf.Leaf.IPAddresses)
			}
		})
	}
}

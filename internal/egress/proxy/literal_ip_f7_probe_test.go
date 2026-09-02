// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F7 PROBE (lane F7-redirect-probe-sni-literal-ip) — NOT part of the tree.
//
// Intended destination: internal/egress/proxy/literal_ip_f7_probe_test.go
// (package proxy — reaches newProxy, egressTarget, evaluate, newCertAuthority,
// leafFor and the genTestCA/publicResolver helpers already in this package's
// tests).
//
// Run (no Postgres, no network):
//
//   cp local/review-0.7/deep/F7-redirect-probe-sni-literal-ip/literal_ip_f7_probe_test.go internal/egress/proxy/
//   nice -n 10 GOMAXPROCS=8 go test -p 4 ./internal/egress/proxy -run 'TestF7_' -count=1 -v
//   rm internal/egress/proxy/literal_ip_f7_probe_test.go
//
// Both tests are EXPECTED RED at fa910735 — they are the data-path halves of
// hypotheses H-5 and H-6 in ../F7-redirect-probe-sni-literal-ip.md. A green
// run means the finding was fixed; update the doc.

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

// TestF7_LeafForLiteralIP_VerifiesAsIPSAN: a token-carrying redirect whose To
// is a literal IP puts "10.40.2.11:443" on mitmHosts (planArtifactRedirect in
// artifact_redirect.go) and handleConnect TLS-terminates the sandbox's
// CONNECT 10.40.2.11:443 with leafFor("10.40.2.11") (Proxy.handleConnect ->
// Proxy.mitmConnect -> certAuthority.leafFor). leafFor mints DNSNames=[host]
// only — no IPAddresses SAN — so every sandbox TLS client (Go, OpenSSL/curl,
// Node) rejects the leaf for an IP target. The token-injection lane for a
// literal-IP mirror is therefore dead on the data path, while test-redirect
// (which never MITMs: grants nil, handleTestSiteConfigRedirect in
// site_config_probe.go) reports "reached".
func TestF7_LeafForLiteralIP_VerifiesAsIPSAN(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	for _, ip := range []string{"10.40.2.11", "100.64.5.7"} {
		leaf, err := ca.leafFor(ip)
		if err != nil {
			t.Fatalf("leafFor(%s): %v", ip, err)
		}
		if leaf.Leaf == nil {
			t.Fatalf("leafFor(%s): no parsed Leaf", ip)
		}
		if verr := leaf.Leaf.VerifyHostname(ip); verr != nil {
			t.Errorf("H-5 (expected red at fa910735): the MITM leaf minted for literal-IP CONNECT host %s does not verify for that IP: %v "+
				"(DNSNames=%v IPAddresses=%v) — a sandbox client speaking to a token-injected literal-IP mirror fails TLS",
				ip, verr, leaf.Leaf.DNSNames, leaf.Leaf.IPAddresses)
		}
	}
}

// TestF7_LiteralIPTrust_RefusesOwnSubnetAndControlPlane: the InternalHosts
// lift refuses an address on the proxy's own interface subnets or its
// control-plane host (liftInternalHost -> onOwnSubnetOrControlPlane, both in
// egress_target.go) so a declaration can never reach the sidecar's
// docker-network neighbours (postgres/dex/registry). The literal-IP
// trust — Proxy.evaluate's literal-IP step 0 and egressTarget's literal branch
// (trustsExactLiteralIP) — consults only isBlockedIP + AllowsLiteralIP and
// never that exclusion. An exact allowlist entry for a neighbour's address
// (the shape an egress-redirect To writes via substituteArtifactEgress, or
// the probe run's []string{toHost}) is trusted straight through.
func TestF7_LiteralIPTrust_RefusesOwnSubnetAndControlPlane(t *testing.T) {
	_, ownSubnet, _ := net.ParseCIDR("172.18.0.0/16")
	controlPlane := net.ParseIP("172.18.0.2")
	neighbour := "172.18.0.5" // e.g. postgres on wardyn-internal

	p := newProxy(Options{
		RunID:          uuid.New(),
		Policy:         CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{neighbour, controlPlane.String()}}),
		Sink:           &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver:       publicResolver{},
		LocalSubnets:   []*net.IPNet{ownSubnet},
		ControlPlaneIP: controlPlane,
	})

	for _, host := range []string{neighbour, controlPlane.String()} {
		if tgt, src, err := p.egressTarget(host, 5432); err == nil {
			t.Errorf("H-6 (expected red at fa910735): egressTarget(%s:5432) admitted %q via %q — an exact literal allow entry reaches the proxy's OWN subnet / control-plane host, which the InternalHosts lift refuses by design", host, tgt, src)
		}
		if decision, tgt, log := p.evaluate(context.Background(), host, 5432, "CONNECT", ""); decision != egress.Deny {
			t.Errorf("H-6 (expected red at fa910735): evaluate(CONNECT %s:5432) = %q target=%q rule=%q, want deny (own-subnet / control-plane address)", host, decision, tgt, log.RuleSource)
		}
	}
}

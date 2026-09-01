// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Regression (caught live by e2e): a literal blocked IP must be denied by the
// builtin guard BEFORE the first-use approval path. The original ordering ran
// policy/approval first, so 169.254.169.254 fell into "unknown host" and
// raised an approvable egress_domain request — meaning an admin could have
// approved egress to the cloud metadata service, violating invariant 3
// (private/link-local/metadata ranges are blocked regardless of policy).
func TestEvaluate_BlockedLiteralIPBeatsFirstUseApproval(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains:   []string{"github.com"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}
	p, buf := newTestProxy(t, spec, "127.0.0.1:1", nil, nil)

	for _, host := range []string{
		"169.254.169.254",  // cloud metadata
		"10.1.2.3",         // RFC1918
		"127.0.0.1",        // loopback
		"::1",              // v6 loopback
		"::ffff:127.0.0.1", // v4-mapped smuggle
	} {
		buf.Reset()
		decision, target, log := p.evaluate(context.Background(), host, 80, "GET", "/")
		if decision != egress.Deny {
			t.Errorf("%s: decision = %q, want deny (got rule %q)", host, decision, log.RuleSource)
		}
		if target != "" {
			t.Errorf("%s: dial target must be empty on deny, got %q", host, target)
		}
		if log.RuleSource != "builtin:private-ip" {
			t.Errorf("%s: rule_source = %q, want builtin:private-ip (the guard must fire BEFORE policy/approval)", host, log.RuleSource)
		}
	}
}

// Regression (W13-S1-3): an operator-declared egress-redirect target that is
// itself a private/reserved IP literal (a realistic "To" for an on-prem
// registry — see docs/OPERATIONS.md "network only" redirects and
// site_config.go's validSiteURLOrHost, which happily persists one) must
// actually be reachable, not unconditionally denied by the same guard
// TestEvaluate_BlockedLiteralIPBeatsFirstUseApproval proves fires for an
// UNDECLARED private IP above. An EXACT AllowedDomains entry naming the
// literal address is the one deliberate, narrowly-scoped exception; a
// sibling private IP the operator never declared must still be denied.
func TestEvaluate_DeclaredLiteralIPRedirectTargetAllowed(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"10.40.2.11", "github.com"}}
	p, buf := newTestProxy(t, spec, "127.0.0.1:1", nil, nil)

	buf.Reset()
	decision, target, log := p.evaluate(context.Background(), "10.40.2.11", 8443, "GET", "/")
	if decision != egress.Allow {
		t.Fatalf("declared literal IP: decision = %q, want allow (rule %q)", decision, log.RuleSource)
	}
	if target != "10.40.2.11:8443" {
		t.Fatalf("declared literal IP: target = %q, want 10.40.2.11:8443", target)
	}
	if log.RuleSource == "builtin:private-ip" {
		t.Fatalf("declared literal IP: rule_source = %q, must not be the unconditional guard", log.RuleSource)
	}

	// A sibling private IP the operator never declared stays denied — the
	// exemption is scoped to the exact declared entry, never blanket.
	buf.Reset()
	decision, target, log = p.evaluate(context.Background(), "10.40.2.12", 8443, "GET", "/")
	if decision != egress.Deny || log.RuleSource != "builtin:private-ip" {
		t.Fatalf("undeclared sibling IP: decision = %q rule = %q, want deny/builtin:private-ip", decision, log.RuleSource)
	}
	if target != "" {
		t.Fatalf("undeclared sibling IP: target must be empty on deny, got %q", target)
	}
}

// The literal-IP exemption is scoped to blockPrivate (RFC1918/ULA/CGNAT), the
// same ceiling SiteConfig.InternalHosts lifts. An operator who allow-lists a
// loopback/link-local/metadata literal must NOT thereby hand the sandbox that
// address — declaring 169.254.169.254 in allowed_domains is still denied, at
// both the step-0 guard (evaluate) and the re-vet path (egressTarget). Without
// this, a `kind != blockNone` gate trusted any declared blocked literal, so an
// allowed_domains entry was an SSRF-to-metadata footgun (audit finding).
func TestEvaluate_DeclaredMetadataLiteralStillDenied(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{
		"169.254.169.254", // cloud metadata (blockLocal)
		"127.0.0.1",       // loopback (blockLocal)
		"10.40.2.11",      // RFC1918 (blockPrivate) — the one that MAY be trusted
	}}
	p, _ := newTestProxy(t, spec, "127.0.0.1:1", nil, nil)

	for _, host := range []string{"169.254.169.254", "127.0.0.1"} {
		decision, target, log := p.evaluate(context.Background(), host, 443, "GET", "/")
		if decision != egress.Deny || log.RuleSource != "builtin:private-ip" {
			t.Errorf("evaluate(%s): decision=%q rule=%q, want deny/builtin:private-ip even though declared", host, decision, log.RuleSource)
		}
		if target != "" {
			t.Errorf("evaluate(%s): target must be empty on deny, got %q", host, target)
		}
		if _, _, err := p.egressTarget(host, 443); err == nil {
			t.Errorf("egressTarget(%s): must refuse a declared metadata/loopback literal", host)
		}
	}

	// The blockPrivate sibling in the same policy stays reachable — the fix
	// narrows the exemption, it does not remove it.
	if tgt, src, err := p.egressTarget("10.40.2.11", 8443); err != nil || tgt != "10.40.2.11:8443" || src != ruleSourceEgressRedirect {
		t.Fatalf("egressTarget(10.40.2.11)=%q/%q/%v, want 10.40.2.11:8443/%q/nil", tgt, src, err, ruleSourceEgressRedirect)
	}
}

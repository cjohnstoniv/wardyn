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

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

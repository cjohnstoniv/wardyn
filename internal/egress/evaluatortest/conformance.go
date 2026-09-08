// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package evaluatortest provides a reusable conformance suite for any
// egress.Evaluator implementation, so the blessed default (builtin) and a future
// alternate (OPA/Cedar) are held to the identical policy-verdict contract.
package evaluatortest

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// EvaluatorErrorRuleSource is the operator-visible rule source a host whose
// policy could not be evaluated is denied under (docs/UI-SANDBOXES.md publishes
// it as a decision reason). It lives here rather than only in the proxy because
// the fail-closed handling it names is part of the Evaluator CONTRACT
// (egress.Evaluator: "An EvaluateHost error MUST be treated as deny"), and a
// contract with no shared assertion is a comment.
const EvaluatorErrorRuleSource = "policy:evaluator-error"

// ErrEvaluate is the error ErrorEvaluator returns.
var ErrEvaluate = errors.New("evaluatortest: engine unavailable")

// ErrorEvaluator is an egress.Evaluator whose host evaluation always fails —
// the OPA sidecar that is down, the Cedar policy that will not compile, the
// network call an alternate engine makes. Its methods are otherwise permissive
// (MethodAllowed returns true) so a Deny observed by RunHostErrorFailsClosed
// can only have come from the error handling under test.
type ErrorEvaluator struct{}

func (ErrorEvaluator) Name() string { return "evaluatortest:error" }

func (ErrorEvaluator) EvaluateHost(context.Context, egress.Request) (egress.HostVerdict, error) {
	// The verdict returned alongside the error is deliberately the MOST
	// permissive one: a host that fails closed only because the engine also
	// happened to say "deny" is not failing closed at all.
	return egress.VerdictAllow, ErrEvaluate
}

func (ErrorEvaluator) MethodAllowed(string) bool { return true }

// RunHostErrorFailsClosed holds a HOST of the Evaluator seam — the proxy today,
// any other consumer tomorrow — to the error half of the contract:
// egress.Evaluator states "An EvaluateHost error MUST be treated as deny (fail
// closed)", and nothing tested it. evaluate is given an evaluator that always
// errors and must report what the host decided plus the rule source it recorded.
//
// F142: with the branch disabled (`if verr != nil` -> `if false`) the whole
// egress + ipguard + hostrules + contentscan suite stayed green, and the shared
// conformance suite could not catch it either — its verdict helper t.Fatalf's on
// any error, so an alternate engine can pass conformance while returning the
// very errors this branch is the only thing standing behind.
func RunHostErrorFailsClosed(t *testing.T, evaluate func(egress.Evaluator) (egress.Decision, string)) {
	t.Helper()
	decision, ruleSource := evaluate(ErrorEvaluator{})
	if decision != egress.Deny {
		t.Fatalf("an EvaluateHost error produced decision %q, want %q: the Evaluator contract "+
			"makes an evaluation error a DENY, so a failed/unavailable policy engine can never "+
			"become open egress", decision, egress.Deny)
	}
	if ruleSource != EvaluatorErrorRuleSource {
		t.Fatalf("rule_source = %q, want %q: an operator has to be able to tell a policy that "+
			"denied from a policy that could not be evaluated", ruleSource, EvaluatorErrorRuleSource)
	}
}

// RunConformance exercises the egress.Evaluator contract. newFromSpec must build
// an evaluator that enforces the given RunPolicySpec.
func RunConformance(t *testing.T, newFromSpec func(types.RunPolicySpec) egress.Evaluator) {
	ctx := context.Background()
	verdict := func(spec types.RunPolicySpec, host string) egress.HostVerdict {
		t.Helper()
		v, err := newFromSpec(spec).EvaluateHost(ctx, egress.Request{Host: host})
		if err != nil {
			t.Fatalf("EvaluateHost(%q): %v", host, err)
		}
		return v
	}

	t.Run("empty_policy_is_unknown_then_proxy_default_denies", func(t *testing.T) {
		// An empty allowlist (no allow-all) yields Unknown for any host — the proxy
		// turns Unknown into deny (or approval). The evaluator must NOT return Allow.
		if v := verdict(types.RunPolicySpec{}, "example.com"); v == egress.VerdictAllow {
			t.Fatalf("empty policy must not allow; got %v", v)
		}
	})

	t.Run("allow_then_allow", func(t *testing.T) {
		if v := verdict(types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}}, "api.example.com"); v != egress.VerdictAllow {
			t.Fatalf("allowed host = %v, want allow", v)
		}
	})

	t.Run("deny_beats_allow", func(t *testing.T) {
		spec := types.RunPolicySpec{
			AllowedDomains: []string{"api.example.com"},
			DeniedDomains:  []string{"api.example.com"},
		}
		if v := verdict(spec, "api.example.com"); v != egress.VerdictDeny {
			t.Fatalf("deny must beat allow; got %v", v)
		}
	})

	t.Run("wildcard_is_label_boundary_safe", func(t *testing.T) {
		spec := types.RunPolicySpec{AllowedDomains: []string{"*.example.com"}}
		if v := verdict(spec, "a.example.com"); v != egress.VerdictAllow {
			t.Fatalf("*.example.com should allow a.example.com; got %v", v)
		}
		if v := verdict(spec, "example.com"); v == egress.VerdictAllow {
			t.Fatalf("*.example.com must NOT allow the bare apex example.com; got %v", v)
		}
		if v := verdict(spec, "notexample.com"); v == egress.VerdictAllow {
			t.Fatalf("*.example.com must NOT allow notexample.com; got %v", v)
		}
	})

	t.Run("allow_all_does_not_disable_deny", func(t *testing.T) {
		spec := types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"evil.example.com"}}
		if v := verdict(spec, "anything.example.org"); v != egress.VerdictAllow {
			t.Fatalf("allow-all should allow a non-denied host; got %v", v)
		}
		if v := verdict(spec, "evil.example.com"); v != egress.VerdictDeny {
			t.Fatalf("allow-all must still honor the deny-list; got %v", v)
		}
	})

	t.Run("method_restriction", func(t *testing.T) {
		ev := newFromSpec(types.RunPolicySpec{AllowedMethods: []string{"GET"}})
		if !ev.MethodAllowed("GET") {
			t.Fatal("GET should be allowed")
		}
		if ev.MethodAllowed("POST") {
			t.Fatal("POST should be denied by a GET-only restriction")
		}
		// Empty restriction allows all methods.
		if !newFromSpec(types.RunPolicySpec{}).MethodAllowed("DELETE") {
			t.Fatal("empty method restriction should allow all methods")
		}
	})
}

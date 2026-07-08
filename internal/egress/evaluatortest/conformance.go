// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package evaluatortest provides a reusable conformance suite for any
// egress.Evaluator implementation, so the blessed default (builtin) and a future
// alternate (OPA/Cedar) are held to the identical policy-verdict contract.
package evaluatortest

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/evaluatortest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEvaluatorErrorFailsClosed pins F142: evaluate() must treat an
// egress.Evaluator error as a DENY under rule_source policy:evaluator-error.
//
// internal/egress/evaluator.go states it as a MUST and docs/UI-SANDBOXES.md
// publishes the rule source as an operator-visible decision reason, but nothing
// exercised the branch: replacing `if verr != nil {` with `if false {` left the
// whole ./internal/egress/... ./internal/ipguard/... ./internal/hostrules/...
// ./internal/contentscan/... suite green. The policy the erroring evaluator is
// wrapped around is ALLOW-ALL-shaped on purpose — under a default-deny spec a
// Deny here would prove nothing, since the request would be refused whether the
// fail-closed branch existed or not.
//
// The assertions themselves live in evaluatortest (the shared suite), so a
// future OPA/Cedar host is held to the same contract instead of re-deriving it.
func TestEvaluatorErrorFailsClosed(t *testing.T) {
	evaluatortest.RunHostErrorFailsClosed(t, func(ev egress.Evaluator) (egress.Decision, string) {
		p := newProxy(Options{
			RunID:     uuid.New(),
			Policy:    CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
			Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
			Resolver:  publicResolver{},
			Evaluator: ev,
		})
		d, target, log := p.evaluate(context.Background(), "example.com", 443, http.MethodConnect, "")
		if target != "" {
			t.Errorf("dial target = %q on an unevaluatable policy, want empty", target)
		}
		return d, decisionReason(log)
	})
}

// TestEvaluatorErrorControl is the counterweight: the SAME proxy shape with a
// working evaluator allows the same host, so the Deny above is the error
// handling and not the fixture.
func TestEvaluatorErrorControl(t *testing.T) {
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver: publicResolver{},
	})
	if d, _, log := p.evaluate(context.Background(), "example.com", 443, http.MethodConnect, ""); d != egress.Allow {
		t.Fatalf("control: example.com = %v (%s), want allow", d, decisionReason(log))
	}
}

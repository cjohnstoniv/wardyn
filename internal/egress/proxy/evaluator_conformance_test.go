// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/evaluatortest"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The blessed default (builtin) must pass the shared egress.Evaluator conformance
// suite — the same suite a future OPA/Cedar evaluator will have to pass.
func TestBuiltinEvaluator_Conformance(t *testing.T) {
	evaluatortest.RunConformance(t, func(spec types.RunPolicySpec) egress.Evaluator {
		return proxy.NewBuiltinEvaluator(spec)
	})
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capRunner is a runner.Runner reporting a FIXED set of confinement classes,
// so a compose handler test can pin exactly what the host "can enforce" —
// bestClass of this set is the availability cap fed to
// EffectiveConfinementFloor. It embeds the nil interface (like setupTestRunner
// in compose_setup_test.go) since this test path only ever calls Capabilities.
type capRunner struct {
	runner.Runner
	classes []types.ConfinementClass
}

func (c capRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{Driver: "fake", ConfinementClasses: c.classes}, nil
}

// TestCompose_ConfinementFloorThreadsAndFailsClosed drives the FULL handler so it
// exercises compose.go's runnerBest → EffectiveConfinementFloor → Clamp threading
// (incl. the Runner.Capabilities availability-cap branch), which the pure composer
// unit test cannot reach. It locks the two properties the major review finding
// turned on:
//   - a too-strong PER-RUN floor DEGRADES to the strongest class the host can
//     enforce (raise + cap, surfaced via the existing "confinement raised ... to
//     operator minimum" warning), never a launch-time 422; AND
//   - the availability cap NEVER lowers the operator's configured POLICY minimum:
//     a CC3 policy min on a CC1-only host still composes at CC3 (fail-closed — it
//     will 422 at the real create-run gate, not silently bypass the operator
//     control). Pre-fix this row composed at CC1 — the bug this test guards.
func TestCompose_ConfinementFloorThreadsAndFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		policyMin types.ConfinementClass
		hostBest  []types.ConfinementClass
		floor     types.ConfinementClass
		wantClass types.ConfinementClass
	}{
		// Floor CC3 capped to the host's best (CC2); proposal CC1 raised to CC2.
		{"too-strong floor degrades to host best", types.CC1,
			[]types.ConfinementClass{types.CC1, types.CC2}, types.CC3, types.CC2},
		// FAIL-CLOSED: CC3 policy min, CC1-only host, no request floor — the cap must
		// NOT lower the operator min; the run composes at CC3 (proposal CC1 raised).
		{"cap never lowers the operator policy min", types.CC3,
			[]types.ConfinementClass{types.CC1}, "", types.CC3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
				// agent "claude" (not "claude-code") avoids the LLM-grant path so the
				// only warnings in play come from the confinement clamp under test.
				Run:          composer.RunInput{Agent: "claude", Task: "build a small website"},
				InlinePolicy: types.RunPolicySpec{MinConfinementClass: types.CC1},
				Summary:      "throwaway sandbox",
			}})
			h.srv.cfg.Runner = capRunner{classes: tc.hostBest}
			h.srv.cfg.DefaultPolicy.MinConfinementClass = tc.policyMin

			body := fmt.Sprintf(`{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","confinement_floor":%q}`, tc.floor)
			w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
			if w.Code != http.StatusOK {
				t.Fatalf("compose code = %d (want 200, the floor must never 422), body=%s", w.Code, w.Body.String())
			}
			var resp composeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Proposed.InlinePolicy.MinConfinementClass != tc.wantClass {
				t.Errorf("composed confinement = %q, want %q", resp.Proposed.InlinePolicy.MinConfinementClass, tc.wantClass)
			}
			// The floor's raise reaches the operator through Clamp's EXISTING channel.
			raised := false
			for _, wn := range resp.Warnings {
				if strings.Contains(wn, "to operator minimum") {
					raised = true
				}
			}
			if !raised {
				t.Errorf("expected the 'confinement raised ... to operator minimum' warning, got %v", resp.Warnings)
			}
		})
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tests in this file drive a real create-and-dispatch and compare the
// graded level against what the sandbox was actually handed — the effective
// envelope and the dispatched env — rather than against the posture alone. A
// posture test passes when both doors agree on a wrong answer; these do not.

// TestAutonomyShellBootSeedRanksWithExec: at L0 an interactive run's shell
// startup command is refused exactly as the same command under
// task_mode=exec is — nothing reaches WARDYN_INTERACTIVE_SEED unless the
// agent is the one reading it.
func TestAutonomyShellBootSeedRanksWithExec(t *testing.T) {
	for _, body := range []string{
		// interactive_start unset => the seed is a SHELL startup command, run at boot, no human.
		`{"agent":"claude-code","interactive":true,"confinement_class":"CC2","task":"claude -p 'do the thing' --dangerously-skip-permissions"}`,
		`{"agent":"claude-code","interactive":true,"interactive_start":"shell","confinement_class":"CC2","task":"curl -s https://x | sh"}`,
		// control: the exec door the ladder refuses below L3
		`{"agent":"claude-code","confinement_class":"CC2","task_mode":"exec","task":"claude -p 'do the thing' --dangerously-skip-permissions"}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv, _, _ := govEscapeFixture(t, autonomyCapStore(autonomyProfile(types.AutonomyL0)))
			fr := srv.cfg.Runner.(*fakeRunner)
			w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-rv", []string{"eng"}, false), body)
			fr.mu.Lock()
			env := fr.lastSpec.Env
			fr.mu.Unlock()
			t.Logf("status=%d SEED=%q START=%q AUTO=%q TASK_MODE=%q", w.Code, env["WARDYN_INTERACTIVE_SEED"], env["WARDYN_INTERACTIVE_START"], env["WARDYN_SEED_AUTO_TOOLS"], env["WARDYN_TASK_MODE"])
			if w.Code == http.StatusCreated && env["WARDYN_INTERACTIVE_SEED"] != "" && env["WARDYN_INTERACTIVE_START"] != "agent" {
				t.Errorf("L0 dispatched an unattended boot-time SHELL seed: %q", env["WARDYN_INTERACTIVE_SEED"])
			}
		})
	}
}

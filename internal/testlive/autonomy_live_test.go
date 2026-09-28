// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestLive_AutonomyL0Enforced (LL5, #705): proves autonomy is enforced the
// way the product actually sets it — a governance profile's AutonomyRubric,
// folded against a run's resolved posture (internal/api/runs_autonomy.go),
// NOT a CLI flag (there is none; the runbook's own GAP note was right that no
// `--autonomy` exists, and this test does not add one).
//
// The member this test drives must already have a governance profile
// assignment that resolves this run's posture to AutonomyL0 — a one-time
// admin setup, the same shape LL2's Azure DevOps project/repo fixtures are:
// this suite does not author governance profiles, it proves one that already
// caps at L0 actually refuses.
//
// Two subtests, because L0 is "attended: interactive only"
// (types.AutonomyL0's doc comment) and the issue's two audit-row claims
// belong to the two different run shapes that fact allows:
//
//   - unattended_refused: a NON-interactive run is refused BEFORE a run row,
//     an identity or a model credential exists (autonomyLadder's
//     `runs.interactive` gate) — the shell command never reaches an agent
//     and no run.exec row is ever written, because there is no run for one
//     to attach to. Zero model calls.
//   - interactive_agent_policy_delivered: an INTERACTIVE run IS permitted at
//     L0, reaches dispatch and gets a real sandbox, so this is where
//     run.agent_policy.write's delivered=true actually lives — proving the
//     managed-settings file (internal/agentpolicy) reached the sandbox on
//     whichever runner substrate this deployment uses. This request carries
//     NO Task: agentPolicyFor's document is chosen from run.Agent and
//     run.AutonomyLevel alone, so an unseeded interactive run gets it
//     exactly the same as a seeded one — but a non-empty Task on an
//     interactive run becomes WARDYN_INTERACTIVE_SEED
//     (runs_dispatch_mounts.go), which the claude-code image's boot session
//     runs as `claude "$seed"` in tmux BEFORE anyone attaches, spending at
//     least one real model call. Leaving Task empty is what keeps this
//     subtest's cost at zero. The run is killed at the end regardless of
//     outcome.
//
// The ≤3-call budget in the issue is unattended_refused's FAILURE-path
// ceiling, not either subtest's success-path cost (both are zero calls): if
// the gate has regressed and the unattended run is admitted, this test kills
// it immediately (rather than let an unattended agent run loose) and reports
// the regression as a finding — the runbook's own instruction for this
// outcome ("don't try to fix your command").
func TestLive_AutonomyL0Enforced(t *testing.T) {
	Require(t, EnvAutonomy, EnvBaseURL, EnvIdentities, EnvAutonomyModelProvider)
	ids, err := LoadIdentities()
	if err != nil {
		Fatalf(t, "%v", err)
	}
	member := ids["member"]
	if member.APIToken == "" {
		Fatalf(t, "live: the identities file has no member api_token. Sign in to the console once as the "+
			"member and create an API token under Settings, then add it as member.api_token. This member must "+
			"also have a governance profile assignment that caps this run's posture at L0 (docs/LIVE-TESTS.md, LL5)")
	}
	c := client.New(os.Getenv(EnvBaseURL), member.APIToken)
	modelProvider := os.Getenv(EnvAutonomyModelProvider)

	t.Run("unattended_refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		created, err := c.CreateRun(ctx, client.CreateRunRequest{
			Agent:         "claude-code",
			Task:          "Run a shell command: cat /etc/passwd",
			ModelProvider: modelProvider,
			Title:         "live-local LL5",
		})

		var apiErr *client.APIError
		if !errors.As(err, &apiErr) {
			// The regression path: either the run was flatly ADMITTED (err ==
			// nil) or it failed for some OTHER reason. Either way, stop
			// anything that may have started running unattended before
			// reporting. run.ID is not a secret (it is how an operator kills
			// a stuck run) — t.Fatalf/t.Logf directly, bypassing Redact's
			// generic GUID mask.
			if err == nil {
				run := created.AgentRun
				t.Logf("run %s was ADMITTED non-interactively; killing it now", run.ID)
				if _, kerr := c.KillRun(ctx, run.ID); kerr != nil {
					t.Logf("kill %s: %v (may already have exited)", run.ID, kerr)
				}
				t.Fatalf("run %s was admitted: L0 no longer refuses an unattended run. This is the regression "+
					"T-45 exists to catch — report it, don't adjust the request (docs/LIVE-TESTS.md, LL5)", run.ID)
			}
			Fatalf(t, "create run: %v (want a governance_profile 403, not this)", err)
		}
		if apiErr.Status != 403 {
			Fatalf(t, "create run refused %d, want 403: %s", apiErr.Status, apiErr.Body)
		}
		// No check against apiErr.Reason: refuse() -> writeError sends an
		// EMPTY reason on this refusal (errorBody.Reason is omitempty, and
		// refuse() never sets one for this gate) — a reason check would fail
		// red against a server enforcing L0 correctly (#1247 review F1).
		// AutonomyL0RefusalOK grades the body text instead, which IS what the
		// server actually sends.
		if err := AutonomyL0RefusalOK(apiErr.Body); err != nil {
			Fatalf(t, "%v", err)
		}
		Logf(t, "unattended run refused before it existed: 403, autonomy level L0")
	})

	t.Run("interactive_agent_policy_delivered", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		created, err := c.CreateRun(ctx, client.CreateRunRequest{
			Agent:         "claude-code",
			Interactive:   true,
			ModelProvider: modelProvider,
			Title:         "live-local LL5-interactive",
		})
		if err != nil {
			Fatalf(t, "create interactive run: %v (L0 permits interactive; this should have been admitted)", err)
		}
		run := created.AgentRun
		defer func() {
			if _, err := c.KillRun(context.Background(), run.ID); err != nil {
				t.Logf("kill %s: %v (may already have exited)", run.ID, err)
			}
		}()

		for run.State != types.RunRunning && !run.State.IsTerminal() {
			select {
			case <-ctx.Done():
				t.Fatalf("run %s still %s after 3 minutes", run.ID, run.State)
			case <-time.After(3 * time.Second):
			}
			if run, err = c.GetRun(ctx, run.ID); err != nil {
				Fatalf(t, "get run: %v", err)
			}
		}
		if run.State != types.RunRunning {
			t.Fatalf("run %s ended %s before it ever reached RUNNING: %s", run.ID, run.State, Redact(run.FailureHint))
		}

		// run.agent_policy.write is written once the agent's container exists
		// (auditAgentPolicy's own doc), which races this poll by at most a
		// beat.
		var events []types.AuditEvent
		for i := 0; i < 10; i++ {
			if events, err = c.AuditEvents(ctx, run.ID); err != nil {
				t.Fatalf("audit events for %s: %v", run.ID, err)
			}
			if AgentPolicyDeliveredOK(events, string(types.AutonomyL0)) == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		if err := AgentPolicyDeliveredOK(events, string(types.AutonomyL0)); err != nil {
			t.Fatalf("run %s: %v", run.ID, err)
		}
		t.Logf("run %s: managed-settings delivered at L0", run.ID)
	})
}

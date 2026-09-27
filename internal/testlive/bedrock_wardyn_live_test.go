// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestLiveBedrockWardyn (LL3w, #691): Bedrock reached THROUGH a governed
// Wardyn run, as opposed to LL3 (bedrock_live_test.go), which drives the
// Bedrock data plane directly from the test process on Identity Center role
// credentials it holds itself.
//
// The member here holds none of its own: the run's model credential is
// Wardyn's own per-user AWS SSO capture (resolveBedrockAuth's ssoInject lane,
// internal/api/runs_bedrock.go — selectable only for agent="claude-code",
// non-exec, non-interactive: bedrockLaneSelectable), bound to the run through
// IntegrationID naming this install's Bedrock Integration. The member must
// have signed in to AWS through the console once already (docs/LIVE-TESTS.md
// setup) so that capture exists.
//
// converse_through_wardyn proves the ordinary path: the run completes, and
// its own audit trail carries a credential.* row that is never the shared
// admin bearer (BedrockWardynCredentialRowsOK) — this run's own credential,
// not a shared one.
//
// forced_access_denied proves the fault path (internal/egress/proxy/
// bedrock_fault.go's bedrockUpstreamFault): pointed at a SECOND Integration
// the owner has bound to a model this capped account's service control
// policy denies, AWS's real 403 AccessDeniedException reaches the run's own
// failure_hint (internal/api/bedrock_dataplane_fault.go) unmodified — the
// proxy only ever observes, never retries or rewrites it. It is OPTIONAL
// (EnvBedrockWardynDeniedIntegrationID): unset, this half alone skips, named,
// rather than blocking the whole suite on a fixture LL3 never needed. A real
// ThrottlingException is not exercised live: reliably forcing one means
// exhausting the account's own quota, which is the one thing this suite's
// spend fence exists to avoid.
func TestLiveBedrockWardyn(t *testing.T) {
	Require(t, EnvBedrockWardyn, EnvBaseURL, EnvIdentities, EnvBedrockWardynIntegrationID)
	ids, err := LoadIdentities()
	if err != nil {
		Fatalf(t, "%v", err)
	}
	member := ids["member"]
	if member.APIToken == "" {
		Fatalf(t, "live: the identities file has no member api_token. Sign in to the console once as the member, "+
			"connect AWS SSO when asked, create an API token under Settings, and add it as member.api_token (docs/LIVE-TESTS.md, LL3w)")
	}
	c := client.New(os.Getenv(EnvBaseURL), member.APIToken)

	t.Run("converse_through_wardyn", func(t *testing.T) {
		run := liveBedrockWardynRun(t, c, os.Getenv(EnvBedrockWardynIntegrationID), "live-local LL3w")
		if run.State != types.RunCompleted {
			Fatalf(t, "run %s ended %s: %s. If the hint names a missing AWS sign-in, sign in to AWS again from "+
				"Settings as the member and re-run (docs/LIVE-TESTS.md, LL3w)", run.ID, run.State, run.FailureHint)
		}
		events, err := c.AuditEvents(context.Background(), run.ID)
		if err != nil {
			Fatalf(t, "audit events for %s: %v", run.ID, err)
		}
		if err := BedrockWardynCredentialRowsOK(events); err != nil {
			Fatalf(t, "run %s: %v", run.ID, err)
		}
		Logf(t, "run %s COMPLETED with a per-run (never shared-admin) credential row", run.ID)
	})

	t.Run("forced_access_denied", func(t *testing.T) {
		denied := os.Getenv(EnvBedrockWardynDeniedIntegrationID)
		if denied == "" {
			Skipf(t, "live: %s unset — point it at a second Bedrock Integration bound to a model the capped "+
				"account's service control policy denies, to prove the forced-AccessDenied half of LL3w (docs/LIVE-TESTS.md, LL3w)",
				EnvBedrockWardynDeniedIntegrationID)
		}
		run := liveBedrockWardynRun(t, c, denied, "live-local LL3w-deny")
		if err := BedrockWardynForcedFaultOK(run.FailureHint, "AccessDeniedException"); err != nil {
			Fatalf(t, "run %s ended %s: %v", run.ID, run.State, err)
		}
		Logf(t, "run %s: AWS's real AccessDeniedException reached failure_hint unmodified", run.ID)
	})
}

// liveBedrockWardynRun launches one minimal non-interactive claude-code run
// against integrationID and waits for a terminal state, tolerating a short
// extra wait for a fault hint that can land just after termination
// (bedrockUpstreamFault's own doc: the decision row races the completion
// watcher). It never asserts the end state itself — callers grade that
// differently for the clean and forced-fault cases.
func liveBedrockWardynRun(t *testing.T, c *client.Client, integrationID, title string) types.AgentRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Agent:         "claude-code",
		Task:          "Reply with the single word pong and nothing else.",
		IntegrationID: integrationID,
		Title:         title,
	})
	if err != nil {
		Fatalf(t, "create run: %v", err)
	}
	for _, w := range created.Warnings {
		Logf(t, "warning: %s", w)
	}
	run := created.AgentRun
	for !run.State.IsTerminal() {
		select {
		case <-ctx.Done():
			Fatalf(t, "run %s still %s after 5 minutes", run.ID, run.State)
		case <-time.After(3 * time.Second):
		}
		if run, err = c.GetRun(ctx, run.ID); err != nil {
			Fatalf(t, "get run: %v", err)
		}
	}
	// A fault hint written "as the decision arrives" can trail the completion
	// watcher by a beat; give it a few extra polls before grading FailureHint.
	for i := 0; i < 5 && run.FailureHint == "" && run.State == types.RunFailed; i++ {
		time.Sleep(2 * time.Second)
		if run, err = c.GetRun(ctx, run.ID); err != nil {
			Fatalf(t, "get run: %v", err)
		}
	}
	return run
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// bedrockWardynReply is the exact reply text every LL3w run asks its model
// for — kept short and fixed so the transcript check has one literal to
// search for, and so worst-case spend per call stays negligible: one short
// prompt, one short reply, on a model the ONLY-genuine-lane subtest verifies
// post-hoc is Claude Haiku 4.5 or Amazon Nova Micro (bedrock.go's
// ModelAllowed). There is no independent max_tokens fence on this path —
// unlike LL3's own direct SigV4 calls, a claude-code harness run sends its
// own system prompt and default generation settings, which this suite cannot
// override from the CreateRun API. Worst case, absent the model check
// catching a misconfigured Integration first, is one claude-code turn's
// ordinary token usage on Haiku/Nova pricing — a few cents, not the
// unbounded cost a larger model would risk.
const bedrockWardynReply = "pong"

// TestLive_BedrockWardyn (LL3w, #691): Bedrock reached THROUGH a governed
// Wardyn run, as opposed to LL3 (bedrock_live_test.go), which drives the
// Bedrock data plane directly from the test process on Identity Center role
// credentials it holds itself. Named so `-run TestLiveBedrock` (LL3's own
// -run pattern) does not also match this test.
//
// The member here holds none of its own: the run's model credential is
// Wardyn's own per-user AWS SSO capture (resolveBedrockAuth's ssoInject lane,
// internal/api/runs_bedrock.go — selectable only for agent="claude-code",
// non-exec, non-interactive: bedrockLaneSelectable), bound to the run through
// IntegrationID naming this install's Bedrock Integration. The member must
// have signed in to AWS through the console once already (docs/LIVE-TESTS.md
// setup) so that capture exists.
//
// converse_through_wardyn proves the run actually used Bedrock, on the
// per-user SSO lane specifically, on an allow-listed model, with a real
// credential mint (BedrockWardynRunProvesPerUserSSO); that the run is
// attributed to the member, never anyone else (RunCreatedByIsMember); and
// that the model's own reply reached the transcript (TranscriptContainsReply)
// — never just "some credential.* row exists", which any run of any kind
// would trivially carry.
//
// forced_access_denied proves the fault path (internal/egress/proxy/
// bedrock_fault.go's bedrockUpstreamFault) on the ONE lane it is actually
// observable on: pointed at a SECOND Integration the owner has bound, on the
// BEARER (API-key) lane, to a model this capped account's service control
// policy denies, Wardyn's own AccessDeniedException sentence
// (bedrockFaultHints — keyed by AWS's error class, not AWS's raw response
// verbatim) reaches the run's failure_hint. It is NOT observable on the
// per-user SSO lane: that lane's bedrock-runtime traffic is an opaque,
// un-MITM'd tunnel (isMITMHost covers only the SSO portal host, not
// bedrock-runtime, on that lane), so bedrockUpstreamFault never runs and no
// hint is ever written — the subtest checks the row's own mode and fails
// with that explanation rather than a confusing missing-hint error if the
// denied Integration was configured on the wrong lane.
//
// forced_access_denied is OPTIONAL (EnvBedrockWardynDeniedIntegrationID):
// unset, this half alone skips, named, rather than blocking the whole suite
// on a fixture LL3 never needed. A real ThrottlingException is not exercised
// live: reliably forcing one means exhausting the account's own quota, which
// is the one thing this suite's spend fence exists to avoid.
func TestLive_BedrockWardyn(t *testing.T) {
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

	memberPrincipal, err := bedrockWardynPrincipal(context.Background(), c)
	if err != nil {
		Fatalf(t, "GET /me for the member token: %v", err)
	}

	t.Run("converse_through_wardyn", func(t *testing.T) {
		run := liveBedrockWardynRun(t, c, os.Getenv(EnvBedrockWardynIntegrationID), "live-local LL3w")
		if run.State != types.RunCompleted {
			// run.ID is not a secret (it is how an operator kills a stuck
			// run) — t.Fatalf directly, bypassing Redact's generic GUID mask.
			t.Fatalf("run %s ended %s: %s. If the hint names a missing AWS sign-in, sign in to AWS again from "+
				"Settings as the member and re-run (docs/LIVE-TESTS.md, LL3w)", run.ID, run.State, Redact(run.FailureHint))
		}
		events, err := c.AuditEvents(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("audit events for %s: %v", run.ID, err)
		}
		if err := BedrockWardynRunProvesPerUserSSO(events); err != nil {
			t.Fatalf("run %s: %v", run.ID, err)
		}
		if err := RunCreatedByIsMember(run.CreatedBy, memberPrincipal); err != nil {
			t.Fatalf("run %s: %v", run.ID, err)
		}
		transcript, err := bedrockWardynTranscript(context.Background(), c, run.ID)
		if err != nil {
			t.Fatalf("run %s: recording: %v (GET /api/v1/runs/{id}/recording/{id} needs RecordingStore configured "+
				"on this install; docs/LIVE-TESTS.md, LL3w)", run.ID, err)
		}
		if err := TranscriptContainsReply(transcript, bedrockWardynReply); err != nil {
			t.Fatalf("run %s: %v", run.ID, err)
		}
		t.Logf("run %s COMPLETED: per-user SSO lane, allow-listed model, minted credential, attributed to the "+
			"member, reply in the transcript", run.ID)
	})

	t.Run("forced_access_denied", func(t *testing.T) {
		denied := os.Getenv(EnvBedrockWardynDeniedIntegrationID)
		if denied == "" {
			// The variable NAME alone, never a value — t.Skipf directly, the
			// same reason Require itself bypasses Redact: at 48 characters
			// the name itself matches Redact's long-secret shape and would
			// print as "[long-secret]", hiding which variable to set.
			t.Skipf("live: %s unset — point it at a second Bedrock Integration on the BEARER (API-key) lane, "+
				"bound to a model the capped account's service control policy denies, to prove the "+
				"forced-AccessDenied half of LL3w (docs/LIVE-TESTS.md, LL3w)", EnvBedrockWardynDeniedIntegrationID)
		}
		run := liveBedrockWardynRun(t, c, denied, "live-local LL3w-deny")
		events, err := c.AuditEvents(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("audit events for %s: %v", run.ID, err)
		}
		mode, err := bedrockConfiguredMode(events)
		if err != nil {
			t.Fatalf("run %s: %v", run.ID, err)
		}
		if mode != "bearer" {
			t.Fatalf("run %s: run.bedrock.configure mode is %q, not \"bearer\" — %s must name a Bedrock "+
				"Integration on the bearer (API-key) lane; the per-user AWS SSO lane's bedrock-runtime traffic "+
				"is never MITM'd, so it can never produce this hint (see the test's own doc comment)",
				run.ID, mode, EnvBedrockWardynDeniedIntegrationID)
		}
		if err := BedrockWardynForcedFaultOK(run.FailureHint, "AccessDeniedException"); err != nil {
			t.Fatalf("run %s ended %s: %v", run.ID, run.State, err)
		}
		t.Logf("run %s: AccessDeniedException reached failure_hint on the bearer lane", run.ID)
	})
}

// bedrockWardynPrincipal returns the caller's own principal (token `sub`),
// GET /api/v1/me's "principal" field — the SAME value AgentRun.CreatedBy
// stores for a run this token launches.
func bedrockWardynPrincipal(ctx context.Context, c *client.Client) (string, error) {
	raw, err := c.Me(ctx)
	if err != nil {
		return "", err
	}
	var me struct {
		Principal string `json:"principal"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return "", err
	}
	if me.Principal == "" {
		return "", errors.New("GET /me returned no principal")
	}
	return me.Principal, nil
}

// bedrockConfiguredMode returns the mode field off events' own
// run.bedrock.configure row, or an error naming that none exists.
func bedrockConfiguredMode(events []types.AuditEvent) (string, error) {
	for _, e := range events {
		if e.Action != "run.bedrock.configure" {
			continue
		}
		var d bedrockConfigureData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return "", err
		}
		return d.Mode, nil
	}
	return "", errors.New("no run.bedrock.configure audit row")
}

// bedrockWardynTranscript fetches runID's own (non-interactive, single
// session, castKey defaults to the bare run id) recording, bounded to 256
// KiB — ample for one short prompt/reply pair.
func bedrockWardynTranscript(ctx context.Context, c *client.Client, runID uuid.UUID) ([]byte, error) {
	rc, err := c.GetRecording(ctx, runID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, 256<<10))
}

// liveBedrockWardynRun launches one minimal non-interactive claude-code run
// against integrationID asking for bedrockWardynReply, and waits for a
// terminal state, tolerating a short extra wait for a fault hint that can
// land just after termination (bedrockUpstreamFault's own doc: the decision
// row races the completion watcher). It never asserts the end state itself —
// callers grade that differently for the clean and forced-fault cases. On a
// timeout it kills the run before failing, rather than leave it running.
func liveBedrockWardynRun(t *testing.T, c *client.Client, integrationID, title string) types.AgentRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	created, err := c.CreateRun(ctx, client.CreateRunRequest{
		Agent:         "claude-code",
		Task:          "Reply with the single word " + bedrockWardynReply + " and nothing else.",
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
			if _, kerr := c.KillRun(context.Background(), run.ID); kerr != nil {
				t.Logf("kill %s: %v (may already have exited)", run.ID, kerr)
			}
			t.Fatalf("run %s still %s after 5 minutes; killed it", run.ID, run.State)
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

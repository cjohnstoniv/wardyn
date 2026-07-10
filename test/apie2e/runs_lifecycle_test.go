// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRuns_CreateGetList is the happy-path lifecycle over the SDK: POST /runs
// (201 via CreateRun), then GET /runs/{id} (GetRun) returns the same row, and
// GET /runs (ListRuns) includes it. Headless (no runner): the run is created and
// queryable even with no sandbox driver wired.
func TestRuns_CreateGetList(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	created, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "acme/widgets",
		Task:  "fix issue " + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatalf("created run has nil id")
	}
	if created.Agent != "claude-code" {
		t.Errorf("agent = %q, want claude-code", created.Agent)
	}

	got, err := h.sdk.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetRun id = %s, want %s", got.ID, created.ID)
	}

	runs, err := h.sdk.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if !slices.ContainsFunc(runs, func(r types.AgentRun) bool { return r.ID == created.ID }) {
		t.Errorf("ListRuns does not contain the created run %s", created.ID)
	}
}

// TestRuns_ThreadsConfinement asserts the requested confinement_class is parsed
// and persisted onto the created run (CC3 > the CC2 default-policy minimum).
// This is the SDK/HTTP analogue of internal/api's TestCreateRunThreadsConfinement.
func TestRuns_ThreadsConfinement(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	run, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent:            "claude-code",
		Repo:             "acme/widgets",
		ConfinementClass: "CC3",
	})
	if err != nil {
		t.Fatalf("CreateRun(CC3): %v", err)
	}
	if run.ConfinementClass != types.CC3 {
		t.Errorf("confinement_class = %q, want CC3 (threaded from request)", run.ConfinementClass)
	}
}

// TestRuns_InheritsConfinementWhenUnset asserts an empty confinement_class
// inherits the policy minimum (CC2).
func TestRuns_InheritsConfinementWhenUnset(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	run, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "acme/widgets",
	})
	if err != nil {
		t.Fatalf("CreateRun(unset CC): %v", err)
	}
	if run.ConfinementClass != types.CC2 {
		t.Errorf("confinement_class = %q, want CC2 (inherited policy minimum)", run.ConfinementClass)
	}
}

// TestRuns_RejectsUnknownConfinement asserts an unknown confinement_class fails
// closed with 400 before any store write (fail-closed validation in the handler).
func TestRuns_RejectsUnknownConfinement(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent:            "claude-code",
		Repo:             "acme/widgets",
		ConfinementClass: "CC9",
	})
	assertAPIStatus(t, err, http.StatusBadRequest)
}

// TestRuns_RejectsWeakerConfinement asserts a class WEAKER than the policy
// minimum (default CC2) is refused 422 — a run may only request equal-or-
// stronger confinement, never erode the policy floor.
func TestRuns_RejectsWeakerConfinement(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent:            "claude-code",
		Repo:             "acme/widgets",
		ConfinementClass: "CC1", // weaker than the CC2 default minimum
	})
	assertAPIStatus(t, err, http.StatusUnprocessableEntity)
}

// TestRuns_RejectsUnknownPolicyID asserts referencing a non-existent policy_id
// fails closed with a 4xx (the resolver cannot find the stored policy). Proves
// bad-policy create paths surface a proper client error end-to-end.
func TestRuns_RejectsUnknownPolicyID(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	bogus := uuid.New()
	_, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent:    "claude-code",
		Repo:     "acme/widgets",
		PolicyID: &bogus,
	})
	if err == nil {
		t.Fatalf("CreateRun with unknown policy_id: want a 4xx error, got nil")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *client.APIError, got %T: %v", err, err)
	}
	if apiErr.Status < 400 || apiErr.Status >= 500 {
		t.Fatalf("unknown policy_id status = %d, want a 4xx", apiErr.Status)
	}
}

// TestRuns_RejectsPolicyXorInline asserts supplying BOTH policy_id and
// inline_policy is rejected 400 (mutually exclusive XOR). Confirms the create
// request validation through the SDK body.
func TestRuns_RejectsPolicyXorInline(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	bogus := uuid.New()
	_, err := h.sdk.CreateRun(context.Background(), client.CreateRunRequest{
		Agent:    "claude-code",
		Repo:     "acme/widgets",
		PolicyID: &bogus,
		InlinePolicy: &client.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: "CC2",
		},
	})
	assertAPIStatus(t, err, http.StatusBadRequest)
}

// TestRuns_CompletedTriggersRevocationCascade is the END-TO-END PROOF of the
// COMPLETED run-state fix: when a run reaches its terminal COMPLETED state via
// the fake runner (Wait returns exit 0), the completion watcher must fire
// revokeRunCascade — deny-listing the run's identity so its token can no longer
// be verified. We:
//
//  1. wire a fakeRunner and a never-reap default policy so the idle reaper does
//     not race the completion path;
//  2. create a run WITH a task (so dispatch starts the agent Exec + the
//     completion watcher) over the SDK;
//  3. wait for the server to dispatch it to RUNNING;
//  4. mint a run token for that run id and confirm it Verifies OK (not yet
//     revoked);
//  5. release the runner's Wait so the agent "exits 0";
//  6. assert the run reaches COMPLETED (observed through the SDK GetRun);
//  7. assert the SAME run token NO LONGER verifies — the cascade revoked the
//     run identity. This is the durable, end-to-end evidence that reaching a
//     terminal state runs the kill-switch revocation half.
func TestRuns_CompletedTriggersRevocationCascade(t *testing.T) {
	fr := newFakeRunner()
	fr.waitExit = 0 // exit 0 => COMPLETED
	neverReap := types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		AutoStopAfterSec:    -1, // disable idle reaping so it cannot race completion
	}
	h := newHarness(t, harnessOpts{withRunner: fr, defaultPolicy: &neverReap})
	ctx := context.Background()

	run, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "acme/widgets",
		Task:  "do the thing", // a task => agent Exec + completion watcher
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Wait for the asynchronous dispatch to advance the run to RUNNING.
	if !waitFor(t, 5*time.Second, func() bool {
		got, gerr := h.sdk.GetRun(ctx, run.ID)
		return gerr == nil && got.State == types.RunRunning
	}) {
		cur, _ := h.sdk.GetRun(ctx, run.ID)
		t.Fatalf("run did not reach RUNNING; state=%s", cur.State)
	}

	// Before completion: a run token for this run verifies cleanly (not revoked).
	tok := h.mintRunToken(run.ID)
	if _, verr := h.idp.Verify(ctx, tok, internalAudience); verr != nil {
		t.Fatalf("run token should verify before completion, got: %v", verr)
	}

	// Release the agent process: Wait returns exit 0, the completion watcher wins
	// RUNNING->COMPLETED and runs revokeRunCascade.
	fr.releaseWait()

	// The run reaches COMPLETED (observed through the public SDK).
	if !waitFor(t, 5*time.Second, func() bool {
		got, gerr := h.sdk.GetRun(ctx, run.ID)
		return gerr == nil && got.State == types.RunCompleted
	}) {
		cur, _ := h.sdk.GetRun(ctx, run.ID)
		t.Fatalf("run did not reach COMPLETED; state=%s", cur.State)
	}

	// END-TO-END PROOF: the cascade revoked the run identity, so the SAME token
	// no longer verifies. Poll briefly because the cascade is best-effort and
	// runs just after the terminal transition.
	if !waitFor(t, 5*time.Second, func() bool {
		_, verr := h.idp.Verify(ctx, tok, internalAudience)
		return verr != nil
	}) {
		t.Fatalf("run token still verifies after COMPLETED; revokeRunCascade did not fire")
	}

	// The completion watcher also tears the sandbox down on its terminal win.
	// StopSandbox runs just after the cascade on the detached goroutine, so poll
	// (race-clean read via stopCount) until it lands.
	if !waitFor(t, 5*time.Second, func() bool { return fr.stopCount() > 0 }) {
		t.Errorf("expected the completion watcher to StopSandbox on its terminal win")
	}
}

// TestRuns_GetUnknown_404 asserts an unknown run id is 404 through the SDK.
func TestRuns_GetUnknown_404(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.sdk.GetRun(context.Background(), uuid.New())
	assertAPIStatus(t, err, http.StatusNotFound)
}

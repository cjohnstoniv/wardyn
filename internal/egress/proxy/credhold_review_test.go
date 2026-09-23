// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// ADOPTED VERBATIM from the blind F4 security round (REVIEW-1-security-repro_test.go.txt).
// These two reproduced BLOCKER-1 and are kept unchanged as its pins.
//
// REVIEW SCRATCH (blind F4 security round) — drives the hold through the REAL
// entry point (injector.resolveCtx, the call serveMITMRequest makes) rather
// than resolveInjectionHolding directly, which is what every lane test does.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

const reviewHost = "portal.sso.eu-west-2.amazonaws.com"

func reviewInjector(t *testing.T, is *injectionServer, reader approvalReader) *injector {
	t.Helper()
	tok := &tokenSource{}
	tok.Set("t")
	inj := &injector{
		byHost: map[string]*injEntry{}, base: is.srv.URL, token: tok, client: is.srv.Client(),
		reauth: newReauthCoordinator(), approvals: reader,
	}
	t.Cleanup(inj.reauth.stop) // a workflow outlives its first caller by design

	// A dynamic entry already past its refresh margin: the next resolveCtx re-resolves.
	inj.byHost[reviewHost] = &injEntry{
		grantID: uuid.New(), header: injectedHeader{name: "x-amz-sso_bearer_token", value: "stale"},
		expiresAt: time.Now().Add(-time.Minute).UnixMilli(),
	}
	return inj
}

// Codex #2 as SHIPPED: a second GetRoleCredentials arriving while the leader
// holds must be a ctx-cancellable follower of the leader's workflow (same
// deadline, no new counted workflow). Through resolveCtx it is neither: it
// queues on reMu (uncancellable), and after the leader's budget ends it starts
// a NEW counted workflow with a FRESH full budget.
func TestReview_FollowerThroughResolveCtxQueuesOnReMuAndStartsASecondWorkflow(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shrinkReauthFloor(t, 50*time.Millisecond)
	shortBudget(t, "1ms")                  // clamped UP to the (shrunk) floor
	is := newInjectionServer(t, 1_000_000) // the control plane answers 423 forever
	reader := &fakeApprovalReader{steps: pending(1)}
	inj := reviewInjector(t, is, reader)

	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := inj.resolveCtx(context.Background(), reviewHost)
		leaderDone <- err
	}()
	waitForWorkflowDeadline(t, inj.reauth) // the leader is inside its hold

	// The follower: an SDK that hangs up after 300 ms. Per the contract it
	// must return ~300 ms later with ctx.Err() and open no workflow.
	fctx, fcancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer fcancel()
	start := time.Now()
	_, _, ferr := inj.resolveCtx(fctx, reviewHost)
	felapsed := time.Since(start)
	<-leaderDone

	inj.reauth.mu.Lock()
	counted := inj.reauth.counted
	inj.reauth.mu.Unlock()

	t.Logf("follower returned after %v with err=%v; counted workflows=%d", felapsed.Round(100*time.Millisecond), ferr, counted)
	if felapsed > 2*time.Second {
		t.Errorf("follower took %v — it queued on reMu instead of joining the leader's cancellable wait", felapsed.Round(time.Second))
	}
	if counted != 1 {
		t.Errorf("counted workflows = %d, want 1: the queued caller started a fresh full-budget workflow for the SAME lapse", counted)
	}
}

// The other half of Codex #2: a LIVE follower (an SDK still connected) that
// queued on reMu takes the lock after the leader's budget ends, re-resolves,
// gets the same 423 for the same PENDING row, and opens a SECOND counted
// workflow with a SECOND full budget — N callers, N budgets, one lapse.
func TestReview_QueuedLiveFollowerOpensASecondFullBudgetWorkflow(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	shrinkReauthFloor(t, 200*time.Millisecond)
	shortBudget(t, "1ms") // clamped UP to the (shrunk) floor; two sequential waits below
	is := newInjectionServer(t, 1_000_000)
	reader := &fakeApprovalReader{steps: pending(1)}
	inj := reviewInjector(t, is, reader)

	start := time.Now()
	go func() { _, _, _ = inj.resolveCtx(context.Background(), reviewHost) }()
	waitForWorkflowDeadline(t, inj.reauth)
	_, _, ferr := inj.resolveCtx(context.Background(), reviewHost)
	elapsed := time.Since(start)

	inj.reauth.mu.Lock()
	counted := inj.reauth.counted
	inj.reauth.mu.Unlock()
	t.Logf("follower returned after %v with err=%v; counted workflows=%d", elapsed.Round(time.Millisecond), ferr, counted)
	if counted != 1 {
		t.Errorf("counted workflows = %d, want 1 — the queued caller opened a fresh full-budget hold for the SAME lapse", counted)
	}
	// Relative to the floor: one shared workflow ends near 1x the budget, two
	// sequential budgets near 2x.
	if elapsed > minCredentialReauthTimeout*3/2 {
		t.Errorf("one lapse held callers for %v with a %v budget — two sequential budgets, not one shared workflow", elapsed.Round(time.Millisecond), minCredentialReauthTimeout)
	}
}

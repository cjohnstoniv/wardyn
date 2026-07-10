// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// githubScope mirrors the broker's github_token scope JSON shape so the test can
// author a grant scope and later assert the minted credential carries exactly
// that scope (no widening).
type githubScope struct {
	Repos       []string          `json:"repos"`
	Permissions map[string]string `json:"permissions"`
}

// TestApprovals_ApproveThenMintInGatedTx is the END-TO-END proof of the
// approval-gated mint through REAL code (real broker, real approval FSM service,
// real pg store, real identity) — only the GitHub installation-token minter is
// faked:
//
//  1. seed a RUNNING run + an approval-gated github_token grant;
//  2. call the REAL broker.MintForGrant — with no decided approval it ensures a
//     PENDING approval and returns ErrApprovalPending (mint refused, fail closed);
//  3. find that PENDING approval over the PUBLIC SDK (ListApprovals) and APPROVE
//     it over the SDK (POST /approvals/{id}/approve -> real approval.Decide);
//  4. call broker.MintForGrant AGAIN — now it mints inside the approval-gated
//     transaction (the single FOR UPDATE tx that verifies APPROVED + matching
//     run + single-use + no-widening), returning the github token;
//  5. assert the minted scope == the approved scope (no widening): the minted
//     metadata repos/permissions deep-equal the grant spec scope;
//  6. assert the provable join: approvals.minted_jti == the minted JTI, written
//     in the SAME transaction as the mint.
func TestApprovals_ApproveThenMintInGatedTx(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	runID := uuid.New()
	seedRun(t, h, runID)

	scope := githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
	}
	scopeJSON, _ := json.Marshal(scope)
	grantID := seedGrant(t, h, runID, types.GrantSpec{
		Kind:             types.GrantGitHubToken,
		Scope:            scopeJSON,
		RequiresApproval: true,
		TTLSeconds:       600,
	})

	// (2) Mint with no decision => fail closed with ErrApprovalPending, and a
	// PENDING approval is created.
	caller := h.callerClaims(runID)
	_, err := h.broker.MintForGrant(ctx, caller, grantID)
	var pending broker.ErrApprovalPending
	if !errors.As(err, &pending) {
		t.Fatalf("first mint: want ErrApprovalPending, got %v", err)
	}

	// (3) Discover and approve the PENDING approval over the public SDK.
	approvals, err := h.sdk.ListApprovals(ctx, types.ApprovalPending)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	ap := findApprovalForRun(approvals, runID)
	if ap == nil {
		t.Fatalf("no PENDING approval surfaced for run %s via the SDK", runID)
	}
	if ap.ID != pending.ApprovalID {
		t.Errorf("SDK approval id %s != broker pending id %s", ap.ID, pending.ApprovalID)
	}
	decided, err := h.sdk.Approve(ctx, ap.ID, "looks good")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if decided.State != types.ApprovalApproved {
		t.Fatalf("approval state = %s, want APPROVED", decided.State)
	}

	// (4) Mint again => now it mints in the approval-gated tx.
	minted, err := h.broker.MintForGrant(ctx, caller, grantID)
	if err != nil {
		t.Fatalf("mint after approve: %v", err)
	}
	if minted.Kind != types.GrantGitHubToken {
		t.Errorf("minted kind = %q, want github_token", minted.Kind)
	}
	if minted.Token != "ghs_apie2e" {
		t.Errorf("minted token = %q, want the fake github token", minted.Token)
	}

	// (5) No-widening: the minted credential's scope == the approved scope. The
	// broker surfaces the clamped repos/permissions in Metadata; they must equal
	// the grant spec scope exactly (the approver saw requested_scope == spec).
	assertNoWidening(t, scope, minted)

	// (6) Provable join: approvals.minted_jti was written in the SAME tx.
	var gotJTI string
	if qerr := h.pool.QueryRow(ctx, `SELECT minted_jti FROM approvals WHERE id=$1`, ap.ID).Scan(&gotJTI); qerr != nil {
		t.Fatalf("read minted_jti: %v", qerr)
	}
	if gotJTI != minted.JTI {
		t.Fatalf("approvals.minted_jti = %q, want %q (provable join broken)", gotJTI, minted.JTI)
	}

	// Cleanup the run row (audit_events are append-only and left in place).
	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// TestApprovals_DenyRefusesMint asserts the fail-closed path: DENYING the
// approval over the SDK makes the REAL broker refuse to mint with
// ErrApprovalDenied. The credential is never issued.
func TestApprovals_DenyRefusesMint(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	runID := uuid.New()
	seedRun(t, h, runID)

	scopeJSON, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write"},
	})
	grantID := seedGrant(t, h, runID, types.GrantSpec{
		Kind:             types.GrantGitHubToken,
		Scope:            scopeJSON,
		RequiresApproval: true,
		TTLSeconds:       600,
	})

	caller := h.callerClaims(runID)
	// Create the PENDING approval via the broker's first mint attempt.
	_, err := h.broker.MintForGrant(ctx, caller, grantID)
	var pending broker.ErrApprovalPending
	if !errors.As(err, &pending) {
		t.Fatalf("first mint: want ErrApprovalPending, got %v", err)
	}

	// Deny it over the public SDK.
	if _, derr := h.sdk.Deny(ctx, pending.ApprovalID, "nope"); derr != nil {
		t.Fatalf("Deny: %v", derr)
	}

	// Mint must now be refused (fail closed) with ErrApprovalDenied.
	_, err = h.broker.MintForGrant(ctx, caller, grantID)
	var denied broker.ErrApprovalDenied
	if !errors.As(err, &denied) {
		t.Fatalf("mint after deny: want ErrApprovalDenied, got %v", err)
	}
	if denied.ApprovalID != pending.ApprovalID {
		t.Errorf("denied approval id = %s, want %s", denied.ApprovalID, pending.ApprovalID)
	}

	// No credential was issued: approvals.minted_jti stays empty.
	var gotJTI string
	if qerr := h.pool.QueryRow(ctx, `SELECT minted_jti FROM approvals WHERE id=$1`, pending.ApprovalID).Scan(&gotJTI); qerr != nil {
		t.Fatalf("read minted_jti: %v", qerr)
	}
	if gotJTI != "" {
		t.Errorf("approvals.minted_jti = %q, want empty (deny must never mint)", gotJTI)
	}

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// TestApprovals_AlreadyDecided_409 asserts deciding the same approval twice over
// the SDK is a 409 Conflict (the FSM only transitions from PENDING). The first
// decision wins; the second is rejected end-to-end through real code.
func TestApprovals_AlreadyDecided_409(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	runID := uuid.New()
	seedRun(t, h, runID)
	scopeJSON, _ := json.Marshal(githubScope{Repos: []string{"acme/widgets"}})
	grantID := seedGrant(t, h, runID, types.GrantSpec{
		Kind: types.GrantGitHubToken, Scope: scopeJSON, RequiresApproval: true, TTLSeconds: 600,
	})

	_, err := h.broker.MintForGrant(ctx, h.callerClaims(runID), grantID)
	var pending broker.ErrApprovalPending
	if !errors.As(err, &pending) {
		t.Fatalf("first mint: want ErrApprovalPending, got %v", err)
	}

	if _, aerr := h.sdk.Approve(ctx, pending.ApprovalID, "yes"); aerr != nil {
		t.Fatalf("first Approve: %v", aerr)
	}
	// Second decision (deny) must conflict.
	_, derr := h.sdk.Deny(ctx, pending.ApprovalID, "changed my mind")
	assertAPIStatus(t, derr, 409)

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// ─── seed helpers ─────────────────────────────────────────────────────────────

// seedRun inserts a RUNNING run owned by the harness, using unique ids so tests
// are isolated within the shared DB.
func seedRun(t *testing.T, h *harness, runID uuid.UUID) {
	t.Helper()
	_, err := store.NewPG(h.pool).CreateRun(context.Background(), types.AgentRun{
		ID:               runID,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
		CreatedBy:        "tester",
		Agent:            "claude-code",
		Repo:             "acme/widgets",
		ConfinementClass: types.CC2,
		State:            types.RunRunning,
		SPIFFEID:         "spiffe://" + trustDomain + "/agent-run/" + runID.String(),
		RunnerTarget:     "docker",
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// seedGrant inserts a credential grant for runID and returns its id.
func seedGrant(t *testing.T, h *harness, runID uuid.UUID, spec types.GrantSpec) uuid.UUID {
	t.Helper()
	g, err := store.NewPG(h.pool).CreateGrant(context.Background(), types.CredentialGrant{
		ID:        uuid.New(),
		RunID:     runID,
		CreatedAt: time.Now().UTC(),
		Spec:      spec,
	})
	if err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return g.ID
}

// findApprovalForRun returns the first approval owned by runID, or nil.
func findApprovalForRun(aps []types.ApprovalRequest, runID uuid.UUID) *types.ApprovalRequest {
	for i := range aps {
		if aps[i].RunID == runID {
			return &aps[i]
		}
	}
	return nil
}

// metadataBaselinePerm is the fixed, narrowest read-only permission the broker's
// clampGitHubPermissions ALWAYS injects into every github_token (GitHub requires
// metadata:read for any installation token). It is a hard-coded baseline, NOT a
// widening of the authored scope, so the no-widening assertion permits exactly
// this one extra key and no other.
var metadataBaselinePerm = map[string]string{"metadata": "read"}

// assertNoWidening asserts the minted github credential's effective scope did not
// widen beyond what was authored/approved: same repos, and every minted
// permission is either (a) an authored permission clamped to the
// contents:write + pull_requests:write ceiling, or (b) the fixed metadata:read
// baseline the broker always adds. No permission outside {authored ∪ baseline}
// may appear — that would be true scope-widening.
func assertNoWidening(t *testing.T, authored githubScope, minted broker.Minted) {
	t.Helper()
	var gotRepos []string
	if err := json.Unmarshal([]byte(minted.Metadata["repos"]), &gotRepos); err != nil {
		t.Fatalf("decode minted repos metadata: %v", err)
	}
	if !slices.Equal(slices.Sorted(slices.Values(gotRepos)), slices.Sorted(slices.Values(authored.Repos))) {
		t.Errorf("minted repos = %v, want %v (no widening)", gotRepos, authored.Repos)
	}
	var gotPerms map[string]string
	if err := json.Unmarshal([]byte(minted.Metadata["permissions"]), &gotPerms); err != nil {
		t.Fatalf("decode minted permissions metadata: %v", err)
	}
	for k, v := range gotPerms {
		// The metadata:read baseline is auto-injected by the clamp and is not a
		// widening of the authored scope.
		if base, ok := metadataBaselinePerm[k]; ok && base == v {
			continue
		}
		av, ok := authored.Permissions[k]
		if !ok {
			t.Errorf("minted permission %q=%q was NOT in the approved scope nor the metadata baseline (widening)", k, v)
			continue
		}
		// Authored permission: it must not exceed the authored level (the clamp
		// only narrows, never widens).
		if av != v {
			t.Errorf("minted permission %q=%q != approved %q (clamp must not widen)", k, v, av)
		}
	}
	// Every authored permission within the ceiling must still be present (the
	// approver's intent is honoured, just clamped). contents/pull_requests are
	// the ceiling perms; assert they survived when authored.
	for _, p := range []string{"contents", "pull_requests"} {
		if av, ok := authored.Permissions[p]; ok {
			if gotPerms[p] != av {
				t.Errorf("authored permission %q=%q missing/changed in minted scope: got %q", p, av, gotPerms[p])
			}
		}
	}
}

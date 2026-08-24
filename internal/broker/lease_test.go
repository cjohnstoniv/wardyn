// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leaseFixture wires an APPROVAL-GATED git_pat grant with one already-minted
// approval, so every case below differs only in the approval's stored
// decision_scope — the value the lease turns on.
func leaseFixture(t *testing.T, decScope types.ApprovalScope) (*Broker, *fakeDB, *fakeAudit, uuid.UUID, uuid.UUID) {
	t.Helper()
	b, db, au, _ := newTestBroker(t)
	sec := newMemSecrets()
	sec.m["ado-pat"] = []byte("pat-value-1234567890")
	b.secrets = sec

	runID := uuid.New()
	spec := gitPATSpec("dev.azure.com", "ado-pat", "")
	spec.RequiresApproval = true
	gid := seedGrant(db, runID, spec)
	aid := seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)
	db.approvals[aid].mintedJTI = "jti-from-the-first-mint"
	db.approvals[aid].decScope = decScope
	return b, db, au, runID, gid
}

// TestLease_RunScopedDecisionReMints is B2's per-run credential lease: a git_pat
// whose approval the human scoped to `run` may mint again for the rest of that
// run, instead of raising a fresh approval on every git operation.
func TestLease_RunScopedDecisionReMints(t *testing.T) {
	b, db, _, runID, gid := leaseFixture(t, types.ScopeRun)

	minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("run-scoped re-mint: %v, want a leased mint", err)
	}
	if minted.Token != "pat-value-1234567890" {
		t.Fatalf("leased mint token = %q, want the stored PAT value", minted.Token)
	}
	// The first mint's burn is untouched: a lease re-mints, it does not re-burn.
	for _, a := range db.approvals {
		if a.mintedJTI != "jti-from-the-first-mint" {
			t.Fatalf("minted_jti = %q, want the FIRST mint's jti — a lease must not overwrite the burn", a.mintedJTI)
		}
	}
	// The stream must say a lease widened what one approval authorized (B2's own
	// condition on the feature).
	leaseRows := 0
	for _, r := range db.auditRows {
		if r.action == "credential.mint" && r.outcome == "success" && strings.Contains(r.data, `"lease":true`) {
			leaseRows++
		}
	}
	if leaseRows != 1 {
		t.Fatalf("credential.mint rows marked lease=true: %d, want 1 — a lease mint must be distinguishable from the human's own", leaseRows)
	}
}

// TestLease_NormalizedLegacyDecisionIsNotALease is THE regression for the
// raw-vs-Normalize() comparison.
//
// Every credential approval ever decided carries an EMPTY decision_scope: the
// column's NOT NULL DEFAULT, and until the lease shipped api.decide 400'd any
// explicit scope on a credential approval. types.ApprovalScope.Normalize()
// deliberately maps "" to ScopeRun ("those already meant run-scoped"), which is
// right for egress and catastrophic here — comparing normalized would turn
// EVERY legacy approval in EVERY deployment into a standing re-mint lease on
// upgrade, silently deleting the single-use guarantee those decisions were made
// under. The assertion below pins that: a decision whose Normalize() IS ScopeRun
// must still fail closed with ErrAlreadyMinted.
func TestLease_NormalizedLegacyDecisionIsNotALease(t *testing.T) {
	legacy := types.ApprovalScope("")
	if legacy.Normalize() != types.ScopeRun {
		t.Fatalf("precondition: legacy scope Normalize() = %q, want %q — this test is only meaningful while Normalize() widens the empty value",
			legacy.Normalize(), types.ScopeRun)
	}

	b, _, _, runID, gid := leaseFixture(t, legacy)
	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("legacy empty decision_scope: err=%v, want ErrAlreadyMinted — Normalize()d it reads as `run`, and comparing it that way would lease every shipped approval", err)
	}
}

// TestLease_BoundedByScopeKindAndState pins the other three conditions, each of
// which is a distinct way the lease must NOT open.
func TestLease_BoundedByScopeKindAndState(t *testing.T) {
	// `once` — the human explicitly chose the tightest scope.
	b, _, _, runID, gid := leaseFixture(t, types.ScopeOnce)
	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("once-scoped decision: err=%v, want ErrAlreadyMinted", err)
	}

	// A DIFFERENT KIND with the same run-scoped decision: the lease is git_pat
	// only. github_token is brokered proxy-side and re-minting it from a sandbox
	// is exactly what single-use exists to stop.
	b2, db2, _, _ := newTestBroker(t)
	runID2 := uuid.New()
	ghSpec := githubGrantSpec(t, true)
	gid2 := seedGrant(db2, runID2, ghSpec)
	aid2 := seedApproval(db2, runID2, gid2, ghSpec.Scope, types.ApprovalApproved)
	db2.approvals[aid2].mintedJTI = "jti-1"
	db2.approvals[aid2].decScope = types.ScopeRun
	if _, err := b2.MintForGrant(context.Background(), callerFor(runID2), gid2); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("github_token with a run-scoped decision: err=%v, want ErrAlreadyMinted — the lease is git_pat only", err)
	}

	// SCOPE DRIFT: the grant's scope no longer matches what the human approved.
	// The lease is per-run PER SCOPE, so it must not carry over to a different
	// (host, secret) pairing.
	b3, db3, _, runID3, gid3 := leaseFixture(t, types.ScopeRun)
	for _, a := range db3.approvals {
		a.scope = json.RawMessage(`{"host":"gitlab.com","secret_name":"other-pat"}`)
	}
	if _, err := b3.MintForGrant(context.Background(), callerFor(runID3), gid3); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("scope drift under a run-scoped decision: err=%v, want ErrAlreadyMinted", err)
	}

	// REVOKED RUN: the kill-switch still kills a leased re-mint. This is the
	// property that makes "revoke at run end" true rather than aspirational.
	b4, db4, _, runID4, gid4 := leaseFixture(t, types.ScopeRun)
	db4.revokedRuns[runID4] = true
	if _, err := b4.MintForGrant(context.Background(), callerFor(runID4), gid4); !errors.Is(err, ErrRunRevoked) {
		t.Fatalf("leased re-mint on a revoked run: err=%v, want ErrRunRevoked", err)
	}
}

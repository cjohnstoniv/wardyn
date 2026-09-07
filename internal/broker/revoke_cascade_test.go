// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PINS for F096/F122 (RevokeRun emitted ZERO credential.revoke rows for every
// credential the run auto-minted, and for a leased git_pat's 2nd..Nth mint,
// while THREAT-MODEL.md publishes kill-cascade step 4 as "every minted
// credential for the run") and F013 (one hardcoded GitHub-installation-token
// note on every revoke row, false in both halves for git_pat/ssh_key).
//
// The existing TestRevokeRun_EmitsRevokeAudit seeds an APPROVED approval, i.e.
// exactly the one shape that already worked, which is why neither defect was
// visible to it. These tests take the other shapes.
package broker

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeNoteFor returns the note stamped on the one revoke row carrying jti.
func revokeNoteFor(t *testing.T, au *fakeAudit, jti string) string {
	t.Helper()
	for _, ev := range au.byAction("credential.revoke") {
		var d struct {
			JTI  string `json:"jti"`
			Note string `json:"note"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode revoke data: %v", err)
		}
		if d.JTI == jti {
			return d.Note
		}
	}
	t.Fatalf("no credential.revoke row carries jti %q (rows: %d)", jti, len(au.byAction("credential.revoke")))
	return ""
}

// TestRevokeRun_AutoMintedGrant_EmitsRevokeAudit is F096/F122's shape: a grant
// with RequiresApproval=false mints a REAL credential and creates no approvals
// row at all, so a cascade sourced from approvals.minted_jti saw nothing to
// revoke.
func TestRevokeRun_AutoMintedGrant_EmitsRevokeAudit(t *testing.T) {
	b, db, au, _ := newTestBroker(t)
	runID := uuid.New()
	gid := seedGrant(db, runID, githubGrantSpec(t, false))

	minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("MintForGrant (auto-mint): %v", err)
	}
	if minted.JTI == "" {
		t.Fatal("auto-mint returned no jti")
	}
	// Precondition the finding rests on: no approval row exists for this mint.
	if len(db.approvals) != 0 {
		t.Fatalf("auto-mint created %d approvals rows, want 0 — the fixture is not the shape under test", len(db.approvals))
	}

	if err := b.RevokeRun(context.Background(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	rows := au.byAction("credential.revoke")
	if len(rows) != 1 {
		t.Fatalf("credential.revoke rows for 1 auto-minted credential = %d, want 1. "+
			"The kill cascade is published as \"every minted credential for the run\" (THREAT-MODEL.md), "+
			"but sourcing it from approvals.minted_jti misses every mint that never had an approval row.", len(rows))
	}
	if got := revokeNoteFor(t, au, minted.JTI); got == "" {
		t.Fatal("the revoke row carries no note")
	}
}

// TestRevokeRun_LeasedGitPAT_EmitsOneRevokePerMint is the second half of F122:
// under the B2 per-run lease the minted_jti burn is SKIPPED on re-mints, so
// every jti after the first was invisible to a cascade reading that column.
func TestRevokeRun_LeasedGitPAT_EmitsOneRevokePerMint(t *testing.T) {
	b, db, au, runID, gid := leaseFixture(t, types.ScopeRun)

	first, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("first leased mint: %v", err)
	}
	second, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("second leased mint: %v", err)
	}
	if first.JTI == second.JTI {
		t.Fatal("the two leased mints share a jti; the fixture is not exercising two credentials")
	}
	// The burn still holds the FIRST mint's jti (the lease does not re-burn),
	// which is exactly why the approvals-only cascade under-counted.
	for _, a := range db.approvals {
		if a.mintedJTI != "jti-from-the-first-mint" {
			t.Fatalf("minted_jti = %q, want the pre-seeded first burn", a.mintedJTI)
		}
	}

	if err := b.RevokeRun(context.Background(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	// Three live credentials: the pre-seeded burn plus the two leased mints.
	if n := len(au.byAction("credential.revoke")); n != 3 {
		t.Fatalf("credential.revoke rows = %d, want 3 (the burnt jti + both leased mints)", n)
	}
	revokeNoteFor(t, au, first.JTI)
	revokeNoteFor(t, au, second.JTI)
}

// TestRevokeRun_NotePerGrantKind is F013: the note is the ONLY place a revoke
// row says what actually happens to the credential, and one constant note
// claimed GitHub TTL semantics for an operator-managed PAT that Wardyn can
// neither expire nor down-scope (broker_mint_kinds.go's "honesty ceiling").
func TestRevokeRun_NotePerGrantKind(t *testing.T) {
	t.Run("git_pat says the operator must rotate it", func(t *testing.T) {
		// Every credential in this run is a git_pat: the approval's pre-seeded
		// burn plus the leased mint. NO row may carry the GitHub note.
		b, _, au, runID, gid := leaseFixture(t, types.ScopeRun)
		minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
		if err != nil {
			t.Fatalf("git_pat mint: %v", err)
		}
		if err := b.RevokeRun(context.Background(), runID); err != nil {
			t.Fatalf("RevokeRun: %v", err)
		}
		rows := au.byAction("credential.revoke")
		if len(rows) == 0 {
			t.Fatal("RevokeRun emitted no credential.revoke rows for a git_pat run")
		}
		for _, ev := range rows {
			var d struct{ JTI, Note string }
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatalf("decode revoke data: %v", err)
			}
			if strings.Contains(d.Note, "github installation tokens") {
				t.Fatalf("the revoke row for git_pat jti %q carries the GitHub-installation-token note: %q.\n"+
					"Both halves are false for this kind: the value is a long-lived operator-managed PAT Wardyn cannot "+
					"expire or down-scope, and the identity denylist stops further MINTS, not use of a secret the sandbox "+
					"already holds (broker_mint_kinds.go, \"the honesty ceiling for this grant kind\").", d.JTI, d.Note)
			}
			if !strings.Contains(d.Note, "rotate") {
				t.Fatalf("git_pat revoke note for jti %q = %q, want the operator-rotation story", d.JTI, d.Note)
			}
		}
		revokeNoteFor(t, au, minted.JTI)
	})

	t.Run("github_token keeps its TTL-expiry note", func(t *testing.T) {
		b, db, au, _ := newTestBroker(t)
		runID := uuid.New()
		gid := seedGrant(db, runID, githubGrantSpec(t, false))
		minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
		if err != nil {
			t.Fatalf("github mint: %v", err)
		}
		if err := b.RevokeRun(context.Background(), runID); err != nil {
			t.Fatalf("RevokeRun: %v", err)
		}
		note := revokeNoteFor(t, au, minted.JTI)
		if !strings.Contains(note, "github installation tokens expire") {
			t.Fatalf("github_token revoke note = %q, want the TTL-expiry story preserved", note)
		}
		// F013/B4/B5: GitHub's DELETE /installation/token endpoint is real (it
		// revokes the token you authenticate with), so the note must not say
		// the API is missing; and wardyn DOES hold in-memory copies of the
		// value (the run's mask corpus, broker.go maskReg.Add; the proxy's
		// per-grant re-use cache), so the note must not claim the value is not
		// retained either. The true constraint: nothing here presents the
		// token — RevokeRun holds only the jti.
		if strings.Contains(note, "no per-token revocation") {
			t.Fatalf("github_token revoke note = %q claims GitHub has no per-token revocation API; "+
				"DELETE /installation/token exists (docs.github.com/en/rest/apps/installations)", note)
		}
		if strings.Contains(note, "does not retain the token value") || strings.Contains(note, "never retains") {
			t.Fatalf("github_token revoke note = %q claims the value is not retained; the run's mask corpus "+
				"(broker.go maskReg.Add) holds it until the RunSecretGrace sweep", note)
		}
		if !strings.Contains(note, "does not call GitHub's DELETE /installation/token") || !strings.Contains(note, "holds only the jti") {
			t.Fatalf("github_token revoke note = %q, want the real constraint (wardyn does not call DELETE /installation/token; RevokeRun holds only the jti)", note)
		}
	})
}

// TestPG_RevokeRun_AutoMintedGrant_EmitsRevokeAudit pins the same property on
// the REAL statement (mintedCredentialsSQL) rather than the fake's mirror: an
// auto-mint writes no approvals row, so only the audit half of the UNION can
// find it, and the kind must come back off credential_grants.
func TestPG_RevokeRun_AutoMintedGrant_EmitsRevokeAudit(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: false, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)

	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, &FakeGitHubMinter{Token: "ghs_automint"})
	minted, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if err != nil {
		t.Fatalf("MintForGrant (auto-mint): %v", err)
	}
	if n := pgApprovalsForRun(ctx, t, pool, runID); n != 0 {
		t.Fatalf("auto-mint created %d approvals rows, want 0 — the fixture is not the shape under test", n)
	}

	au := &fakeAudit{}
	b2 := New(NewPgxStore(pool), nil, au, nil, nil)
	if err := b2.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	rows := au.byAction("credential.revoke")
	if len(rows) != 1 {
		t.Fatalf("credential.revoke rows for 1 auto-minted credential = %d, want 1", len(rows))
	}
	var d struct {
		JTI, Kind, Note string
	}
	if err := json.Unmarshal(rows[0].Data, &d); err != nil {
		t.Fatalf("decode revoke data: %v", err)
	}
	if d.JTI != minted.JTI {
		t.Fatalf("revoke row jti = %q, want the minted %q", d.JTI, minted.JTI)
	}
	if d.Kind != string(types.GrantGitHubToken) {
		t.Fatalf("revoke row kind = %q, want github_token (joined off credential_grants)", d.Kind)
	}
	if !strings.Contains(d.Note, "github installation tokens expire") {
		t.Fatalf("revoke note = %q, want the github_token TTL-expiry story", d.Note)
	}
}

// pgApprovalsForRun counts approvals rows for a run — the precondition the
// auto-mint findings rest on.
func pgApprovalsForRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE run_id=$1`, runID).Scan(&n); err != nil {
		t.Fatalf("count approvals: %v", err)
	}
	return n
}

// TestNoStaleMintedJTIsReferences pins the rename this cascade required.
// RevokeRun used to read approvals.minted_jti through a MintedJTIs bulk read;
// sourcing the cascade from what the run ACTUALLY minted replaced that method on
// both TxBeginner and PgxStore with MintedCredentials (mintedCredentialsSQL).
// The method is gone, so any surviving mention of it in this package is a
// comment describing code that no longer exists — which is how
// concurrency_pg_test.go came to tell a reader that TestPG_MintRevokeRoundTrip
// exercises "the real MintedJTIs bulk read" it cannot exercise.
//
// The package's own directory is the whole scope on purpose: MintedJTIs was
// never exported past internal/broker. This file is the one exclusion — it has
// to write the retired name to name it.
func TestNoStaleMintedJTIsReferences(t *testing.T) {
	const self = "revoke_cascade_test.go"
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == self {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "MintedJTIs") {
			t.Errorf("%s still names MintedJTIs, a method this package no longer has; "+
				"say MintedCredentials / mintedCredentialsSQL instead", e.Name())
		}
	}
}

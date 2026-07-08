// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Integration test against a real Postgres. Guarded by WARDYN_TEST_PG (a DSN);
// skipped cleanly when unset. Requires migration 0001_init.sql applied.
func TestIntegration_MintWritesJTIInSameTx(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)

	scope, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
	})
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	specJSON, _ := json.Marshal(spec)

	grantID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO credential_grants (id, run_id, spec) VALUES ($1,$2,$3)`,
		grantID, runID, specJSON); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	approvalID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO approvals (id, run_id, grant_id, kind, requested_scope, state)
		 VALUES ($1,$2,$3,'credential',$4,'APPROVED')`,
		approvalID, runID, grantID, []byte(scope)); err != nil {
		t.Fatalf("insert approval: %v", err)
	}

	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, &FakeGitHubMinter{Token: "ghs_int"})

	minted, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	var gotJTI string
	if err := pool.QueryRow(ctx, `SELECT minted_jti FROM approvals WHERE id=$1`, approvalID).Scan(&gotJTI); err != nil {
		t.Fatalf("read minted_jti: %v", err)
	}
	if gotJTI != minted.JTI {
		t.Fatalf("minted_jti = %q, want %q", gotJTI, minted.JTI)
	}

	// Double-mint blocked at the DB level.
	if _, err := b.MintForGrant(ctx, callerFor(runID), grantID); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("double mint: want ErrAlreadyMinted, got %v", err)
	}

	// RevokeRun finds the minted jti and emits an audit revoke.
	au := &fakeAudit{}
	b2 := New(NewPgxStore(pool), nil, au, nil, nil)
	if err := b2.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if len(au.byAction("credential.revoke")) != 1 {
		t.Fatalf("expected 1 revoke audit, got %d", len(au.byAction("credential.revoke")))
	}

	// Cleanup (audit_events are append-only; leave them).
	_, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

func seedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		 VALUES ($1,'tester','claude-code','acme/widgets','CC2','RUNNING',$2,'docker')`,
		runID, spiffeForRun(runID))
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

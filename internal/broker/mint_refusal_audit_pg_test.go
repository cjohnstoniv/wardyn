// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolAudit is a Recorder that writes through the pool, as the daemon's chain
// does. The in-memory fakeAudit borrows no connection, so it cannot see a mint
// that audits while its own transaction still holds one.
type poolAudit struct{ pool *pgxpool.Pool }

func (a poolAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	return store.InsertAuditEvent(ctx, a.pool, &ev)
}

// burnDuringMint claims the approval on another connection while the mint
// transaction is open, which is the lost single-use race made deterministic: the
// approval row is not locked (FOR UPDATE OF g), so the write lands and the mint's
// conditional minted_jti UPDATE then matches no row.
type burnDuringMint struct {
	*FakeGitHubMinter
	burn func()
}

func (m burnDuringMint) MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	m.burn()
	return m.FakeGitHubMinter.MintInstallationToken(ctx, repos, permissions, ttl)
}

// TestPG_RefusedMint_AuditsOnAPoolOfOne drives every denial and failure arm of
// mint that writes an audit row, on a pool of ONE connection. The row goes
// through the Recorder, which needs a connection of its own, so the mint must
// have given its transaction's connection back first; otherwise the audit waits
// on the mint that is waiting on it. Each arm must return its error inside the
// deadline and leave exactly one credential.mint row.
func TestPG_RefusedMint_AuditsOnAPoolOfOne(t *testing.T) {
	wide := pgPool(t)
	cfg, err := pgxpool.ParseConfig(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool of one: %v", err)
	}
	t.Cleanup(one.Close)

	scope := pgGithubScope(t)
	widened, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets", "acme/secrets"},
		Permissions: map[string]string{"contents": "write"},
	})
	gated := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	errMint := errors.New("github is down")

	type arm struct {
		name    string
		spec    types.GrantSpec
		outcome string
		wantErr error
		// direct calls mint itself: MintForGrant refuses a cloud_sts grant, and
		// creates the missing approval, before either arm inside mint is reached.
		direct bool
		// setup seeds what the arm needs and may replace the minter.
		setup func(ctx context.Context, t *testing.T, runID, grantID uuid.UUID, gh *GitHubMinter)
	}
	arms := []arm{
		{
			name: "cloud sts needs SPIRE", outcome: "denied", wantErr: ErrRequiresSPIRE, direct: true,
			spec: types.GrantSpec{Kind: types.GrantCloudSTS, Scope: json.RawMessage(`{}`), TTLSeconds: 600},
		},
		{name: "approval required, none on file", spec: gated, outcome: "denied", wantErr: ErrNotApproved, direct: true},
		{
			name: "scope mismatch", spec: gated, outcome: "denied", wantErr: ErrScopeMismatch,
			setup: func(ctx context.Context, t *testing.T, runID, grantID uuid.UUID, _ *GitHubMinter) {
				pgSeedApproval(ctx, t, wide, runID, grantID, widened)
			},
		},
		{
			name: "revoked run", spec: gated, outcome: "denied", wantErr: ErrRunRevoked,
			setup: func(ctx context.Context, t *testing.T, runID, grantID uuid.UUID, _ *GitHubMinter) {
				pgSeedApproval(ctx, t, wide, runID, grantID, scope)
				if _, err := wide.Exec(ctx,
					`INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`,
					"run:"+runID.String(), runID); err != nil {
					t.Fatalf("seed revocation: %v", err)
				}
			},
		},
		{
			name: "mint failure", spec: gated, outcome: "failure", wantErr: errMint,
			setup: func(ctx context.Context, t *testing.T, runID, grantID uuid.UUID, gh *GitHubMinter) {
				pgSeedApproval(ctx, t, wide, runID, grantID, scope)
				*gh = &FakeGitHubMinter{Err: errMint}
			},
		},
		{
			name: "already minted", spec: gated, outcome: "denied", wantErr: ErrAlreadyMinted,
			setup: func(ctx context.Context, t *testing.T, runID, grantID uuid.UUID, gh *GitHubMinter) {
				approvalID := pgSeedApproval(ctx, t, wide, runID, grantID, scope)
				*gh = burnDuringMint{FakeGitHubMinter: &FakeGitHubMinter{Token: "ghs_lost_the_race"}, burn: func() {
					if _, err := wide.Exec(ctx,
						`UPDATE approvals SET minted_jti = 'claimed-elsewhere' WHERE id = $1`, approvalID); err != nil {
						t.Errorf("claim approval: %v", err)
					}
				}}
			},
		},
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			bg := context.Background()
			runID := uuid.New()
			seedRun(bg, t, wide, runID)
			defer func() { _, _ = wide.Exec(bg, `DELETE FROM agent_runs WHERE id=$1`, runID) }()
			defer func() { _, _ = wide.Exec(bg, `DELETE FROM identity_revocations WHERE run_id=$1`, runID) }()
			grantID := pgSeedGrant(bg, t, wide, runID, a.spec)

			var gh GitHubMinter = &FakeGitHubMinter{Token: "ghs_should_not_mint"}
			if a.setup != nil {
				a.setup(bg, t, runID, grantID, &gh)
			}
			b := New(NewPgxStore(one), nil, poolAudit{one}, nil, gh)

			ctx, cancel := context.WithTimeout(bg, 5*time.Second)
			defer cancel()
			var err error
			if a.direct {
				_, err = b.mint(ctx, callerFor(runID), grantID, uuid.Nil)
			} else {
				_, err = b.MintForGrant(ctx, callerFor(runID), grantID)
			}
			if !errors.Is(err, a.wantErr) {
				t.Fatalf("want %v, got %v", a.wantErr, err)
			}
			if ctx.Err() != nil {
				t.Fatalf("the refused mint held its connection until the 5s deadline: %v", ctx.Err())
			}
			if n := countMintAudits(bg, t, wide, runID, a.outcome); n != 1 {
				t.Fatalf("%s credential.mint rows = %d, want 1", a.outcome, n)
			}
			if n := countMintAudits(bg, t, wide, runID, "success"); n != 0 {
				t.Fatalf("success credential.mint rows = %d, want 0 for a refused mint", n)
			}
		})
	}
}

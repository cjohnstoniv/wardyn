// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Concurrency + no-widening integration tests against a REAL Postgres, run with
// -race. These COMPLEMENT integration_test.go (which proves the same-tx
// minted_jti write happy path) and broker_test.go (which proves the mint logic
// against an in-memory fake): the HIGH test-gap they close is that single-use /
// no-widening was only ever exercised against the fake DB — the actual
// SELECT ... FOR UPDATE serialization at the Postgres level was untested.
//
// Guarded by WARDYN_TEST_PG (a DSN); skipped cleanly when unset, so plain CI is
// unaffected. When set, these tests MUST run and pass against the live pool.
// Every test uses fresh uuids so it is isolated within the shared DB and only
// asserts on rows it created (an empty DB is never assumed).
//
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55432/wardyn_broker?sslmode=disable" \
//        go test -race ./internal/broker/...

// pgPool connects to WARDYN_TEST_PG, applies migrations (idempotent), and returns
// a pool registered for cleanup. Skips the test cleanly when the DSN is unset —
// the only sanctioned skip (genuinely absent substrate). Mirrors the api
// pgHarness convention of db.Connect -> db.Migrate.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres concurrency integration test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgSeedGrant inserts a credential_grants row for runID with the given spec and
// returns its id. The caller owns the run (seedRun, from integration_test.go).
func pgSeedGrant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, spec types.GrantSpec) uuid.UUID {
	t.Helper()
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	grantID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO credential_grants (id, run_id, spec) VALUES ($1,$2,$3)`,
		grantID, runID, specJSON); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	return grantID
}

// pgSeedApproval inserts an APPROVED credential approval whose requested_scope
// is exactly scope (the approver-saw scope) and returns its id.
func pgSeedApproval(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID, grantID uuid.UUID, scope json.RawMessage) uuid.UUID {
	t.Helper()
	approvalID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO approvals (id, run_id, grant_id, kind, requested_scope, state)
		 VALUES ($1,$2,$3,'credential',$4,'APPROVED')`,
		approvalID, runID, grantID, []byte(scope)); err != nil {
		t.Fatalf("insert approval: %v", err)
	}
	return approvalID
}

// pgGithubScope builds a canonical github_token scope JSON for the happy path.
func pgGithubScope(t *testing.T) json.RawMessage {
	t.Helper()
	scope, err := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
	})
	if err != nil {
		t.Fatalf("marshal github scope: %v", err)
	}
	return scope
}

// readMintedJTI reads approvals.minted_jti for the given approval id.
func readMintedJTI(ctx context.Context, t *testing.T, pool *pgxpool.Pool, approvalID uuid.UUID) string {
	t.Helper()
	var jti string
	if err := pool.QueryRow(ctx,
		`SELECT minted_jti FROM approvals WHERE id=$1`, approvalID).Scan(&jti); err != nil {
		t.Fatalf("read minted_jti: %v", err)
	}
	return jti
}

// TestPG_MintRevokeRoundTrip exercises mint -> persist -> revoke against the
// real PgxStore: a single approved github_token grant mints once (writing
// minted_jti in the same tx, the provable join), the persisted jti matches the
// returned one, the no-widening scope is honored, and RevokeRun finds the
// minted jti through the real MintedJTIs bulk read and emits one audit revoke.
func TestPG_MintRevokeRoundTrip(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, scope)

	au := &fakeAudit{}
	b := New(NewPgxStore(pool), nil, au, nil, &FakeGitHubMinter{Token: "ghs_roundtrip"})

	minted, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	if minted.Token != "ghs_roundtrip" {
		t.Fatalf("token = %q, want ghs_roundtrip", minted.Token)
	}
	if minted.JTI == "" {
		t.Fatal("expected non-empty jti")
	}
	// INVARIANT 2: the mint wrote minted_jti back in the SAME tx that saw APPROVED.
	if got := readMintedJTI(ctx, t, pool, approvalID); got != minted.JTI {
		t.Fatalf("persisted minted_jti = %q, want %q (same-tx write)", got, minted.JTI)
	}
	// No-widening: clamped perms include metadata:read and the minted approval id
	// is the one we approved.
	if minted.ApprovalID != approvalID {
		t.Fatalf("minted approval id = %s, want %s", minted.ApprovalID, approvalID)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "success" {
		t.Fatalf("expected 1 successful credential.mint, got %+v", mints)
	}

	// Revoke through the real PgxStore.MintedJTIs bulk read.
	au2 := &fakeAudit{}
	b2 := New(NewPgxStore(pool), nil, au2, nil, nil)
	if err := b2.RevokeRun(ctx, runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if n := len(au2.byAction("credential.revoke")); n != 1 {
		t.Fatalf("expected 1 credential.revoke for the one minted jti, got %d", n)
	}
}

// TestPG_RevokedRun_RefusesMint proves the kill-switch/mint coupling against a
// real Postgres: once a row exists in identity_revocations for the run, the in-tx
// runRevoked check refuses an otherwise-APPROVED mint with ErrRunRevoked, writes
// no minted_jti, and never calls the github minter.
func TestPG_RevokedRun_RefusesMint(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM identity_revocations WHERE run_id=$1`, runID) }()

	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, scope)

	// Kill-switch cascade recorded the revocation (run-scoped deny-list entry).
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`,
		"run:"+runID.String(), runID); err != nil {
		t.Fatalf("seed revocation: %v", err)
	}

	gh := &FakeGitHubMinter{Token: "ghs_should_not_mint"}
	_, err := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, gh).
		MintForGrant(ctx, callerFor(runID), grantID)
	if !errors.Is(err, ErrRunRevoked) {
		t.Fatalf("want ErrRunRevoked, got %v", err)
	}
	if gh.Calls != 0 {
		t.Fatalf("github minter called %d times, want 0 (revoked run must not mint)", gh.Calls)
	}
	if got := readMintedJTI(ctx, t, pool, approvalID); got != "" {
		t.Fatalf("minted_jti = %q, want empty (no mint on revoked run)", got)
	}
}

// TestPG_NoWidening_ScopeMismatch proves the no-widening invariant at the DB
// level: when approvals.requested_scope (what the approver saw) does NOT
// deep-equal the grant spec scope, the in-tx mint is refused with
// ErrScopeMismatch, no minted_jti is written, and the FakeGitHubMinter is never
// called (no token is ever produced for a widened scope).
func TestPG_NoWidening_ScopeMismatch(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	// Grant scope: one repo. Approval scope: TWO repos (a widening attempt).
	grantScope, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write"},
	})
	approvedScope, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets", "acme/secrets"},
		Permissions: map[string]string{"contents": "write"},
	})
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: grantScope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, approvedScope)

	gh := &FakeGitHubMinter{Token: "ghs_should_not_mint"}
	au := &fakeAudit{}
	b := New(NewPgxStore(pool), nil, au, nil, gh)

	_, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("want ErrScopeMismatch, got %v", err)
	}
	// Fail closed: no token minted, no jti persisted.
	if gh.Calls != 0 {
		t.Fatalf("github minter called %d times, want 0 (scope mismatch must not mint)", gh.Calls)
	}
	if got := readMintedJTI(ctx, t, pool, approvalID); got != "" {
		t.Fatalf("minted_jti = %q, want empty (no mint on widening)", got)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "denied" {
		t.Fatalf("expected 1 denied credential.mint, got %+v", mints)
	}
}

// TestPG_ConcurrentMint_ExactlyOnceWins is the core new coverage. N goroutines
// fire MintForGrant for the SAME approved grant concurrently against the real
// pool. The single-use minted_jti guard is enforced by the SELECT ... FOR
// UPDATE transaction at the DB level (not just the fake): EXACTLY ONE attempt
// must succeed and the remaining N-1 must fail with ErrAlreadyMinted. We assert
// exactly-once on the in-memory results AND on the persisted row (one stable
// minted_jti equal to the winner's), and that the github minter ran exactly once.
func TestPG_ConcurrentMint_ExactlyOnceWins(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, scope)

	// One shared FakeGitHubMinter (thread-safe via its mutex) so we can assert
	// the kind-specific minter fired exactly once across all goroutines.
	gh := &FakeGitHubMinter{Token: "ghs_concurrent"}
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, gh)

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		already int
		other   []error
		winJTI  string
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			minted, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				winJTI = minted.JTI
			case errors.Is(err, ErrAlreadyMinted):
				already++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("unexpected errors from concurrent mint: %v", other)
	}
	// EXACTLY ONE winner; the rest are single-use rejections.
	if wins != 1 {
		t.Fatalf("concurrent mint winners = %d, want exactly 1 (single-use minted_jti)", wins)
	}
	if already != n-1 {
		t.Fatalf("ErrAlreadyMinted count = %d, want %d", already, n-1)
	}
	// The persisted minted_jti is the winner's, written once and stable.
	if got := readMintedJTI(ctx, t, pool, approvalID); got != winJTI {
		t.Fatalf("persisted minted_jti = %q, want winner %q", got, winJTI)
	}
	// The winner minted. Losing goroutines that BLOCKED on the FOR UPDATE OF g
	// lock resume on their original snapshot, read a stale minted_jti='' from the
	// non-locked (nullable-side) approval row, pass the row.mintedJTI fast path,
	// and can legitimately CALL the minter before their conditional-UPDATE
	// rows-affected check returns 0 and fails them closed (the ErrAlreadyMinted
	// counted above). That throwaway token is discarded, never returned.
	// Exactly-once applies to RETURNED credentials (wins==1 above), NOT to minter
	// invocations — identical to the sibling TestPG_ConcurrentMintOnApproval_
	// ExactlyOnce, whose mint() path (and FOR UPDATE OF g lock) this shares;
	// MintForGrant's extra loadGrant/ensureApproval round-trips only add timing
	// jitter. Asserting ==1 here was a timing-luck flake that fails on both pg16
	// and pg17.
	if gh.Calls < 1 {
		t.Fatalf("github minter calls = %d, want >= 1 (winner must mint)", gh.Calls)
	}
}

// TestPG_ConcurrentMint_AutoApprovalGrant_Independent is a control alongside the
// exactly-once test: an AUTO-mintable grant (RequiresApproval=false) has no
// single-use approval row, so it is re-mintable BY DESIGN. We fire N concurrent
// auto-mints for the same grant against the real pool and assert ALL succeed
// (the grant-row FOR UPDATE serializes them but does not block re-mint), each
// producing a distinct jti. This guards against a regression that would wrongly
// extend single-use semantics to the auto-mint path.
func TestPG_ConcurrentMint_AutoApprovalGrant_Independent(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	// api_key auto-mint: no approval row, secret never returned. Re-mintable.
	scope, _ := json.Marshal(apiKeyScope{
		Host:       "api.example.com",
		Header:     "Authorization",
		Format:     "Bearer %s",
		SecretName: "example-key",
	})
	spec := types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, RequiresApproval: false, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)

	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, nil)

	const n = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		ok   int
		errs []error
		jtis = map[string]struct{}{}
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			minted, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ok++
			jtis[minted.JTI] = struct{}{}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("auto-mint concurrent errors: %v", errs)
	}
	if ok != n {
		t.Fatalf("auto-mint successes = %d, want %d (re-mintable by design)", ok, n)
	}
	if len(jtis) != n {
		t.Fatalf("distinct jtis = %d, want %d (each mint a fresh jti)", len(jtis), n)
	}
}

// TestPG_ConcurrentMintOnApproval_ExactlyOnce is the tightest single-use race:
// MintOnApproval skips MintForGrant's loadGrant/ensureApproval round-trips and
// goes STRAIGHT to the FOR UPDATE mint transaction, so all N goroutines contend
// on selectGrantApprovalForUpdate near-simultaneously — the worst case for the
// single-use guard. It asserts exactly one win, N-1 ErrAlreadyMinted, and one
// stable persisted jti equal to the winner's.
//
// This guards a REAL concurrent double-mint (audit finding "F1", a true
// positive proven by a two-session PG16 experiment): a contender that BLOCKS on
// the FOR UPDATE OF g lock mid-tx resumes on its original statement snapshot,
// reads a stale minted_jti='' from the joined (non-locked, nullable-side)
// approval row — EvalPlanQual re-checks only the locked g tuple, never re-fetches
// the approval — so it PASSES the row.mintedJTI fast-path guard and mints a real
// token. Only the rows-affected check on the conditional minted_jti UPDATE (which
// returns 0 rows for that loser) stops the second credential from being returned.
// With the rows-affected check removed this test yields multiple winners.
func TestPG_ConcurrentMintOnApproval_ExactlyOnce(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID) }()

	scope := pgGithubScope(t)
	spec := types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, scope)

	gh := &FakeGitHubMinter{Token: "ghs_moa"}
	b := New(NewPgxStore(pool), nil, &fakeAudit{}, nil, gh)

	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    int
		already int
		other   []error
		winJTI  string
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together to maximize contention on the mint tx
			minted, err := b.MintOnApproval(ctx, runID, grantID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				winJTI = minted.JTI
			case errors.Is(err, ErrAlreadyMinted):
				already++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("unexpected errors from concurrent MintOnApproval: %v", other)
	}
	if wins != 1 {
		t.Fatalf("MintOnApproval winners = %d, want exactly 1 (single-use minted_jti)", wins)
	}
	if already != n-1 {
		t.Fatalf("ErrAlreadyMinted = %d, want %d", already, n-1)
	}
	if got := readMintedJTI(ctx, t, pool, approvalID); got != winJTI {
		t.Fatalf("persisted minted_jti = %q, want winner %q", got, winJTI)
	}
	// gh.Calls >= 1, not == 1: a lock-blocked stale-snapshot waiter legitimately
	// passes the fast path and CALLS the minter before its rows-affected check
	// returns 0 and fails it closed (that throwaway token is discarded, never
	// returned). Exactly-once applies to RETURNED credentials (wins==1 above), not
	// to minter invocations. This is the very interleaving the fix defends.
	if gh.Calls < 1 {
		t.Fatalf("github minter calls = %d, want >= 1 (winner must mint)", gh.Calls)
	}
}

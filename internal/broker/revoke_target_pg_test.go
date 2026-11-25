// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sealFixture is one throwaway database with a run, its masking manifest (unless noManifest), an auto-mintable
// github_token grant and a broker whose token seal uses a real subject-key manager.
type sealFixture struct {
	pool    *pgxpool.Pool
	keys    *subjectkey.Manager
	seal    *TokenSeal
	gh      *FakeGitHubMinter
	b       *Broker
	runID   uuid.UUID
	owner   string
	grantID uuid.UUID
}

func newSealFixture(t *testing.T, manifest bool) *sealFixture {
	t.Helper()
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	f := &sealFixture{pool: pool, keys: subjectkeytest.Manager(pool, k), runID: uuid.New(),
		owner: "person-" + uuid.NewString() + "@example.com", gh: &FakeGitHubMinter{Token: "ghs_seal_" + strings.ReplaceAll(uuid.NewString(), "-", "")}}
	ctx := t.Context()
	seedRun(ctx, t, pool, f.runID)
	if manifest {
		if _, err := pool.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, $2)`, f.runID, f.owner); err != nil {
			t.Fatal(err)
		}
	}
	f.grantID = f.addGrant(t)
	f.seal = NewTokenSeal(pool, f.keys)
	f.b = New(NewPgxStore(pool), nil, poolAudit{pool}, nil, f.gh).WithTokenSeal(f.seal)
	return f
}

func (f *sealFixture) addGrant(t *testing.T) uuid.UUID {
	t.Helper()
	return pgSeedGrant(t.Context(), t, f.pool, f.runID, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: pgGithubScope(t), TTLSeconds: 600})
}

// sealRows counts the run's rows, tombstones included.
func (f *sealFixture) sealRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM run_github_token_seals WHERE run_id = $1`, f.runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditRows is the data of every row of action for the run, in time order.
func auditRows(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, action string) []map[string]any {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT outcome, data FROM audit_events WHERE run_id = $1 AND action = $2 ORDER BY seq`, runID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var outcome string
		var data []byte
		if err := rows.Scan(&outcome, &data); err != nil {
			t.Fatal(err)
		}
		d := map[string]any{}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &d); err != nil {
				t.Fatal(err)
			}
		}
		d["_outcome"] = outcome
		out = append(out, d)
	}
	return out
}

// assertNoTokenInAudit fails when token appears in any audit row of the run.
func assertNoTokenInAudit(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, token string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE run_id = $1 AND data::text LIKE '%' || $2 || '%'`, runID, token).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d audit rows of the run carry the token value", n)
	}
}

// captureLog sends slog's default logger to a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func revokedTokens(f *FakeGitHubMinter) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.RevokedTokens...)
}

// hookKeys replaces the key service's Current.
type hookKeys struct {
	maskmanifest.Keys
	current func(ctx context.Context, owner, purpose string) (int, []byte, error)
}

func (h hookKeys) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	return h.current(ctx, owner, purpose)
}

// A seal that fails rolls the mint back and hands the discarded token back to GitHub.
func TestPG_MintGitHub_SealFailureRollsBackAndRevokes(t *testing.T) {
	f := newSealFixture(t, true)
	errKeysDown := errors.New("key service is down")
	f.b.WithTokenSeal(NewTokenSeal(f.pool, hookKeys{Keys: f.keys, current: func(context.Context, string, string) (int, []byte, error) {
		return 0, nil, errKeysDown
	}}))
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if !errors.Is(err, errKeysDown) || got.Token != "" {
		t.Fatalf("mint with the key service down = %+v, %v; want %v and no token", got, err, errKeysDown)
	}
	if r := revokedTokens(f.gh); len(r) != 1 || r[0] != f.gh.Token {
		t.Fatalf("revoked = %v, want the discarded token", r)
	}
	if n := countMintAudits(t.Context(), t, f.pool, f.runID, "success"); n != 0 {
		t.Fatalf("success credential.mint rows = %d, want 0", n)
	}
	if n := f.sealRows(t); n != 0 {
		t.Fatalf("seal rows = %d, want 0", n)
	}
}

// The row is sealed under the run's masking-manifest owner, which differs from created_by, and opens to the token.
func TestPG_MintGitHub_SealOwnerIsManifestOwner(t *testing.T) {
	f := newSealFixture(t, true)
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if err != nil {
		t.Fatal(err)
	}
	var owner, createdBy string
	var version int
	var sealed []byte
	var expires time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT s.owner, a.created_by, s.key_version, s.sealed, s.expires_at
		FROM run_github_token_seals s JOIN agent_runs a ON a.id = s.run_id WHERE s.run_id = $1 AND s.jti = $2`, f.runID, got.JTI).
		Scan(&owner, &createdBy, &version, &sealed, &expires); err != nil {
		t.Fatalf("read the seal row: %v", err)
	}
	if owner != f.owner || owner == createdBy {
		t.Fatalf("seal owner = %q (created_by %q), want the manifest owner %q", owner, createdBy, f.owner)
	}
	if bytes.Contains(sealed, []byte(got.Token)) {
		t.Fatal("the row carries the token in clear")
	}
	if d := expires.Sub(got.ExpiresAt); d > time.Second || d < -time.Second {
		t.Fatalf("expires_at = %s, want the token's %s", expires, got.ExpiresAt)
	}
	key, err := f.keys.Key(t.Context(), owner, subjectkey.PurposeCred, version)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := kek.Open(key, sealed, tokenSealAAD(f.runID, got.JTI, version))
	if err != nil || string(plain) != got.Token {
		t.Fatalf("open = %q, %v; want the minted token", plain, err)
	}
	if got.seal != nil {
		t.Fatal("the returned Minted still carries its prepared seal")
	}
}

// A run with no masking manifest mints as before, keeps no copy, and its revoke row says none.
func TestPG_MintGitHub_NoManifestNoSeal(t *testing.T) {
	f := newSealFixture(t, false)
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if err != nil || got.Token != f.gh.Token {
		t.Fatalf("mint = %+v, %v", got, err)
	}
	if n := f.sealRows(t); n != 0 {
		t.Fatalf("seal rows = %d, want 0", n)
	}
	if err := f.b.RevokeRun(t.Context(), f.runID); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, f.pool, f.runID, "credential.revoke")
	if len(rows) != 1 || rows[0]["target_revoke"] != targetRevokeNone {
		t.Fatalf("credential.revoke rows = %v, want one with target_revoke none", rows)
	}
}

// The key read runs before the mint transaction, so no mint row lock is held while it waits.
func TestPG_MintGitHub_KeyReadHoldsNoRowLock(t *testing.T) {
	f := newSealFixture(t, true)
	entered, release := make(chan struct{}), make(chan struct{})
	f.b.WithTokenSeal(NewTokenSeal(f.pool, hookKeys{Keys: f.keys, current: func(ctx context.Context, owner, purpose string) (int, []byte, error) {
		close(entered)
		<-release
		return f.keys.Current(ctx, owner, purpose)
	}}))
	done := make(chan error, 1)
	go func() {
		_, err := f.b.MintForGrant(context.WithoutCancel(t.Context()), callerFor(f.runID), f.grantID)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the mint never reached the key read")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT 1 FROM credential_grants WHERE id = $1 FOR UPDATE NOWAIT`,
		`SELECT 1 FROM run_mask_manifest WHERE run_id = $1 FOR UPDATE NOWAIT`,
	} {
		arg := f.grantID
		if strings.Contains(q, "run_mask_manifest") {
			arg = f.runID
		}
		var one int
		if err := tx.QueryRow(ctx, q, arg).Scan(&one); err != nil {
			_ = tx.Rollback(ctx)
			close(release)
			t.Fatalf("%s while the key read waits: %v (a mint row lock is held)", q, err)
		}
	}
	_ = tx.Rollback(ctx)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("mint after the key read: %v", err)
	}
	if n := f.sealRows(t); n != 1 {
		t.Fatalf("seal rows = %d, want 1", n)
	}
}

// Four slow hand-backs neither delay RevokeRun nor lose a credential.revoke row; the worker then audits each.
func TestPG_RevokeRun_FourSlowGitHubDeletesDoNotDelayItOrLoseARow(t *testing.T) {
	logs := captureLog(t)
	f := newSealFixture(t, true)
	var tokens []string
	for i := range 4 {
		gid := f.grantID
		if i > 0 {
			gid = f.addGrant(t)
		}
		f.gh.Token = "ghs_slow_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), gid)
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		tokens = append(tokens, got.Token)
	}
	f.b.github = slowMinter{FakeGitHubMinter: f.gh, hold: refRevokeTimeout + 500*time.Millisecond}
	wctx, stop := context.WithCancel(context.Background())
	worker := make(chan struct{})
	go func() { f.b.RunGitHubTokenRevokes(wctx); close(worker) }()
	defer func() { stop(); <-worker }()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := f.b.RevokeRun(ctx, f.runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("RevokeRun took %s with four slow hand-backs queued; want under 1s", d)
	}
	rows := auditRows(t, f.pool, f.runID, "credential.revoke")
	if len(rows) != 4 {
		t.Fatalf("credential.revoke rows = %d, want 4", len(rows))
	}
	for _, r := range rows {
		if r["target_revoke"] != targetRevokeQueued {
			t.Fatalf("credential.revoke row %v, want target_revoke queued", r)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	var failures []map[string]any
	for time.Now().Before(deadline) {
		failures = auditRows(t, f.pool, f.runID, "github_token.revoke")
		if len(failures) >= 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(failures) != 4 {
		t.Fatalf("github_token.revoke rows = %d, want 4", len(failures))
	}
	for _, r := range failures {
		if r["_outcome"] != "failure" || r["retry_at"] == nil || r["target_outcome"] != targetFailed {
			t.Fatalf("github_token.revoke row %v, want a failure with retry_at", r)
		}
	}
	for _, tok := range tokens {
		assertNoTokenInAudit(t, f.pool, f.runID, tok)
		if strings.Contains(logs.String(), tok) {
			t.Fatal("a log line carries a token value")
		}
	}
}

// slowMinter holds every hand-back past refRevokeTimeout, then fails it, as a blackholed GitHub does.
type slowMinter struct {
	*FakeGitHubMinter
	hold time.Duration
}

func (s slowMinter) Revoke(_ context.Context, _ string) error {
	time.Sleep(s.hold)
	return context.DeadlineExceeded
}

// A fenced manifest refuses the seal at prepare: the mint fails and the token is handed back.
func TestPG_TokenSeal_FenceRefusesLateSeal(t *testing.T) {
	f := newSealFixture(t, true)
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_mask_manifest SET fenced_at = now() WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID); !errors.Is(err, maskmanifest.ErrFenced) {
		t.Fatalf("mint on a fenced manifest = %v, want ErrFenced", err)
	}
	if n := f.sealRows(t); n != 0 {
		t.Fatalf("seal rows = %d, want 0", n)
	}
	if r := revokedTokens(f.gh); len(r) != 1 {
		t.Fatalf("revoked = %v, want the discarded token", r)
	}
}

// A fence that lands between prepare and the mint transaction rolls the mint back.
func TestPG_TokenSeal_FenceAfterPrepareRollsBack(t *testing.T) {
	f := newSealFixture(t, true)
	f.b.WithTokenSeal(NewTokenSeal(f.pool, hookKeys{Keys: f.keys, current: func(ctx context.Context, owner, purpose string) (int, []byte, error) {
		v, k, err := f.keys.Current(ctx, owner, purpose)
		if _, ferr := f.pool.Exec(ctx, `UPDATE run_mask_manifest SET fenced_at = now() WHERE run_id = $1`, f.runID); ferr != nil {
			t.Error(ferr)
		}
		return v, k, err
	}}))
	if _, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID); !errors.Is(err, maskmanifest.ErrFenced) {
		t.Fatalf("mint fenced after prepare = %v, want ErrFenced", err)
	}
	if n := countMintAudits(t.Context(), t, f.pool, f.runID, "success"); n != 0 {
		t.Fatalf("success credential.mint rows = %d, want 0", n)
	}
	if n := f.sealRows(t); n != 0 {
		t.Fatalf("seal rows = %d, want 0", n)
	}
	if r := revokedTokens(f.gh); len(r) != 1 {
		t.Fatalf("revoked = %v, want the discarded token", r)
	}
}

// A seal store that cannot be read writes no credential.revoke row on that pass and returns the error.
func TestPG_RevokeRun_SealReadErrorWritesNoRows(t *testing.T) {
	f := newSealFixture(t, true)
	if _, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID); err != nil {
		t.Fatal(err)
	}
	closed, err := pgxpool.NewWithConfig(t.Context(), f.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	b := New(NewPgxStore(f.pool), nil, poolAudit{f.pool}, nil, f.gh).WithTokenSeal(NewTokenSeal(closed, f.keys))
	if err := b.RevokeRun(t.Context(), f.runID); err == nil || !strings.Contains(err.Error(), "read sealed github tokens") {
		t.Fatalf("RevokeRun with an unreadable seal store = %v", err)
	}
	if rows := auditRows(t, f.pool, f.runID, "credential.revoke"); len(rows) != 0 {
		t.Fatalf("credential.revoke rows = %d, want 0", len(rows))
	}
}

// The in-mint prune never waits on a row another transaction holds.
func TestPG_TokenSeal_PruneSkipsLockedRows(t *testing.T) {
	f := newSealFixture(t, true)
	other := uuid.New()
	seedRun(t.Context(), t, f.pool, other)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO run_github_token_seals (run_id, jti, owner, key_version, sealed, expires_at)
		VALUES ($1, 'old', 'someone', 1, '\x00'::bytea, now() - interval '10 minutes')`, other); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM run_github_token_seals WHERE run_id = $1 FOR UPDATE`, other); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := f.b.MintForGrant(ctx, callerFor(f.runID), f.grantID); err != nil {
		t.Fatalf("mint while a prunable row is locked: %v", err)
	}
	_ = tx.Rollback(t.Context())
	var n int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM run_github_token_seals WHERE run_id = $1`, other).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the locked row = %d, %v; want it skipped, not pruned", n, err)
	}
	if _, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.addGrant(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM run_github_token_seals WHERE run_id = $1`, other).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the released row = %d, %v; want it pruned by the next mint", n, err)
	}
}

// A rerun after a hand-back says handed_back and queues nothing; an abandoned tombstone says abandoned.
func TestPG_RevokeRun_RerunAfterHandBackSaysHandedBack(t *testing.T) {
	f := newSealFixture(t, true)
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.RevokeRun(t.Context(), f.runID); err != nil {
		t.Fatal(err)
	}
	f.b.githubRevokePass(t.Context())
	if r := revokedTokens(f.gh); len(r) != 1 || r[0] != got.Token {
		t.Fatalf("revoked = %v, want the minted token once", r)
	}
	var outcome string
	var sealedLen int
	if err := f.pool.QueryRow(t.Context(), `SELECT ended_outcome, length(sealed) FROM run_github_token_seals WHERE run_id = $1`, f.runID).Scan(&outcome, &sealedLen); err != nil {
		t.Fatal(err)
	}
	if outcome != sealHandedBack || sealedLen != 0 {
		t.Fatalf("row = %s with %d sealed bytes, want handed_back and emptied", outcome, sealedLen)
	}
	if err := f.b.RevokeRun(t.Context(), f.runID); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, f.pool, f.runID, "credential.revoke")
	if len(rows) != 2 || rows[1]["target_revoke"] != sealHandedBack {
		t.Fatalf("credential.revoke rows = %v, want the rerun to say handed_back", rows)
	}
	f.b.githubRevokePass(t.Context())
	if r := revokedTokens(f.gh); len(r) != 1 {
		t.Fatalf("revoked = %v after the rerun, want no second hand-back", r)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET ended_outcome = 'abandoned' WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if err := f.b.RevokeRun(t.Context(), f.runID); err != nil {
		t.Fatal(err)
	}
	if rows := auditRows(t, f.pool, f.runID, "credential.revoke"); rows[len(rows)-1]["target_revoke"] != sealAbandoned {
		t.Fatalf("last credential.revoke row = %v, want abandoned", rows[len(rows)-1])
	}
}

// A tombstone outlives its token's expiry by 5 minutes; then a worker pass prunes it, with no further mint, and it
// reads none.
func TestPG_TokenSeal_TombstonePrunedAfterExpiry(t *testing.T) {
	f := newSealFixture(t, true)
	got, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.grantID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.seal.end(t.Context(), f.runID, got.JTI, sealHandedBack); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET expires_at = now() - interval '2 minutes' WHERE run_id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.b.MintForGrant(t.Context(), callerFor(f.runID), f.addGrant(t)); err != nil {
		t.Fatal(err)
	}
	states, err := f.b.githubRevokeStates(t.Context(), f.runID, []string{got.JTI})
	if err != nil || states[got.JTI] != sealHandedBack {
		t.Fatalf("states 2 minutes past expiry = %v, %v; want handed_back kept", states, err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE run_github_token_seals SET expires_at = now() - interval '6 minutes' WHERE run_id = $1 AND jti = $2`, f.runID, got.JTI); err != nil {
		t.Fatal(err)
	}
	f.b.githubRevokePass(t.Context()) // the worker's backstop: no further mint is needed
	states, err = f.b.githubRevokeStates(t.Context(), f.runID, []string{got.JTI})
	if err != nil || len(states) != 0 {
		t.Fatalf("states 6 minutes past expiry = %v, %v; want the tombstone pruned (none)", states, err)
	}
}

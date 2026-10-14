// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var errMaskDown = errors.New("mask store is down")

// faultMaskBackend is the committed masking record with a fault source: hook,
// when set, decides each per-run registration.
type faultMaskBackend struct {
	secretmask.Backend
	mu   sync.Mutex
	hook func(value []byte) error
}

func (f *faultMaskBackend) PutRun(_ uuid.UUID, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hook == nil {
		return nil
	}
	return f.hook(value)
}

func (f *faultMaskBackend) set(hook func([]byte) error) {
	f.mu.Lock()
	f.hook = hook
	f.mu.Unlock()
}

func faultMaskRegistry() (*secretmask.Registry, *faultMaskBackend) {
	reg := secretmask.NewRegistry()
	fb := &faultMaskBackend{}
	reg.SetBackend(fb)
	return reg, fb
}

func masked(reg *secretmask.Registry, runID uuid.UUID, value string) bool {
	return slices.ContainsFunc(reg.Snapshot(runID), func(s []byte) bool { return string(s) == value })
}

// assertNotBurnt asserts nothing was burnt or recorded as minted.
func assertNotBurnt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID, approvalID uuid.UUID, got Minted) {
	t.Helper()
	if got.Token != "" || got.KnownHosts != "" || got.JTI != "" {
		t.Fatalf("a failed mint returned material: %+v", got)
	}
	if jti := readMintedJTI(ctx, t, pool, approvalID); jti != "" {
		t.Fatalf("approval burnt by a mint that delivered nothing: minted_jti=%q", jti)
	}
	if n := countMintAudits(ctx, t, pool, runID, "success"); n != 0 {
		t.Fatalf("success credential.mint rows = %d, want 0", n)
	}
}

// A masking record that cannot be written must leave the single-use approval
// unburnt, hand back the GitHub token, and let exactly one retry succeed.
func TestPG_MintMaskFailureLeavesApprovalUnburnt(t *testing.T) {
	pool := pgPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	scope := pgGithubScope(t)
	grantID := pgSeedGrant(ctx, t, pool, runID, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600})
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, scope)
	reg, fb := faultMaskRegistry()
	fb.set(func([]byte) error { return errMaskDown })
	gh := &FakeGitHubMinter{Token: "ghs_mask_fault_token"}
	b := New(NewPgxStore(pool), nil, poolAudit{pool}, nil, gh).WithMaskRegistry(reg)

	got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if !errors.Is(err, errMaskDown) {
		t.Fatalf("mint with the mask store down = %+v, %v; want %v", got, err, errMaskDown)
	}
	assertNotBurnt(ctx, t, pool, runID, approvalID, got)
	if gh.Revoked != 1 || gh.RevokedTokens[0] != gh.Token {
		t.Fatalf("undelivered github token not handed back: revoked=%v", gh.RevokedTokens)
	}

	fb.set(nil)
	got, err = b.MintForGrant(ctx, callerFor(runID), grantID)
	if err != nil || got.Token != gh.Token {
		t.Fatalf("retry after recovery = %+v, %v", got, err)
	}
	if jti := readMintedJTI(ctx, t, pool, approvalID); jti != got.JTI || !masked(reg, runID, got.Token) {
		t.Fatalf("retry: minted_jti=%q returned jti=%q masked=%v", jti, got.JTI, masked(reg, runID, got.Token))
	}
	if _, err := b.MintForGrant(ctx, callerFor(runID), grantID); !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("second retry = %v, want ErrAlreadyMinted", err)
	}
	if n := countMintAudits(ctx, t, pool, runID, "success"); n != 1 || gh.Revoked != 1 {
		t.Fatalf("success audits=%d revoked=%d, want exactly one success and no further revoke", n, gh.Revoked)
	}
}

// An ssh_key whose key registers but whose known_hosts does not delivers
// nothing, keeps the key's mask entry, and never revokes the standing key.
func TestPG_MintPartialMaskRegistration(t *testing.T) {
	pool := pgPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\npartial-mask-key\n-----END OPENSSH PRIVATE KEY-----\n"
	const kh = "git.example.test ssh-ed25519 AAAApartialmaskknownhosts"
	secrets := newMemSecrets()
	secrets.m["partial-key"], secrets.m["partial-kh"] = []byte(key), []byte(kh)
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	spec := sshKeySpec("git.example.test", "partial-key", "", "partial-kh")
	spec.RequiresApproval = true
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, spec.Scope)
	reg, fb := faultMaskRegistry()
	fb.set(func(v []byte) error {
		if string(v) == kh {
			return errMaskDown
		}
		return nil
	})
	gh := &FakeGitHubMinter{}
	b := New(NewPgxStore(pool), secrets, poolAudit{pool}, nil, gh).WithMaskRegistry(reg)

	got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
	if !errors.Is(err, errMaskDown) {
		t.Fatalf("mint with known_hosts unrecordable = %v, want %v", err, errMaskDown)
	}
	assertNotBurnt(ctx, t, pool, runID, approvalID, got)
	if !masked(reg, runID, key) || masked(reg, runID, kh) || gh.Revoked != 0 {
		t.Fatalf("key masked=%v known_hosts masked=%v revoked=%d", masked(reg, runID, key), masked(reg, runID, kh), gh.Revoked)
	}

	fb.set(nil)
	got, err = b.MintForGrant(ctx, callerFor(runID), grantID)
	if err != nil || got.Token != key || got.KnownHosts != kh || readMintedJTI(ctx, t, pool, approvalID) != got.JTI {
		t.Fatalf("retry after recovery = %+v, %v", got, err)
	}
	if n := countMintAudits(ctx, t, pool, runID, "success"); n != 1 || !masked(reg, runID, kh) {
		t.Fatalf("success audits=%d known_hosts masked=%v", n, masked(reg, runID, kh))
	}
}

// Authority that changes while the values are being recorded is caught by the
// locked revalidation. The change is written on the broker's own pool of one
// connection, so the registration must hold no transaction open.
func TestPG_MintRevalidatesAfterMaskRegistration(t *testing.T) {
	one := singleMintPool(t)
	scope := pgGithubScope(t)
	widened, _ := json.Marshal(githubScope{Repos: []string{"acme/widgets", "acme/secrets"}, Permissions: map[string]string{"contents": "write"}})
	arms := []struct {
		name    string
		sql     string
		args    func(runID, approvalID uuid.UUID) []any
		wantErr error
		outcome string
	}{
		{"loser", `UPDATE approvals SET minted_jti = 'claimed-elsewhere' WHERE id = $1`,
			func(_, a uuid.UUID) []any { return []any{a} }, ErrAlreadyMinted, "denied"},
		{"revoked run", `INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`,
			func(r, _ uuid.UUID) []any { return []any{"run:" + r.String(), r} }, ErrRunRevoked, "denied"},
		{"scope widened", `UPDATE approvals SET requested_scope = '` + string(widened) + `' WHERE id = $1`,
			func(_, a uuid.UUID) []any { return []any{a} }, ErrScopeMismatch, "denied"},
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			runID := uuid.New()
			seedRun(ctx, t, one, runID)
			grantID := pgSeedGrant(ctx, t, one, runID, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600})
			approvalID := pgSeedApproval(ctx, t, one, runID, grantID, scope)
			reg, fb := faultMaskRegistry()
			var once sync.Once
			fb.set(func([]byte) error {
				var err error
				once.Do(func() { _, err = one.Exec(ctx, a.sql, a.args(runID, approvalID)...) })
				return err
			})
			gh := &FakeGitHubMinter{Token: "ghs_revalidated_token"}
			b := New(NewPgxStore(one), nil, poolAudit{one}, nil, gh).WithMaskRegistry(reg)

			got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			if !errors.Is(err, a.wantErr) || got.Token != "" {
				t.Fatalf("mint = %+v, %v; want %v", got, err, a.wantErr)
			}
			if n := countMintAudits(ctx, t, one, runID, "success"); n != 0 {
				t.Fatalf("success credential.mint rows = %d", n)
			}
			if n := countMintAudits(ctx, t, one, runID, a.outcome); n != 1 {
				t.Fatalf("%s credential.mint rows = %d, want 1", a.outcome, n)
			}
			if gh.Calls != 1 || gh.Revoked != 1 || !masked(reg, runID, gh.Token) {
				t.Fatalf("calls=%d revoked=%d masked=%v: the undelivered token must be revoked and its mask kept", gh.Calls, gh.Revoked, masked(reg, runID, gh.Token))
			}
		})
	}
}

// uniqueMinter issues a distinct token per call, so every concurrent caller
// writes its own masking row.
type uniqueMinter struct {
	*FakeGitHubMinter
	n *atomic.Int32
}

func (m uniqueMinter) MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (string, time.Time, error) {
	_, exp, err := m.FakeGitHubMinter.MintInstallationToken(ctx, repos, permissions, ttl)
	return fmt.Sprintf("ghs_shared_pool_%d", m.n.Add(1)), exp, err
}

// The real mask store and the broker share one pool of ONE connection, as a
// small deployment pool does at its limit. Concurrent callers must all finish
// well inside the mask store's 10s write timeout, exactly one with a credential
// whose value is on the committed masking record.
func TestPG_MintMasksOnSharedSingleConnection(t *testing.T) {
	migrated := subjectkeytest.ThrowawayDB(t)
	cfg := migrated.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	one, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	maskstore.New(one, subjectkeytest.Manager(one, k), reg)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	runID := uuid.New()
	seedRun(ctx, t, one, runID)
	if _, err := one.Exec(ctx, `INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, 'tester')`, runID); err != nil {
		t.Fatal(err)
	}
	scope := pgGithubScope(t)
	grantID := pgSeedGrant(ctx, t, one, runID, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: true, TTLSeconds: 600})
	approvalID := pgSeedApproval(ctx, t, one, runID, grantID, scope)
	gh := &FakeGitHubMinter{}
	b := New(NewPgxStore(one), nil, poolAudit{one}, nil, uniqueMinter{gh, &atomic.Int32{}}).WithMaskRegistry(reg)

	const callers = 6
	type result struct {
		minted Minted
		err    error
	}
	done := make(chan result, callers)
	start := time.Now()
	for range callers {
		go func() {
			got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			done <- result{got, err}
		}()
	}
	var winner Minted
	for range callers {
		r := <-done
		switch {
		case r.err == nil && winner.Token == "":
			winner = r.minted
		case errors.Is(r.err, ErrAlreadyMinted) && r.minted.Token == "":
		default:
			t.Fatalf("caller = %+v, %v (after %s)", r.minted, r.err, time.Since(start))
		}
	}
	if winner.Token == "" || readMintedJTI(ctx, t, one, approvalID) != winner.JTI {
		t.Fatalf("no single winner matches the approval burn: %+v", winner)
	}
	var rows int
	if err := one.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE run_id=$1 AND NOT tombstone`, runID).Scan(&rows); err != nil || rows < 1 || !masked(reg, runID, winner.Token) {
		t.Fatalf("winner's value not on the masking record: rows=%d masked=%v err=%v", rows, masked(reg, runID, winner.Token), err)
	}
	if n := countMintAudits(ctx, t, one, runID, "success"); n != 1 || gh.Revoked != gh.Calls-1 {
		t.Fatalf("success audits=%d calls=%d revoked=%d", n, gh.Calls, gh.Revoked)
	}
	t.Logf("%d callers on one connection finished in %s; %d tokens issued, %d handed back", callers, time.Since(start), gh.Calls, gh.Revoked)
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func singleMintPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := pgPool(t).Config()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type concurrentStoredSecrets struct {
	secretstore.Store
	reads         *atomic.Int32
	ready, resume chan struct{}
	want          int32
}

func (s *concurrentStoredSecrets) For(owner string) secretstore.Store {
	cp := *s
	cp.Store = s.Store.For(owner)
	return &cp
}

func (s *concurrentStoredSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	value, err := s.Store.Get(ctx, name)
	if s.reads.Add(1) == s.want {
		close(s.ready)
	}
	select {
	case <-s.resume:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return value, err
}

func TestPG_ConcurrentStoredMintSingleConnection(t *testing.T) {
	pool := singleMintPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	name := "concurrent-stored-" + uuid.NewString()
	if err := secrets.Put(ctx, name, []byte("stored-pat")); err != nil {
		t.Fatal(err)
	}
	runID := uuid.New()
	seedRun(ctx, t, pool, runID)
	raw, _ := json.Marshal(map[string]string{"host": "git.example.test", "secret_name": name})
	spec := types.GrantSpec{Kind: types.GrantGitPAT, Scope: raw, RequiresApproval: true, TTLSeconds: 600}
	grantID := pgSeedGrant(ctx, t, pool, runID, spec)
	approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, raw)
	const callers = 6
	prepared := &concurrentStoredSecrets{Store: secrets, reads: &atomic.Int32{}, ready: make(chan struct{}), resume: make(chan struct{}), want: callers}
	b := New(NewPgxStore(pool), prepared, &fakeAudit{}, nil, nil)
	type result struct {
		minted Minted
		err    error
	}
	done := make(chan result, callers)
	for range callers {
		go func() {
			got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			done <- result{got, err}
		}()
	}
	select {
	case <-prepared.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(prepared.resume)
	var wins int
	for range callers {
		got := <-done
		if got.err == nil {
			wins++
			if got.minted.Token != "stored-pat" || readMintedJTI(ctx, t, pool, approvalID) != got.minted.JTI {
				t.Fatalf("returned credential does not match the approval burn: %+v", got.minted)
			}
		} else if !errors.Is(got.err, ErrAlreadyMinted) || got.minted.Token != "" {
			t.Fatalf("loser = %+v, %v", got.minted, got.err)
		}
	}
	var successes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='credential.mint' AND outcome='success'`, runID).Scan(&successes); err != nil || successes != 1 || wins != 1 || prepared.reads.Load() != callers {
		t.Fatalf("wins=%d, success audits=%d, stored reads=%d: %v", wins, successes, prepared.reads.Load(), err)
	}
	t.Logf("%d authorized preparations read the existing credential; %d returned credential and atomic approval/audit commit", prepared.reads.Load(), wins)
}

func TestPG_MintStoredSecretsSingleConnection(t *testing.T) {
	pool := singleMintPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstorepg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []types.GrantKind{types.GrantGitPAT, types.GrantSSHKey} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			name := "pool-proof-" + uuid.NewString()
			if err := secrets.Put(ctx, name, []byte("stored-material")); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = secrets.Delete(context.Background(), name) })
			scope := map[string]string{"host": "git.example.test", "secret_name": name}
			if kind == types.GrantSSHKey {
				scope = map[string]string{"host": "git.example.test", "key_secret_ref": name, "known_hosts_secret_ref": name}
			}
			raw, _ := json.Marshal(scope)
			spec := types.GrantSpec{Kind: kind, Scope: raw, RequiresApproval: true, TTLSeconds: 600}
			runID := uuid.New()
			seedRun(ctx, t, pool, runID)
			grantID := pgSeedGrant(ctx, t, pool, runID, spec)
			approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, raw)
			b := New(NewPgxStore(pool), secrets, &fakeAudit{}, nil, nil)
			got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			if err != nil || got.Token != "stored-material" {
				t.Fatalf("mint = (%+v, %v)", got, err)
			}
			if readMintedJTI(ctx, t, pool, approvalID) != got.JTI {
				t.Fatal("approval did not burn the returned credential")
			}
			var successes int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='credential.mint' AND outcome='success'`, runID).Scan(&successes); err != nil || successes != 1 {
				t.Fatalf("success audit = %d, %v", successes, err)
			}
			if _, err := b.MintForGrant(ctx, callerFor(runID), grantID); !errors.Is(err, ErrAlreadyMinted) {
				t.Fatalf("second mint = %v", err)
			}
		})
	}
}

type preparedSecrets struct {
	secretstore.Store
	reads *int
	after *func()
}

func (s *preparedSecrets) For(owner string) secretstore.Store {
	cp := *s
	cp.Store = s.Store.For(owner)
	return &cp
}

func (s *preparedSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	value, err := s.Store.Get(ctx, name)
	*s.reads++
	if f := *s.after; f != nil {
		*s.after = nil
		f()
	}
	return value, err
}

func TestPG_MintRevalidatesAfterStoredPreparation(t *testing.T) {
	for _, change := range []string{"grant", "approval", "revocation"} {
		t.Run(change, func(t *testing.T) {
			pool := singleMintPool(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			runID := uuid.New()
			seedRun(ctx, t, pool, runID)
			id, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			secrets, err := secretstorepg.New(pool, id)
			if err != nil {
				t.Fatal(err)
			}
			name := "prepared-" + uuid.NewString()
			if err := secrets.Put(ctx, name, []byte("stored-pat")); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]string{"host": "git.example.test", "secret_name": name})
			spec := types.GrantSpec{Kind: types.GrantGitPAT, Scope: raw, RequiresApproval: true, TTLSeconds: 600}
			grantID := pgSeedGrant(ctx, t, pool, runID, spec)
			approvalID := pgSeedApproval(ctx, t, pool, runID, grantID, spec.Scope)
			reads := 0
			var after func()
			prepared := &preparedSecrets{Store: secrets, reads: &reads, after: &after}
			after = func() {
				if n := pool.Stat().AcquiredConns(); n != 0 {
					t.Fatalf("stored-secret read holds %d pooled connections", n)
				}
				var err error
				switch change {
				case "grant":
					_, err = pool.Exec(ctx, `UPDATE credential_grants SET spec=jsonb_set(spec,'{ttl_seconds}','60') WHERE id=$1`, grantID)
				case "approval":
					_, err = pool.Exec(ctx, `UPDATE approvals SET state='CANCELLED' WHERE id=$1`, approvalID)
				case "revocation":
					_, err = pool.Exec(ctx, `INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`, uuid.NewString(), runID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			b := New(NewPgxStore(pool), prepared, &fakeAudit{}, nil, nil)
			got, err := b.MintForGrant(ctx, callerFor(runID), grantID)
			if change == "grant" {
				if err != nil || reads != 2 || time.Until(got.ExpiresAt) > time.Minute {
					t.Fatalf("changed grant mint = (%+v, %v), calls %d", got, err, reads)
				}
			} else {
				if err == nil || readMintedJTI(ctx, t, pool, approvalID) != "" {
					t.Fatalf("changed authority issued a credential: %+v, %v", got, err)
				}
			}
			if change != "grant" && (reads != 1 || got.Token != "") {
				t.Fatalf("refused mint: %d reads, token %q", reads, got.Token)
			}
		})
	}
}

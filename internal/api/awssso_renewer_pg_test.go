// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The renewer across replicas, on Postgres: each replica runs its own renewer
// pass over the same runs and sessions, and AWS sees one renewal. Guarded by
// WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

// renewPGStore is the Postgres store with the fixture provider's site config,
// so a test need not write the provider roster.
type renewPGStore struct{ store.PG }

func (renewPGStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return reauthProviderSite(reauthProvider()), nil
}

// renewPGReplica is one wardynd over pool: its own store and secret-store
// handles, its own lock connections, mask registry and audit sink.
func renewPGReplica(t *testing.T, pool *pgxpool.Pool, id age.Identity) (*Server, *memAudit) {
	t.Helper()
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	audit := &memAudit{}
	s := &Server{cfg: Config{
		Store: renewPGStore{store.NewPG(pool)}, Secrets: sec, MaskRegistry: secretmask.NewRegistry(),
		Now: time.Now, Audit: audit,
	}}
	s.locks.override = db.NewPGLocker(pool, 4)
	return s, audit
}

// renewPGRun is a running provider run of alice's with its AWS SSO grant, and
// her session expiring in five minutes.
func renewPGRun(t *testing.T, pool *pgxpool.Pool, s *Server) types.AgentRun {
	t.Helper()
	ctx := context.Background()
	pg := store.NewPG(pool)
	now, id := time.Now().UTC(), uuid.New()
	run, err := pg.CreateRun(ctx, types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "alice@example.com", Agent: "claude-code", Task: "renewal",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(),
		RunnerTarget: "docker", ModelProviderID: reauthProviderID,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: reauthSnapshotScope(t, "alice@example.com", reauthRegion, "111122223333", "WardynAgent")},
	}); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	b := liveSSOBlob()
	b.ExpiresAt = now.Add(5 * time.Minute)
	b.RefreshToken = "refresh-token-both-replicas-see"
	b.ClientID, b.ClientSecret = "cid", "csec"
	b.RegistrationExpiresAt = now.Add(30 * 24 * time.Hour)
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.Secrets.For("alice@example.com").Put(ctx, providerSecretName(reauthProviderUID, providerSSOPart), raw); err != nil {
		t.Fatal(err)
	}
	return run
}

// slowRenewOIDC answers CreateToken with a fresh pair after holding the
// request a moment, so the other replica's pass runs while the first renewal
// is in flight.
func slowRenewOIDC(t *testing.T) *atomic.Int32 {
	t.Helper()
	return fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": renewedToken, "expiresIn": 3600})
	})
}

func TestPG_AWSSSORenewer_ConcurrentReplicasRenewOnce(t *testing.T) {
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	a, auditA := renewPGReplica(t, pool, id)
	b, auditB := renewPGReplica(t, pool, id)
	renewPGRun(t, pool, a)
	calls := slowRenewOIDC(t)

	var wg sync.WaitGroup
	start := make(chan struct{})
	renewed := make([]int, 2)
	for i, s := range []*Server{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := s.RenewAWSSSOCredentials(context.Background())
			if err != nil {
				t.Errorf("replica %d: %v", i, err)
			}
			renewed[i] = n
		}()
	}
	close(start)
	wg.Wait()

	if calls.Load() != 1 || renewed[0]+renewed[1] != 1 {
		t.Fatalf("CreateToken calls = %d, renewals reported = %v; want one renewal across the two replicas", calls.Load(), renewed)
	}
	if rows := len(auditA.find("harness.credential.refresh")) + len(auditB.find("harness.credential.refresh")); rows != 1 {
		t.Fatalf("harness.credential.refresh rows = %d, want 1", rows)
	}
	stored, found, err := b.readAWSSSOBlob(context.Background(), reauthScope("alice@example.com"))
	if err != nil || !found || stored.AccessToken != renewedToken {
		t.Fatalf("stored session = %q (found %v, err %v), want the renewed token", stored.AccessToken, found, err)
	}
	// A later pass on either replica finds the session renewed: nothing more.
	for _, s := range []*Server{a, b} {
		if _, err := s.RenewAWSSSOCredentials(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("CreateToken calls after a second pass = %d, want 1", calls.Load())
	}
}

// A revoked run's grant is never renewed for, though its run row still reads
// RUNNING: the broker would refuse its mint, so the proxy can never serve it.
func TestPG_AWSSSORenewer_RevokedRunIsNotRenewed(t *testing.T) {
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	s, audit := renewPGReplica(t, pool, id)
	run := renewPGRun(t, pool, s)
	if _, err := pool.Exec(context.Background(), `INSERT INTO identity_revocations (jti, run_id) VALUES ($1, $2)`, "run:"+run.ID.String(), run.ID); err != nil {
		t.Fatal(err)
	}
	calls := slowRenewOIDC(t)
	if n, err := s.RenewAWSSSOCredentials(context.Background()); n != 0 || err != nil || calls.Load() != 0 {
		t.Fatalf("renewer pass = %d renewed, err %v, %d CreateToken calls; want none for a revoked run", n, err, calls.Load())
	}
	if rows := audit.find("harness.credential.refresh"); len(rows) != 0 {
		t.Fatalf("harness.credential.refresh rows = %d, want none", len(rows))
	}
}

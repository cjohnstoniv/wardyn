// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ownerTestOrgHash = "org-hash"

func ownPool(t *testing.T, pg store.PG, owner, name string) types.RunnerPool {
	t.Helper()
	p, err := pg.CreateRunnerPool(context.Background(), types.RunnerPool{ID: uuid.New(), Name: name, HostingType: types.RunnerPoolSelfHosted, Owner: owner}, owner)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func claimedRunnerOf(t *testing.T, pg store.PG, owner string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	fp := "fp-" + id.String()
	if _, err := pg.CreateRunner(ctx, types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: append(id[:], id[:]...), KeyFingerprint: fp, OrgURLSHA256: ownerTestOrgHash}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ClaimRunner(ctx, id, owner, fp, time.Now()); err != nil {
		t.Fatal(err)
	}
	return id
}

// The store adds a runner only to a pool its own owner owns: a person's own runner offered to another
// person's pool is ErrNotFound and writes nothing, whatever the route above did.
func TestPG_AddOwnRunnerToPoolRefusesAnotherOwnersPool(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	theirs := ownPool(t, pg, "sub-a", "Theirs")
	mine := claimedRunnerOf(t, pg, "sub-b")
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, theirs.ID, mine, "sub-b", ownerTestOrgHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("adding to another owner's pool = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pool_members WHERE pool_id = $1`, theirs.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d members (%v)", n, err)
	}
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, ownPool(t, pg, "sub-b", "Mine").ID, mine, "sub-b", ownerTestOrgHash); err != nil {
		t.Errorf("adding to one's own pool: %v", err)
	}
}

// The oversight read lists the organisation's pools and then the caller's own before anyone else's, so
// however many other people's pools there are, neither drops out at the cap.
func TestPG_OversightCatalogueKeepsTheOrganisationsAndTheCallersOwnPools(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO runner_pools (id, name, hosting_type, owner)
		SELECT gen_random_uuid(), 'a' || g, 'self_hosted', 'sub-' || g FROM generate_series(1, $1::int) g`, store.RunnerPoolsOversightMax); err != nil {
		t.Fatal(err)
	}
	remote, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "zz remote", HostingType: types.RunnerPoolRemoteProvided}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	mine := ownPool(t, pg, "sub-operator", "zz mine")
	rows, err := pg.ListRunnerPoolCatalogue(ctx, "sub-operator", true)
	if err != nil {
		t.Fatal(err)
	}
	found := map[uuid.UUID]bool{}
	for _, r := range rows {
		found[r.Pool.ID] = true
	}
	if len(rows) != store.RunnerPoolsOversightMax || !found[remote.ID] || !found[mine.ID] {
		t.Errorf("oversight read of %d rows: remote listed %v, own listed %v", len(rows), found[remote.ID], found[mine.ID])
	}
}

// A person's erasure holds the lock every pool create takes, so a create cannot slip between the
// erasure's read of their pools and its closing write.
func TestPG_DeleteRunnerPoolsByOwnerWaitsForCreates(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	ownPool(t, pg, "sub-leaver", "Laptop")
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed below on the success path
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('runner_pools.create'))`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := pg.DeleteRunnerPoolsByOwner(short, "sub-leaver"); err == nil {
		t.Fatal("the erasure ran while a create held the catalogue lock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	deleted, err := pg.DeleteRunnerPoolsByOwner(ctx, "sub-leaver")
	if err != nil || len(deleted) != 1 {
		t.Fatalf("erasure after the lock = %d deletions, %v", len(deleted), err)
	}
}

// The Remote Provided bootstrap ignores person-owned pools: a member's own Self-Hosted pool made before
// the first boot with executors does not stop the organisation's pool being made.
func TestPG_BootstrapIgnoresPersonOwnedPools(t *testing.T) {
	pg := store.NewPG(runsPGPoolIsolated(t))
	ownPool(t, pg, "sub-early", "Laptop")
	created, defaults, err := pg.BootstrapRunnerPools(context.Background(), []string{"docker"}, "Remote Provided")
	if err != nil || created == nil || defaults == nil || defaults.RemoteProvided == nil || *defaults.RemoteProvided != created.ID {
		t.Fatalf("bootstrap beside a person's pool = %+v, %+v, %v; want the organisation's pool made and defaulted", created, defaults, err)
	}
}

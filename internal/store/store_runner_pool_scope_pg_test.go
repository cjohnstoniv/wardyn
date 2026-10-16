// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DONE WHEN (RP-1 1): the add statement holds both owners, so another person's runner never joins
// someone's own user pool, whatever the route above did, and nothing is written.
func TestPG_UserPoolNeverHoldsAnotherPersonsRunner(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mine := ownPool(t, pg, "sub-a", "Mine")
	theirs := claimedRunnerOf(t, pg, "sub-b")
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, mine.ID, theirs, "sub-a", ownerTestOrgHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("adding another person's runner to one's own pool = %v, want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pool_members`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d members (%v)", n, err)
	}
	if p, _ := pg.GetRunnerPool(ctx, mine.ID); p.Revision != 1 {
		t.Errorf("the refused add raised the revision to %d", p.Revision)
	}
}

// DONE WHEN (RP-1 2): an organization pool never holds a self-hosted runner, its own owner's included:
// the store refuses it by scope (ErrRunnerPoolScope), before any owner is compared.
func TestPG_OrganizationPoolNeverHoldsASelfHostedRunner(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	org, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	mine := claimedRunnerOf(t, pg, "sub-a")
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, org.ID, mine, "sub-a", ownerTestOrgHash); !errors.Is(err, store.ErrRunnerPoolScope) {
		t.Fatalf("a self-hosted runner into an organization pool = %v, want ErrRunnerPoolScope", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pool_members WHERE runner_id IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d self-hosted members (%v)", n, err)
	}
}

// A remote-hosted runner joins only its adder's own live user pool, and only at the runner revision
// whose people policy the caller checked: a policy written in between is ErrRunnerPoolStale.
func TestPG_AddRemoteRunnerToUserPoolHoldsThePolicyRevision(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	if _, _, err := pg.EnsureRemoteRunners(ctx, []string{"docker"}, nil); err != nil {
		t.Fatal(err)
	}
	mine := ownPool(t, pg, "sub-a", "Mine")
	theirs := ownPool(t, pg, "sub-b", "Theirs")
	org, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"another person's pool": theirs.ID, "an organization pool": org.ID} {
		if _, _, _, err := pg.AddRemoteRunnerToUserPool(ctx, id, "docker", "sub-a", 1); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
	if _, _, _, err := pg.AddRemoteRunnerToUserPool(ctx, mine.ID, "nomad", "sub-a", 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a runner with no row = %v, want ErrNotFound", err)
	}
	if _, _, err := pg.SetRemoteRunnerPeople(ctx, "docker", nil, types.PeoplePoolsEveryone, nil, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := pg.AddRemoteRunnerToUserPool(ctx, mine.ID, "docker", "sub-a", 1); !errors.Is(err, store.ErrRunnerPoolStale) {
		t.Errorf("an add at the revision before the policy moved = %v, want ErrRunnerPoolStale", err)
	}
	m, added, rev, err := pg.AddRemoteRunnerToUserPool(ctx, mine.ID, "docker", "sub-a", 2)
	if err != nil || !added || rev != 2 || m.Kind != types.RunnerKindRemoteHosted || m.RemoteRunnerID != "docker" {
		t.Fatalf("an add at the current revision = %+v %v %d %v", m, added, rev, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pool_members`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d members (%v), want only the admitted add", n, err)
	}
}

// The organisation default cannot name a pool that was deleted after the caller looked: the write
// locks the pool first and refuses a deleted one, and writes nothing.
func TestPG_OrgDefaultRefusesADeletedPool(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	org, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID}, "admin"); err != nil {
		t.Fatalf("naming a live pool: %v", err)
	}
	if _, err := pg.DeleteRunnerPool(ctx, org.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.GetRunnerPoolOrgDefaults(ctx); got.PoolID != nil {
		t.Fatalf("deleting the pool left the default naming it: %v", got.PoolID)
	}
	if err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID}, "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("naming a deleted pool = %v, want ErrNotFound", err)
	}
	if got, _ := pg.GetRunnerPoolOrgDefaults(ctx); got.PoolID != nil {
		t.Errorf("the refused write named %v", got.PoolID)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	if _, _, err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID}, "admin"); err != nil {
		t.Fatalf("naming a live pool: %v", err)
	}
	// The write returns the defaults it replaced, read in its own transaction.
	if before, _, err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID, Barrier: types.CC2}, "admin"); err != nil || before.PoolID == nil || *before.PoolID != org.ID {
		t.Fatalf("second write returned before=%+v err=%v, want the first write's pool", before, err)
	}
	if _, err := pg.DeleteRunnerPool(ctx, org.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.GetRunnerPoolOrgDefaults(ctx); got.PoolID != nil {
		t.Fatalf("deleting the pool left the default naming it: %v", got.PoolID)
	}
	if _, _, err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID}, "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("naming a deleted pool = %v, want ErrNotFound", err)
	}
	if got, _ := pg.GetRunnerPoolOrgDefaults(ctx); got.PoolID != nil {
		t.Errorf("the refused write named %v", got.PoolID)
	}
}

// The first organisation-defaults write has no row to lock. A write that meets another one's still
// uncommitted first row must audit that row as its before, not a zero: the seed gives the row lock
// something to queue on.
func TestPG_OrgDefaultsFirstWriteRaceAuditsARealBefore(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	// The other writer holds its own connection, so polling through the pool below takes no second one
	// from a goroutine already holding one.
	conn, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx) //nolint:errcheck // test teardown
	first, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx) //nolint:errcheck // committed below on the happy path
	var blockerPID int
	if err := first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	// The other writer's first row, not yet committed.
	if _, err := first.Exec(ctx, `INSERT INTO runner_pool_org_defaults (barrier, updated_by) VALUES ('CC2', 'first')`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		before, after types.RunnerPoolDefaults
		err           error
	}
	done := make(chan result, 1)
	go func() {
		b, a, err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{Barrier: types.CC3}, "second")
		done <- result{b, a, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))
			AND query ILIKE '%INSERT INTO runner_pool_org_defaults%')`, blockerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second write never queued behind the first row")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.before.Barrier != types.CC2 || r.before.Revision != 1 {
		t.Errorf("the second write audited before=%+v, want the first write's row (CC2, revision 1)", r.before)
	}
	if r.after.Barrier != types.CC3 || r.after.Revision != 2 {
		t.Errorf("the second write returned after=%+v, want CC3 at revision 2", r.after)
	}
}

// Deleting the pool the organisation default names audits the revision the UPDATE wrote, one past the
// one read under the lock.
func TestPG_DeletingTheDefaultPoolAuditsTheBumpedRevision(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	org, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, written, err := pg.PutRunnerPoolOrgDefaults(ctx, types.RunnerPoolDefaults{PoolID: &org.ID}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	del, err := pg.DeleteRunnerPool(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del.OrgDefaultsAfter == nil || del.OrgDefaultsAfter.Revision != written.Revision+1 {
		t.Fatalf("delete audited after=%+v, want revision %d", del.OrgDefaultsAfter, written.Revision+1)
	}
	if got, _ := pg.GetRunnerPoolOrgDefaults(ctx); got.Revision != del.OrgDefaultsAfter.Revision {
		t.Errorf("stored revision %d differs from the audited %d", got.Revision, del.OrgDefaultsAfter.Revision)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Kept machines against a throwaway Postgres database. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestListRunnerPoolsWithMember: the read lists every pool that holds the runner, whatever its scope
// or owner, so a pool the add path would never allow (written here straight into the table) still
// shows and is refused by the caller. A pool without the runner, and another runner's pools, do not.
func TestListRunnerPoolsWithMember(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mine := claimedRunnerOf(t, pg, "sub-a")
	other := claimedRunnerOf(t, pg, "sub-b")
	if got, err := pg.ListRunnerPoolsWithMember(ctx, mine); err != nil || len(got) != 0 {
		t.Fatalf("a runner in no pool: %v, %v", got, err)
	}
	own := ownPool(t, pg, "sub-a", "Mine")
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, own.ID, mine, "sub-a", ownerTestOrgHash); err != nil {
		t.Fatal(err)
	}
	theirs := ownPool(t, pg, "sub-b", "Theirs")
	if _, _, _, err := pg.AddOwnRunnerToPool(ctx, theirs.ID, other, "sub-b", ownerTestOrgHash); err != nil {
		t.Fatal(err)
	}
	org, err := pg.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Builds", Scope: types.RunnerPoolOrganization}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ownPool(t, pg, "sub-a", "Empty")
	if got, err := pg.ListRunnerPoolsWithMember(ctx, mine); err != nil || len(got) != 1 || got[0].ID != own.ID || got[0].Scope != types.RunnerPoolUser || got[0].Owner != "sub-a" {
		t.Fatalf("one own pool: %+v, %v", got, err)
	}
	for _, p := range []uuid.UUID{org.ID, theirs.ID} {
		if _, err := pool.Exec(ctx, `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, p, mine); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pg.ListRunnerPoolsWithMember(ctx, mine)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[uuid.UUID]types.RunnerPool{}
	for _, p := range got {
		seen[p.ID] = p
	}
	if len(got) != 3 || seen[own.ID].Owner != "sub-a" || seen[org.ID].Scope != types.RunnerPoolOrganization || seen[theirs.ID].Owner != "sub-b" {
		t.Errorf("own, organisation and another person's pool: %+v", got)
	}
	if got, err := pg.ListRunnerPoolsWithMember(ctx, other); err != nil || len(got) != 1 || got[0].ID != theirs.ID {
		t.Errorf("the other runner's pools: %+v, %v", got, err)
	}
}

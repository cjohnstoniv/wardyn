// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mintToken(t *testing.T, st store.PG, owner string, created, expires time.Time) (string, types.RunnerRegistrationToken) {
	t.Helper()
	raw := "wdr_" + uuid.NewString()
	got, err := st.MintRunnerRegistrationToken(context.Background(), raw, types.RunnerRegistrationToken{
		ID: uuid.New(), Owner: owner, MintedBy: "admin", OrgURLSHA256: "org", CreatedAt: created, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	return raw, got
}

func TestPG_RunnerTokenRevokeRacesRedemptionExactlyOneWins(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	for range 20 {
		now := time.Now().UTC()
		raw, token := mintToken(t, st, "alice", now, now.Add(time.Hour))
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, token.ID)
		})
		var redeemed, revoked atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "org", now); err != nil {
				t.Errorf("consume: %v", err)
			} else if ok {
				redeemed.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); err == nil {
				revoked.Add(1)
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("revoke: %v", err)
			}
		}()
		close(start)
		wg.Wait()
		if redeemed.Load()+revoked.Load() != 1 {
			t.Fatalf("redeemed=%d revoked=%d: a token must be spent by exactly one of them", redeemed.Load(), revoked.Load())
		}
	}
}

func TestPG_RunnerTokenRevokedCannotBeRedeemedOrRevokedAgain(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	raw, token := mintToken(t, st, "alice", now, now.Add(time.Hour))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, token.ID)
	})
	if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, raw, "org", now); err != nil || ok {
		t.Fatalf("revoked token redeemed: ok=%v err=%v", ok, err)
	}
	if _, err := st.RevokeRunnerRegistrationToken(ctx, token.ID, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second revoke = %v, want ErrNotFound", err)
	}
}

func TestPG_RunnerTokenListIsOnlyUnusedUnexpired(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	_, live := mintToken(t, st, "alice", now.Add(-time.Minute), now.Add(time.Hour))
	usedRaw, used := mintToken(t, st, "bob", now.Add(-time.Minute), now.Add(time.Hour))
	_, expired := mintToken(t, st, "cara", now.Add(-2*time.Hour), now.Add(-time.Hour))
	_, revoked := mintToken(t, st, "dan", now.Add(-time.Minute), now.Add(time.Hour))
	for _, tok := range []types.RunnerRegistrationToken{live, used, expired, revoked} {
		id := tok.ID
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, id)
		})
	}
	if _, ok, err := st.ConsumeRunnerRegistrationToken(ctx, usedRaw, "org", now); err != nil || !ok {
		t.Fatalf("consume: %v %v", ok, err)
	}
	if _, err := st.RevokeRunnerRegistrationToken(ctx, revoked.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListUnusedRunnerRegistrationTokensPage(ctx, now, "", store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, tok := range got {
		if tok.Token != "" {
			t.Fatalf("listed token carries a raw value: %+v", tok)
		}
		switch tok.ID {
		case live.ID, used.ID, expired.ID, revoked.ID:
			ids = append(ids, tok.ID)
		}
	}
	if len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("listed %v, want only the live token %v", ids, live.ID)
	}
}

func TestPG_RunnerMintedByAndActiveRunCounts(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	r := newRunner("alice@example.com")
	r.MintedBy = "admin@example.com"
	got, err := st.CreateRunner(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, r.ID) })
	if got.MintedBy != r.MintedBy {
		t.Fatalf("minted_by = %q, want %q", got.MintedBy, r.MintedBy)
	}
	for _, state := range []types.RunState{types.RunRunning, types.RunWaiting, types.RunCompleted} {
		run := newRun(state)
		run.Placement = types.PlacementLocal
		run.PlacementFilled = true
		run.RunnerID = &r.ID
		if _, err := st.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		id := run.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}
	counts, err := st.CountActiveRunsByRunner(ctx, []uuid.UUID{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if counts[r.ID] != 2 {
		t.Fatalf("active runs = %d, want 2 (running and waiting, not completed)", counts[r.ID])
	}
}

func TestPG_RunnerTokenListPagesAndFiltersByOwner(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	owner := "paged-" + uuid.NewString()
	var ids []uuid.UUID
	for i := range 3 {
		_, tok := mintToken(t, st, owner, now.Add(-time.Duration(i)*time.Minute), now.Add(time.Hour))
		ids = append(ids, tok.ID)
		id := tok.ID
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, id)
		})
	}
	_, other := mintToken(t, st, "other-"+uuid.NewString(), now, now.Add(time.Hour))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runner_registration_tokens WHERE id=$1`, other.ID)
	})

	first, err := st.ListUnusedRunnerRegistrationTokensPage(ctx, now, owner, store.Page{Limit: 2})
	if err != nil || len(first) != 2 || first[0].ID != ids[0] || first[1].ID != ids[1] {
		t.Fatalf("first page = %v, %v; want the two newest of the owner's", first, err)
	}
	rest, err := st.ListUnusedRunnerRegistrationTokensPage(ctx, now, owner, store.Page{Limit: 2, Offset: 2})
	if err != nil || len(rest) != 1 || rest[0].ID != ids[2] {
		t.Fatalf("second page = %v, %v", rest, err)
	}
}

func TestPG_RunnerListPageFiltersStateAndHidesLapsed(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(state types.RunnerState, age time.Duration) types.Runner {
		r, err := st.CreateRunner(ctx, newRunner("list@example.com"))
		if err != nil {
			t.Fatal(err)
		}
		id := r.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, id) })
		if _, err := pool.Exec(ctx, `UPDATE runners SET state=$2, created_at=$3 WHERE id=$1`, id, string(state), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return r
	}
	waiting, claimed := mk(types.RunnerUnclaimed, time.Hour), mk(types.RunnerClaimed, 48*time.Hour)
	revoked, lapsed := mk(types.RunnerRevoked, time.Hour), mk(types.RunnerUnclaimed, 25*time.Hour)
	has := func(filter types.RunnerFilter) map[uuid.UUID]bool {
		rows, err := st.ListRunnersPage(ctx, filter, now, store.Page{})
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]bool{}
		for _, r := range rows {
			out[r.ID] = true
		}
		return out
	}
	if a := has(types.RunnerFilterActive); !a[waiting.ID] || !a[claimed.ID] || a[revoked.ID] || a[lapsed.ID] {
		t.Fatalf("active = %v", a)
	}
	if a := has(types.RunnerFilterRevoked); a[waiting.ID] || a[claimed.ID] || !a[revoked.ID] || a[lapsed.ID] {
		t.Fatalf("revoked = %v", a)
	}
	if a := has(types.RunnerFilterAll); !a[waiting.ID] || !a[claimed.ID] || !a[revoked.ID] || a[lapsed.ID] {
		t.Fatalf("all = %v", a)
	}
	if rows, err := st.ListRunnersPage(ctx, types.RunnerFilterAll, now, store.Page{Limit: 1}); err != nil || len(rows) != 1 {
		t.Fatalf("a limit of 1 returned %d rows: %v", len(rows), err)
	}
	if _, err := st.ListRunnersPage(ctx, "bogus", now, store.Page{}); err == nil {
		t.Fatal("an unknown filter listed")
	}
}

func TestPG_LapsedUnclaimedRunnerIsDeletedAndItsKeyRegistersAgain(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	old := newRunner("alice@example.com")
	if _, err := st.CreateRunner(ctx, old); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE key_fingerprint=$1`, old.KeyFingerprint)
	})
	if _, err := pool.Exec(ctx, `UPDATE runners SET created_at=$2 WHERE id=$1`, old.ID, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again := old
	again.ID = uuid.New()
	if _, err := st.CreateRunner(ctx, again); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("before the sweep the lapsed row pins its key: %v", err)
	}
	if n, err := st.ExpireUnclaimedRunners(ctx, now.Add(-types.RunnerUnclaimedTTL)); err != nil || n < 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := st.GetRunner(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the lapsed row survived the sweep: %v", err)
	}
	if _, err := st.CreateRunner(ctx, again); err != nil {
		t.Fatalf("the key could not register again after the sweep: %v", err)
	}
}

// inventoryRun is a live run on rn, seeded past placement's runs-at-once admission: the inventory tests
// pin what a runner reports, up to 1001 runs, more than max_runs allows on one runner.
func inventoryRun(t *testing.T, st store.PG, rn types.Runner) types.AgentRun {
	t.Helper()
	r := newRun(types.RunRunning)
	r.CreatedBy, r.Placement = rn.Owner, types.PlacementLocal
	r.SandboxRef = "runner:" + rn.ID.String() + "/" + r.ID.String()
	r.EvidenceSource = types.RunEvidenceRunnerAsserted
	r, err := st.CreateRun(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(t.Context(), `UPDATE agent_runs SET runner_id = $2 WHERE id = $1`, r.ID, rn.ID); err != nil {
		t.Fatal(err)
	}
	r.RunnerID = &rn.ID
	return r
}

func TestPG_RunnerInventoryOver1000AndAtomicPages(t *testing.T) {
	_, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	rows := make([]runnerwire.RunState, 1001)
	for i := range rows {
		r := inventoryRun(t, st, rn)
		rows[i] = runnerwire.RunState{RunID: r.ID, SandboxPresent: true}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].RunID.String() < rows[j].RunID.String() })
	session, inventory := uuid.NewString(), uuid.New()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	after := uuid.Nil
	for i := 0; i < len(rows); i += runnerwire.StatePageRows {
		end := min(i+runnerwire.StatePageRows, len(rows))
		page := runnerwire.StatePage{InventoryID: inventory, After: after, Runs: rows[i:end], Final: end == len(rows)}
		out, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page)
		if err != nil || out.NeedsReconciliation || out.Complete != page.Final {
			t.Fatalf("page %d: %+v %v", i, out, err)
		}
		after = rows[end-1].RunID
		if i == 0 {
			// An admission retry must not wipe progress or allow the first page twice.
			if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
				t.Fatal(err)
			}
			if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page); err == nil {
				t.Fatal("duplicate page accepted")
			}
		}
	}
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, runnerwire.StatePage{InventoryID: inventory, After: after, Final: true}); err == nil {
		t.Fatal("page after Final accepted")
	}
}

func TestPG_RunnerInventoryFinalOmissionUsesOriginalVersion(t *testing.T) {
	for _, scenario := range []string{"unchanged", "updated", "new-generation", "new-after-snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			pool, st, rn, _, _ := spoolPGFixture(t)
			ctx := t.Context()
			seen := inventoryRun(t, st, rn)
			var omitted types.AgentRun
			if scenario != "new-after-snapshot" {
				omitted = inventoryRun(t, st, rn)
			}
			session, inventory := uuid.NewString(), uuid.New()
			if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
				t.Fatal(err)
			}
			first := runnerwire.StatePage{InventoryID: inventory, Runs: []runnerwire.RunState{{RunID: seen.ID, SandboxPresent: true}}}
			out, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, first)
			if err != nil || out.Complete || out.NeedsReconciliation {
				t.Fatalf("partial inventory inferred omission: %+v %v", out, err)
			}
			if scenario == "updated" {
				if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=clock_timestamp() WHERE id=$1`, omitted.ID); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "new-generation" {
				if _, err := st.BeginRunnerExecution(ctx, rn.ID, omitted.ID, uuid.Nil, uuid.New(), runnerwire.MethodExec); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "new-after-snapshot" {
				omitted = inventoryRun(t, st, rn)
			}
			out, err = st.IntakeRunnerStatePage(ctx, rn.ID, session, runnerwire.StatePage{InventoryID: inventory, After: seen.ID, Final: true})
			if err != nil || !out.Complete || out.NeedsReconciliation != (scenario == "unchanged") {
				t.Fatalf("final inventory: %+v %v", out, err)
			}
			got, err := st.GetRun(ctx, omitted.ID)
			if err != nil || got.LostAt != nil || got.State != types.RunRunning {
				t.Fatalf("intake changed lifecycle: %+v %v", got, err)
			}
		})
	}
}

func TestPG_RunnerInventorySessionAndPageRollback(t *testing.T) {
	pool, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	run := inventoryRun(t, st, rn)
	bad := inventoryRun(t, st, rn)
	ids := []uuid.UUID{run.ID, bad.ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_by='foreign' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	session, nextSession := uuid.NewString(), uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	page := runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{{RunID: ids[0], SandboxPresent: true}, {RunID: ids[1], SandboxPresent: true}}}
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("foreign second row: %v", err)
	}
	var observations int
	var last uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_inventory_observations WHERE runner_id=$1`, rn.ID).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("partial observation committed: %d %v", observations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_run_id FROM runner_inventories WHERE runner_id=$1`, rn.ID).Scan(&last); err != nil || last != uuid.Nil {
		t.Fatalf("failed cursor advanced: %v %v", last, err)
	}
	page.Runs = page.Runs[:1]
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginRunnerInventory(ctx, rn.ID, nextSession); err != nil {
		t.Fatal(err)
	}
	// The stale session also cannot submit a structurally valid first page.
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("old session admitted: %v", err)
	}
	// The replacement cannot inherit seen rows from the interrupted session.
	out, err := st.IntakeRunnerStatePage(ctx, rn.ID, nextSession, runnerwire.StatePage{InventoryID: uuid.New(), Final: true})
	if err != nil || !out.Complete || !out.NeedsReconciliation {
		t.Fatalf("replacement inherited incomplete evidence: %+v %v", out, err)
	}
}

func TestPG_RunnerInventoryRevocationBeforeFinal(t *testing.T) {
	pool, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	r := inventoryRun(t, st, rn)
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	page := runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{{RunID: r.ID, SandboxPresent: true}}}
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, page); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=clock_timestamp() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, runnerwire.StatePage{InventoryID: page.InventoryID, After: r.ID, Final: true}); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("revoked Final accepted: %v", err)
	}
}

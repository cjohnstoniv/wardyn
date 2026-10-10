// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"sort"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func inventoryRun(t *testing.T, st store.PG, rn types.Runner) types.AgentRun {
	t.Helper()
	r := newRun(types.RunRunning)
	r.CreatedBy, r.Placement, r.RunnerID = rn.Owner, types.PlacementLocal, &rn.ID
	r.SandboxRef = "runner:" + rn.ID.String() + "/" + r.ID.String()
	r.EvidenceSource = types.RunEvidenceRunnerAsserted
	r, err := st.CreateRun(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, scenario := range []string{"unchanged", "updated", "new-after-snapshot"} {
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
	final := runnerwire.StatePage{InventoryID: page.InventoryID, After: ids[0], Final: true}
	if _, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, final); !errors.Is(err, store.ErrRunnerExecutionRefused) {
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

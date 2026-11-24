// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The disk drive hold record (migration 0401) against a throwaway Postgres database. Guarded by WARDYN_TEST_PG;
// skipped cleanly when unset.

package store_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holdFixture is a migrated database with one member drive.
func holdFixture(t *testing.T) (*store.PG, *pgxpool.Pool, types.UserDrive) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	d, err := st.CreateMemberDrive(context.Background(), memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatal(err)
	}
	return &st, pool, d
}

func holdRun(t *testing.T, pool *pgxpool.Pool, state types.RunState) uuid.UUID {
	t.Helper()
	return persistRun(t, context.Background(), pool, newRun(state)).ID
}

func wantHolder(t *testing.T, st *store.PG, drive, want uuid.UUID) {
	t.Helper()
	got, err := st.MacDiskHolder(context.Background(), drive)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("holder = %v, want %v", got, want)
	}
}

// A first dispatch inserts the hold; the same run again keeps it; a finished run's hold is taken over; another live
// run's hold is left as it is and ok is false.
func TestMacDiskHold_DispatchWriteRules_PG(t *testing.T) {
	st, pool, d := holdFixture(t)
	ctx := context.Background()
	a, b := holdRun(t, pool, types.RunRunning), holdRun(t, pool, types.RunRunning)

	if ok, err := st.HoldMacDisk(ctx, d.ID, a); err != nil || !ok {
		t.Fatalf("first hold = %v, %v; want true", ok, err)
	}
	wantHolder(t, st, d.ID, a)
	if ok, err := st.HoldMacDisk(ctx, d.ID, a); err != nil || !ok {
		t.Fatalf("same run again = %v, %v; want true", ok, err)
	}
	if ok, err := st.HoldMacDisk(ctx, d.ID, b); err != nil || ok {
		t.Fatalf("over a live run's hold = %v, %v; want false, nil", ok, err)
	}
	wantHolder(t, st, d.ID, a)

	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'FAILED' WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HoldMacDisk(ctx, d.ID, b); err != nil || !ok {
		t.Fatalf("over a finished run's hold = %v, %v; want true", ok, err)
	}
	wantHolder(t, st, d.ID, b)
}

// A hold counts only while its run is not finished, whatever finished it; a holder that moves from RUNNING to FAILED
// frees the drive at once.
func TestMacDiskHold_TerminalHoldIgnored_PG(t *testing.T) {
	st, pool, d := holdFixture(t)
	ctx := context.Background()
	for _, state := range []types.RunState{types.RunCompleted, types.RunStopped, types.RunArchived, types.RunFailed, types.RunKilled} {
		run := holdRun(t, pool, state)
		if ok, err := st.HoldMacDisk(ctx, d.ID, run); err != nil || !ok {
			t.Fatalf("%s: hold = %v, %v", state, ok, err)
		}
		wantHolder(t, st, d.ID, uuid.Nil)
	}
	for _, state := range []types.RunState{types.RunQueued, types.RunPending, types.RunStarting, types.RunRunning, types.RunWaiting} {
		run := holdRun(t, pool, state)
		if ok, err := st.HoldMacDisk(ctx, d.ID, run); err != nil || !ok {
			t.Fatalf("%s: hold = %v, %v", state, ok, err)
		}
		wantHolder(t, st, d.ID, run)
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'FAILED' WHERE id = $1`, run); err != nil {
			t.Fatal(err)
		}
		wantHolder(t, st, d.ID, uuid.Nil)
	}
}

// Deleting the drive, or the run, removes its hold.
func TestMacDiskHold_CascadeOnDelete_PG(t *testing.T) {
	st, pool, d := holdFixture(t)
	ctx := context.Background()
	d2, err := st.CreateMemberDrive(ctx, memberDrive("alice", "Other"), 5)
	if err != nil {
		t.Fatal(err)
	}
	run := holdRun(t, pool, types.RunRunning)
	for _, id := range []uuid.UUID{d.ID, d2.ID} {
		if ok, err := st.HoldMacDisk(ctx, id, run); err != nil || !ok {
			t.Fatalf("hold = %v, %v", ok, err)
		}
	}
	count := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM mac_disk_holds`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := pool.Exec(ctx, `DELETE FROM user_drives WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("holds after the drive's delete = %d, want 1", n)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("holds after the run's delete = %d, want 0", n)
	}
}

// The role that holds UPDATE on user_drives gets SELECT, INSERT and UPDATE on mac_disk_holds, and not DELETE; a role
// that holds nothing on user_drives gets none of them. The migration file runs again after the roles are made, as
// the grant loop reads the catalog when it runs.
func TestMacDiskHold_Grants_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	app, other := "wardyn_hold_app_"+suffix, "wardyn_hold_other_"+suffix
	for _, r := range []string{app, other} {
		if _, err := pool.Exec(ctx, `CREATE ROLE `+r+` NOLOGIN`); err != nil {
			t.Fatalf("create role %s: %v", r, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DROP OWNED BY `+r)
			_, _ = pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+r)
		})
	}
	if _, err := pool.Exec(ctx, `GRANT SELECT, INSERT, UPDATE ON user_drives TO `+app); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `GRANT SELECT ON user_drives TO `+other); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../db/migrations/0401_mac_disk_holds.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("run the migration again: %v", err)
	}
	has := func(role, priv string) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, 'mac_disk_holds', $2)`, role, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": false} {
		if got := has(app, priv); got != want {
			t.Errorf("%s on mac_disk_holds for the app role = %v, want %v", priv, got, want)
		}
	}
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if has(other, priv) {
			t.Errorf("%s on mac_disk_holds for a read-only role = true, want false", priv)
		}
	}
}

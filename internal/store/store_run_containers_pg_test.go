// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for run_containers: the extra containers a run keeps beside its agent. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func extrasFixture() []types.RunExtraContainer {
	d := "sha256:" + strings.Repeat("d", 64)
	return []types.RunExtraContainer{
		{Name: "kernel", Image: "registry.example.com/team/kernel@" + d, Command: []string{"serve"}, Ports: []int{8888},
			CPUMillis: 500, MemoryMiB: 512, ImageAdmission: admissionVerdictFixture()},
		{Name: "mcp", Image: "registry.example.com/team/mcp@" + d, Ports: []int{5050}, CPUMillis: 250, MemoryMiB: 256},
	}
}

func runContainersRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_containers WHERE run_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPG_RunContainersRoundTrip: a run created with two extras reads them back as they were; one with none
// writes no row and reads nil.
func TestPG_RunContainersRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	with := newRun(types.RunPending)
	with.Containers = extrasFixture()
	created := persistRun(t, ctx, pool, with)
	got, err := pg.RunContainers(ctx, with.ID)
	if err != nil || !reflect.DeepEqual(got, with.Containers) || !reflect.DeepEqual(created.Containers, with.Containers) {
		t.Fatalf("extras read back %+v (%v), created %+v; want %+v", got, err, created.Containers, with.Containers)
	}
	none := persistRun(t, ctx, pool, newRun(types.RunPending))
	if got, err := pg.RunContainers(ctx, none.ID); err != nil || got != nil || none.Containers != nil {
		t.Errorf("a run with no extras read %+v (%v), created %+v; want nil", got, err, none.Containers)
	}
	if n := runContainersRows(t, pool, none.ID); n != 0 {
		t.Errorf("a run with no extras has %d run_containers rows", n)
	}
}

// TestPG_CreateRunUnderCapWritesContainers: the capped insert writes the extras in its own transaction.
func TestPG_CreateRunUnderCapWritesContainers(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	r.CreatedBy = "cap-" + uuid.NewString()
	r.Containers = extrasFixture()
	created, err := pg.CreateRunUnderCap(ctx, r, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
	got, err := pg.RunContainers(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(got, r.Containers) || !reflect.DeepEqual(created.Containers, r.Containers) {
		t.Fatalf("extras read back %+v (%v), created %+v; want %+v", got, err, created.Containers, r.Containers)
	}
}

// TestPG_StartQueuedRunRewritesContainers: a queued run joins with its extras, and the start writes the ones
// it admitted again over them.
func TestPG_StartQueuedRunRewritesContainers(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 2)
	now := time.Now().UTC()
	r := poolRun(p, types.RunQueued, "alice")
	r.Containers = extrasFixture()
	if _, err := pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.RunContainers(ctx, r.ID); err != nil || !reflect.DeepEqual(got, r.Containers) {
		t.Fatalf("queued extras %+v (%v), want %+v", got, err, r.Containers)
	}
	if promoted, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(promoted) != 1 {
		t.Fatalf("promote = %+v, %v; want the waiting run", promoted, err)
	}
	restarted := extrasFixture()
	restarted[0].Image = "registry.example.com/team/kernel@sha256:" + strings.Repeat("e", 64)
	restarted[0].ImageAdmission = nil
	started, ok, err := pg.StartQueuedRun(ctx, r.ID, store.QueueStart{SPIFFEID: "spiffe://x/" + r.ID.String(),
		ConfinementClass: types.CC2, Containers: restarted})
	if err != nil || !ok || !reflect.DeepEqual(started.Containers, restarted) {
		t.Fatalf("start = %+v ok=%v err=%v; want the start's extras", started.Containers, ok, err)
	}
	if got, err := pg.RunContainers(ctx, r.ID); err != nil || !reflect.DeepEqual(got, restarted) {
		t.Errorf("extras after the start %+v (%v), want %+v", got, err, restarted)
	}
}

// TestPG_RunContainersCascade: deleting the run deletes its extras.
func TestPG_RunContainersCascade(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	r := newRun(types.RunPending)
	r.Containers = extrasFixture()
	persistRun(t, ctx, pool, r)
	if n := runContainersRows(t, pool, r.ID); n != 1 {
		t.Fatalf("run_containers rows = %d, want 1", n)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if n := runContainersRows(t, pool, r.ID); n != 0 {
		t.Errorf("run_containers rows after the run's delete = %d, want 0", n)
	}
}

// TestPG_RunContainersWithoutTheGrant: a role refused the read (a split-role app role the grant loop missed) gets
// an error, never none, since rows another role wrote may be there; the erasure blank fails the same way. A role
// that can select but not insert reads the rows that exist; one that can insert but not select fails; with both it
// reads them.
func TestPG_RunContainersWithoutTheGrant(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	r := newRun(types.RunPending)
	r.Containers = extrasFixture()
	persistRun(t, ctx, pool, r)
	role := "wardyn_nogrant_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`CREATE ROLE ` + role + ` NOLOGIN`)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE IF EXISTS `+role) })
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["role"] = role // every connection of this pool runs as the role
	restricted, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restricted.Close)
	pg := store.NewPG(restricted)

	var pgErr *pgconn.PgError
	if got, err := pg.RunContainers(ctx, r.ID); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("read without any grant = %+v, %v; want an error with code 42501", got, err)
	}
	exec(`GRANT SELECT, UPDATE ON agent_runs, mcp_call_sends, run_harnesses TO ` + role)
	if _, err := pg.BlankRunTasks(ctx, r.CreatedBy); !errors.As(err, &pgErr) || pgErr.Code != "42501" ||
		!strings.Contains(err.Error(), "extra container commands") {
		t.Errorf("erasure blank without access to run_containers: %v; want it to fail on the extra container commands with code 42501", err)
	}
	// The failed blank left every command in place (read as the table's owner).
	if got, err := store.NewPG(pool).RunContainers(ctx, r.ID); err != nil || len(got) != len(r.Containers) {
		t.Fatalf("extras after the failed blank = %+v, %v; want the %d extras", got, err, len(r.Containers))
	} else {
		for i, c := range got {
			if !slices.Equal(c.Command, r.Containers[i].Command) {
				t.Errorf("extra %d command after the failed blank = %q, want %q", i, c.Command, r.Containers[i].Command)
			}
		}
	}
	exec(`GRANT SELECT ON run_containers TO ` + role)
	if got, err := pg.RunContainers(ctx, r.ID); err != nil || len(got) != len(r.Containers) {
		t.Errorf("read with SELECT and no INSERT = %+v, %v; want the %d extras", got, err, len(r.Containers))
	}
	exec(`REVOKE SELECT ON run_containers FROM ` + role)
	exec(`GRANT INSERT ON run_containers TO ` + role)
	if got, err := pg.RunContainers(ctx, r.ID); err == nil {
		t.Errorf("read with INSERT and no SELECT = %+v, nil; want an error", got)
	}
	exec(`GRANT SELECT ON run_containers TO ` + role)
	if got, err := pg.RunContainers(ctx, r.ID); err != nil || len(got) != len(r.Containers) {
		t.Errorf("read with INSERT and SELECT = %+v, %v; want the %d extras", got, err, len(r.Containers))
	}
}

// TestPG_BlankRunTasksStripsContainerCommands: the erasure blank drops each extra's command on the person's runs
// and keeps the rest of the entry, in order; another person's run keeps its command.
func TestPG_BlankRunTasksStripsContainerCommands(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	person := "erased-" + uuid.NewString()
	mine := newRun(types.RunRunning)
	mine.CreatedBy, mine.Containers = person, extrasFixture()
	persistRun(t, ctx, pool, mine)
	theirs := newRun(types.RunRunning)
	theirs.CreatedBy, theirs.Containers = "kept-"+uuid.NewString(), extrasFixture()
	persistRun(t, ctx, pool, theirs)
	if _, err := pg.BlankRunTasks(ctx, person); err != nil {
		t.Fatalf("BlankRunTasks: %v", err)
	}
	want := extrasFixture()
	for i := range want {
		want[i].Command = nil
	}
	if got, err := pg.RunContainers(ctx, mine.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("the person's extras after the blank = %+v (%v); want %+v", got, err, want)
	}
	if got, err := pg.RunContainers(ctx, theirs.ID); err != nil || !reflect.DeepEqual(got, theirs.Containers) {
		t.Errorf("another person's extras after the blank = %+v (%v); want them unchanged", got, err)
	}
	if _, err := pg.BlankRunTasks(ctx, person); err != nil {
		t.Errorf("a second BlankRunTasks: %v", err)
	}
}

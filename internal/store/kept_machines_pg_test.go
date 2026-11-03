// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Kept machines against a throwaway Postgres database. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"hash/crc32"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func keptMachineFor(owner string, runner, run uuid.UUID, name string) types.KeptMachine {
	return types.KeptMachine{Name: name, OwnerSubject: owner, RunnerID: runner, Base: "base-1",
		BasePreparedAt: time.Now().Add(-time.Hour).UTC(), RunID: &run, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC()}
}

// keptMachineOf creates owner's machine on runner for a new keep run, gives it to that run and
// records its stop, so it is kept. It returns the machine.
func keptMachineOf(t *testing.T, pg store.PG, owner string, runner uuid.UUID, name string) types.KeptMachine {
	t.Helper()
	ctx := context.Background()
	run := erasureRun(t, pg, owner, "keep")
	m, err := pg.CreateKeptMachine(ctx, keptMachineFor(owner, runner, run, name), 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, owner, run); err != nil {
		t.Fatal(err)
	}
	m, err = pg.RecordKeptMachineStop(ctx, store.KeptMachineStop{ID: m.ID, RunnerID: runner, RunID: run, Scrubbed: true, DiskBytes: 1 << 30, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil || m.State != types.KeptMachineKept {
		t.Fatalf("stop: %+v, %v", m, err)
	}
	return m
}

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

// TestKeptMachinePersonLimitAtInsert: the per-person limit is counted at the insert, rows being
// deleted count, another person is not counted, and a name is unique per person case-blind. The
// count waits for the person's lock: a row another transaction inserts under it is counted once that
// transaction commits, where an unlocked count would miss it and insert past the limit.
func TestKeptMachinePersonLimitAtInsert(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mac := claimedRunnerOf(t, pg, "sub-a")
	first, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, erasureRun(t, pg, "sub-a", "keep"), "dev"), 1)
	if err != nil || first.State != types.KeptMachineCreating || first.OwnerSubject != "sub-a" {
		t.Fatalf("first: %+v, %v", first, err)
	}
	if _, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, erasureRun(t, pg, "sub-a", "keep"), "dev2"), 1); !errors.Is(err, store.ErrKeptMachineLimit) {
		t.Fatalf("second at limit 1 = %v, want ErrKeptMachineLimit", err)
	}
	if _, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-b", claimedRunnerOf(t, pg, "sub-b"), erasureRun(t, pg, "sub-b", "keep"), "dev"), 1); err != nil {
		t.Fatalf("another person's first: %v", err)
	}
	if _, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, erasureRun(t, pg, "sub-a", "keep"), "DEV"), 2); !errors.Is(err, store.ErrKeptMachineNameTaken) {
		t.Fatalf("same name, other case = %v, want ErrKeptMachineNameTaken", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kept_machines SET state = 'deleting', delete_started_at = now(), delete_by = 'owner' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, erasureRun(t, pg, "sub-a", "keep"), "dev2"), 1); !errors.Is(err, store.ErrKeptMachineLimit) {
		t.Fatalf("a row being deleted still counts: %v", err)
	}

	// A concurrent insert under the lock, committed after the create started counting. The runs are
	// made first: this goroutine holds the transaction's connection, and a second acquire from it
	// is the nested pool acquire the pool guard stops.
	heldRun, racerRun := erasureRun(t, pg, "sub-c", "keep"), erasureRun(t, pg, "sub-c", "keep")
	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx) //nolint:errcheck // committed below
	if _, err := held.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, db.KeptMachineLockClass, int32(crc32.ChecksumIEEE([]byte("sub-c")))); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(ctx, `INSERT INTO kept_machines (id, name, owner_subject, runner_id, base, base_prepared_at, state, run_id, expires_at)
		VALUES ($1, 'held', 'sub-c', $2, 'base-1', now(), 'creating', $3, now() + interval '1 day')`, uuid.New(), mac, heldRun); err != nil {
		t.Fatal(err)
	}
	racer := keptMachineFor("sub-c", mac, racerRun, "racer")
	done := make(chan error, 1)
	go func() {
		_, err := pg.CreateKeptMachine(ctx, racer, 1)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the create did not wait for the person's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrKeptMachineLimit) {
		t.Fatalf("create after the held insert = %v, want ErrKeptMachineLimit", err)
	}
}

// TestBeginKeptMachineUseIsOwnerBound: another person can neither find nor take a person's machine,
// by id or by name, and their attempt changes nothing; the owner's resume takes it for the new run.
func TestBeginKeptMachineUseIsOwnerBound(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	m := keptMachineOf(t, pg, "sub-a", claimedRunnerOf(t, pg, "sub-a"), "dev")
	theirRun := erasureRun(t, pg, "sub-b", "resume")
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-b", theirRun); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's resume = %v, want ErrNotFound", err)
	}
	for _, ref := range []string{m.ID.String(), "dev"} {
		if _, err := pg.FindKeptMachineOf(ctx, "sub-b", ref); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("another person finds %q: %v", ref, err)
		}
	}
	if got, err := pg.GetKeptMachine(ctx, m.ID); err != nil || got.State != types.KeptMachineKept || *got.RunID != *m.RunID || !got.LastUsedAt.Equal(m.LastUsedAt) {
		t.Fatalf("the refused resume changed the row: %+v, %v", got, err)
	}
	if got, err := pg.FindKeptMachineOf(ctx, "sub-a", "DEV"); err != nil || got.ID != m.ID {
		t.Fatalf("own by name: %+v, %v", got, err)
	}
	mine := erasureRun(t, pg, "sub-a", "resume")
	got, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", mine)
	if err != nil || got.State != types.KeptMachineInUse || *got.RunID != mine {
		t.Fatalf("own resume: %+v, %v", got, err)
	}
	if again, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", mine); err != nil || *again.RunID != mine {
		t.Errorf("the holding run again: %+v, %v", again, err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", erasureRun(t, pg, "sub-a", "resume")); !errors.Is(err, store.ErrKeptMachineInUse) {
		t.Errorf("a second own run = %v, want ErrKeptMachineInUse", err)
	}
	if byRun, err := pg.GetKeptMachineByRun(ctx, mine); err != nil || byRun.ID != m.ID {
		t.Errorf("by run: %+v, %v", byRun, err)
	}
}

// TestBeginKeptMachineUseRace: of eight runs resuming one machine at once, exactly one gets it.
func TestBeginKeptMachineUseRace(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	m := keptMachineOf(t, pg, "sub-a", claimedRunnerOf(t, pg, "sub-a"), "dev")
	const racers = 8
	runs := make([]uuid.UUID, racers)
	for i := range runs {
		runs[i] = erasureRun(t, pg, "sub-a", "resume")
	}
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() { _, errs[i] = pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", runs[i]) })
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won < 0:
			won = i
		case err == nil:
			t.Errorf("runs %d and %d both took the machine", won, i)
		case !errors.Is(err, store.ErrKeptMachineInUse):
			t.Errorf("run %d: %v, want ErrKeptMachineInUse", i, err)
		}
	}
	if won < 0 {
		t.Fatal("no run took the machine")
	}
	if got, err := pg.GetKeptMachine(ctx, m.ID); err != nil || got.State != types.KeptMachineInUse || *got.RunID != runs[won] {
		t.Errorf("the row: %+v, %v; want in use by run %d", got, err, won)
	}
}

// TestKeptMachineCreatingAndStopReport: a creating machine goes only to its own keep run, is
// dropped only while creating, and is kept only by the stop report of the run that holds it, from
// its own runner, once.
func TestKeptMachineCreatingAndStopReport(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mac := claimedRunnerOf(t, pg, "sub-a")
	run := erasureRun(t, pg, "sub-a", "keep")
	m, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, run, "dev"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", erasureRun(t, pg, "sub-a", "other")); !errors.Is(err, store.ErrKeptMachineInUse) {
		t.Fatalf("another run takes a creating machine = %v, want ErrKeptMachineInUse", err)
	}
	if err := pg.DropUnstartedKeptMachine(ctx, m.ID, erasureRun(t, pg, "sub-a", "other")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("drop by another run = %v", err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", run); err != nil {
		t.Fatal(err)
	}
	if err := pg.DropUnstartedKeptMachine(ctx, m.ID, run); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("drop of a started machine = %v", err)
	}
	expires := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Microsecond)
	stop := store.KeptMachineStop{ID: m.ID, RunnerID: claimedRunnerOf(t, pg, "sub-a"), RunID: run, Scrubbed: true, DiskBytes: 12 << 30, ExpiresAt: expires}
	if _, err := pg.RecordKeptMachineStop(ctx, stop); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stop report from another runner = %v", err)
	}
	stop.RunnerID, stop.RunID = mac, erasureRun(t, pg, "sub-a", "other")
	if _, err := pg.RecordKeptMachineStop(ctx, stop); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stop report for another run = %v", err)
	}
	stop.RunID = run
	got, err := pg.RecordKeptMachineStop(ctx, stop)
	if err != nil || got.State != types.KeptMachineKept || !got.Scrubbed || got.DiskBytes != 12<<30 || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("stop: %+v, %v", got, err)
	}
	if _, err := pg.RecordKeptMachineStop(ctx, stop); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the same report again = %v", err)
	}
	if byRun, err := pg.GetKeptMachineByRun(ctx, run); err != nil || byRun.ID != m.ID {
		t.Errorf("by run after the stop: %+v, %v", byRun, err)
	}
	other := erasureRun(t, pg, "sub-a", "keep")
	n, err := pg.CreateKeptMachine(ctx, keptMachineFor("sub-a", mac, other, "dev2"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.DropUnstartedKeptMachine(ctx, n.ID, other); err != nil {
		t.Errorf("drop of an unstarted machine: %v", err)
	}
	if _, err := pg.GetKeptMachine(ctx, n.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dropped machine still read: %v", err)
	}
}

// TestKeptMachineDeleteTombstone: a delete refuses a machine a run holds and another person's,
// blocks a resume and a second delete, is listed while it waits, takes over a stale tombstone with
// its first by, aborts back to kept, and finishes only with its own tombstone.
func TestKeptMachineDeleteTombstone(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	m := keptMachineOf(t, pg, "sub-a", claimedRunnerOf(t, pg, "sub-a"), "dev")
	run := erasureRun(t, pg, "sub-a", "resume")
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", run); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginKeptMachineDelete(ctx, "", m.ID, "admin"); !errors.Is(err, store.ErrKeptMachineInUse) {
		t.Fatalf("delete of a held machine = %v, want ErrKeptMachineInUse", err)
	}
	if _, err := pg.RecordKeptMachineStop(ctx, store.KeptMachineStop{ID: m.ID, RunnerID: m.RunnerID, RunID: run, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginKeptMachineDelete(ctx, "sub-b", m.ID, "owner"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's delete = %v, want ErrNotFound", err)
	}
	d, err := pg.BeginKeptMachineDelete(ctx, "sub-a", m.ID, "owner")
	if err != nil || d.Machine.State != types.KeptMachineDeleting || d.By != "owner" || d.At.IsZero() {
		t.Fatalf("owner delete: %+v, %v", d, err)
	}
	if _, err := pg.BeginKeptMachineDelete(ctx, "", m.ID, "expiry"); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("a second delete = %v, want ErrKeptMachineDeleting", err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", erasureRun(t, pg, "sub-a", "resume")); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("resume of a machine being deleted = %v, want ErrKeptMachineDeleting", err)
	}
	if waiting, err := pg.ListKeptMachineDeletes(ctx); err != nil || len(waiting) != 1 || waiting[0].Machine.ID != m.ID || waiting[0].By != "owner" || !waiting[0].At.Equal(d.At) {
		t.Fatalf("waiting deletes: %+v, %v", waiting, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kept_machines SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	taken, err := pg.BeginKeptMachineDelete(ctx, "", m.ID, "mac_local")
	if err != nil || taken.By != "owner" || !taken.At.After(d.At) {
		t.Fatalf("takeover of a stale tombstone: %+v, %v", taken, err)
	}
	if err := pg.FinishKeptMachineDelete(ctx, m.ID, d.At); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finish with the taken-over tombstone = %v", err)
	}
	if err := pg.AbortKeptMachineDelete(ctx, m.ID, taken.At); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got, err := pg.GetKeptMachine(ctx, m.ID); err != nil || got.State != types.KeptMachineKept || got.DeleteStartedAt != nil {
		t.Fatalf("after abort: %+v, %v", got, err)
	}
	d, err = pg.BeginKeptMachineDelete(ctx, "", m.ID, "admin")
	if err != nil || d.By != "admin" {
		t.Fatalf("admin delete: %+v, %v", d, err)
	}
	if err := pg.FinishKeptMachineDelete(ctx, m.ID, d.At); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.GetKeptMachine(ctx, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("finished delete still read: %v", err)
	}
}

// TestKeptMachineExpiredList: only kept machines past their expiry, with no delete started or
// intended, are listed.
func TestKeptMachineExpiredList(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mac := claimedRunnerOf(t, pg, "sub-a")
	expired := keptMachineOf(t, pg, "sub-a", mac, "old")
	keptMachineOf(t, pg, "sub-a", mac, "fresh")
	held := keptMachineOf(t, pg, "sub-a", mac, "held")
	intended := keptMachineOf(t, pg, "sub-a", mac, "intended")
	if _, err := pg.BeginKeptMachineUse(ctx, held.ID, "sub-a", erasureRun(t, pg, "sub-a", "resume")); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SetKeptMachineDeleteIntent(ctx, *intended.RunID, "non_owner_kill", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kept_machines SET expires_at = now() - interval '1 minute' WHERE id = ANY($1)`,
		[]uuid.UUID{expired.ID, held.ID, intended.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListExpiredKeptMachines(ctx)
	if err != nil || len(got) != 1 || got[0].ID != expired.ID {
		t.Fatalf("expired: %+v, %v", got, err)
	}
}

// TestKeptMachineDeleteIntent: a kill's intent is written once (the first stands), blocks a resume
// and an ordinary delete, survives the stop report, and is listed until a delete starts from it, as
// it would be if the replica died before the kill's claim. That delete keeps the intent's by and
// killer, is never aborted, and is idempotent. An intent whose kill did not happen is cleared.
func TestKeptMachineDeleteIntent(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mac := claimedRunnerOf(t, pg, "sub-a")
	m := keptMachineOf(t, pg, "sub-a", mac, "dev")
	run := erasureRun(t, pg, "sub-a", "resume")
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", run); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SetKeptMachineDeleteIntent(ctx, erasureRun(t, pg, "sub-a", "none"), "non_owner_kill", "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("intent on a run with no machine = %v", err)
	}
	in, err := pg.SetKeptMachineDeleteIntent(ctx, run, "non_owner_kill", "admin")
	if err != nil || in.By != "non_owner_kill" || in.Killer != "admin" || in.At.IsZero() || in.Machine.State != types.KeptMachineInUse {
		t.Fatalf("intent: %+v, %v", in, err)
	}
	if again, err := pg.SetKeptMachineDeleteIntent(ctx, run, "non_owner_kill", "security"); err != nil || again.Killer != "admin" || !again.At.Equal(in.At) {
		t.Fatalf("intent again: %+v, %v; want the first", again, err)
	}
	if _, err := pg.RecordKeptMachineStop(ctx, store.KeptMachineStop{ID: m.ID, RunnerID: mac, RunID: run, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", erasureRun(t, pg, "sub-a", "resume")); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("resume with an intent = %v, want ErrKeptMachineDeleting", err)
	}
	if _, err := pg.BeginKeptMachineDelete(ctx, "sub-a", m.ID, "owner"); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("owner delete with an intent = %v, want ErrKeptMachineDeleting", err)
	}
	pending, err := pg.ListKeptMachineDeleteIntents(ctx)
	if err != nil || len(pending) != 1 || pending[0].Machine.ID != m.ID || !pending[0].At.Equal(in.At) || pending[0].Killer != "admin" {
		t.Fatalf("pending intents: %+v, %v", pending, err)
	}
	d, err := pg.BeginKeptMachineDeleteForKill(ctx, m.ID, in.At)
	if err != nil || d.Machine.State != types.KeptMachineDeleting || d.By != "non_owner_kill" || d.Killer != "admin" {
		t.Fatalf("delete from the intent: %+v, %v", d, err)
	}
	if again, err := pg.BeginKeptMachineDeleteForKill(ctx, m.ID, in.At); err != nil || !again.At.Equal(d.At) {
		t.Fatalf("delete from the intent again: %+v, %v", again, err)
	}
	if err := pg.AbortKeptMachineDelete(ctx, m.ID, d.At); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("abort of a kill's delete = %v, want ErrNotFound", err)
	}
	if err := pg.ClearKeptMachineDeleteIntent(ctx, m.ID, in.At); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("clear after the delete started = %v", err)
	}
	if pending, err := pg.ListKeptMachineDeleteIntents(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after the delete started: %+v, %v", pending, err)
	}
	if waiting, err := pg.ListKeptMachineDeletes(ctx); err != nil || len(waiting) != 1 || waiting[0].By != "non_owner_kill" || waiting[0].Killer != "admin" {
		t.Fatalf("waiting deletes: %+v, %v", waiting, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE kept_machines SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, m.ID); err != nil {
		t.Fatal(err)
	}
	if taken, err := pg.BeginKeptMachineDelete(ctx, "", m.ID, "mac_local"); err != nil || taken.By != "non_owner_kill" || taken.Killer != "admin" {
		t.Fatalf("takeover of a kill's tombstone: %+v, %v", taken, err)
	}

	other := keptMachineOf(t, pg, "sub-a", mac, "other")
	in, err = pg.SetKeptMachineDeleteIntent(ctx, *other.RunID, "non_owner_kill", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.ClearKeptMachineDeleteIntent(ctx, other.ID, in.At); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := pg.BeginKeptMachineDeleteForKill(ctx, other.ID, in.At); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete from a cleared intent = %v", err)
	}
	if _, err := pg.BeginKeptMachineUse(ctx, other.ID, "sub-a", erasureRun(t, pg, "sub-a", "resume")); err != nil {
		t.Errorf("resume after the intent was cleared: %v", err)
	}
}

// TestKeptMachineListsAndRunnerClose: the person's list and the admin list with its filters, and a
// revoked runner's rows closed, never a live runner's.
func TestKeptMachineListsAndRunnerClose(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	macA, macB := claimedRunnerOf(t, pg, "sub-a"), claimedRunnerOf(t, pg, "sub-b")
	keptMachineOf(t, pg, "sub-a", macA, "zeta")
	keptMachineOf(t, pg, "sub-a", macA, "Alpha")
	theirs := keptMachineOf(t, pg, "sub-b", macB, "dev")
	if mine, err := pg.ListKeptMachinesOf(ctx, "sub-a"); err != nil || len(mine) != 2 || mine[0].Name != "Alpha" || mine[1].Name != "zeta" {
		t.Fatalf("own list: %+v, %v", mine, err)
	}
	if all, total, err := pg.ListKeptMachines(ctx, store.KeptMachineFilter{}, store.Page{Limit: 2}); err != nil || total != 3 || len(all) != 2 {
		t.Fatalf("admin page: %d of %d, %v", len(all), total, err)
	}
	if byRunner, total, err := pg.ListKeptMachines(ctx, store.KeptMachineFilter{RunnerID: macB}, store.Page{}); err != nil || total != 1 || byRunner[0].ID != theirs.ID {
		t.Fatalf("by runner: %+v, %d, %v", byRunner, total, err)
	}
	if byOwner, total, err := pg.ListKeptMachines(ctx, store.KeptMachineFilter{Owner: "sub-a", RunnerID: macA}, store.Page{}); err != nil || total != 2 || len(byOwner) != 2 {
		t.Fatalf("by owner and runner: %+v, %d, %v", byOwner, total, err)
	}
	if closed, err := pg.CloseKeptMachinesOfRunner(ctx, macA); err != nil || len(closed) != 0 {
		t.Fatalf("a live runner closed %+v, %v", closed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET revoked_at = now() WHERE id = $1`, macA); err != nil {
		t.Fatal(err)
	}
	if closed, err := pg.CloseKeptMachinesOfRunner(ctx, macA); err != nil || len(closed) != 2 {
		t.Fatalf("revoked runner: %+v, %v", closed, err)
	}
	if left, total, err := pg.ListKeptMachines(ctx, store.KeptMachineFilter{}, store.Page{}); err != nil || total != 1 || left[0].ID != theirs.ID {
		t.Errorf("left: %+v, %d, %v", left, total, err)
	}
}

// TestKillRunMarkingKeptMachine: a kill's claim and its delete intent land together. A won claim marks
// the machine the run holds, which no resume then takes; a lost claim marks nothing and leaves the run;
// the killer's own machine gets no intent; a queued run is never claimed here.
func TestKillRunMarkingKeptMachine(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	ctx := context.Background()
	mac := claimedRunnerOf(t, pg, "sub-a")
	running := func(m types.KeptMachine) uuid.UUID {
		t.Helper()
		run := erasureRun(t, pg, "sub-a", "resume")
		if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", run); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'RUNNING', ended_at = NULL WHERE id = $1`, run); err != nil {
			t.Fatal(err)
		}
		return run
	}
	m := keptMachineOf(t, pg, "sub-a", mac, "dev")
	run := running(m)
	stateOf := func(id uuid.UUID) types.RunState {
		t.Helper()
		r, err := pg.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r.State
	}

	if applied, d, err := pg.KillRunMarkingKeptMachine(ctx, run, types.RunStarting, "non_owner_kill", "admin", "admin"); err != nil || applied || !d.At.IsZero() {
		t.Fatalf("lost claim: %v %+v %v", applied, d, err)
	}
	if in, err := pg.ListKeptMachineDeleteIntents(ctx); err != nil || len(in) != 0 || stateOf(run) != types.RunRunning {
		t.Fatalf("after a lost claim: intents %+v, %v, run %s", in, err, stateOf(run))
	}
	if _, _, err := pg.KillRunMarkingKeptMachine(ctx, run, types.RunQueued, "non_owner_kill", "admin", "admin"); err == nil {
		t.Fatal("a queued run was claimed past its queue")
	}

	applied, d, err := pg.KillRunMarkingKeptMachine(ctx, run, types.RunRunning, "non_owner_kill", "admin", "admin")
	if err != nil || !applied || d.Machine.ID != m.ID || d.By != "non_owner_kill" || d.Killer != "admin" || d.At.IsZero() {
		t.Fatalf("won claim: %v %+v %v", applied, d, err)
	}
	if stateOf(run) != types.RunKilled {
		t.Fatalf("run %s, want KILLED", stateOf(run))
	}
	if _, err := pg.BeginKeptMachineUse(ctx, m.ID, "sub-a", erasureRun(t, pg, "sub-a", "again")); !errors.Is(err, store.ErrKeptMachineDeleting) {
		t.Fatalf("resume of a machine a kill marked = %v, want ErrKeptMachineDeleting", err)
	}
	if in, err := pg.ListKeptMachineDeleteIntents(ctx); err != nil || len(in) != 1 || in[0].Machine.ID != m.ID {
		t.Fatalf("intents %+v, %v; want the marked machine", in, err)
	}

	own := keptMachineOf(t, pg, "sub-a", mac, "own")
	ownRun := running(own)
	if applied, d, err := pg.KillRunMarkingKeptMachine(ctx, ownRun, types.RunRunning, "non_owner_kill", "sub-a", "sub-a"); err != nil || !applied || !d.At.IsZero() {
		t.Fatalf("the owner's own kill: %v %+v %v; want claimed, no intent", applied, d, err)
	}
	if got, err := pg.GetKeptMachine(ctx, own.ID); err != nil || got.State != types.KeptMachineInUse {
		t.Fatalf("own machine: %+v, %v", got, err)
	}
}

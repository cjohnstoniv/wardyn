// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Member drives against a throwaway Postgres database. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func memberDrive(owner, name string) types.UserDrive {
	return types.UserDrive{Name: name, Backend: types.DriveBackendDockerVolume, OwnerSubject: owner, CreatedBy: owner}
}

// TestMemberDriveCountCapAtomic: with a cap of 2, eight concurrent creates
// that all pause between the count and the insert leave exactly two rows.
// Without the advisory lock every create counts zero and inserts.
func TestMemberDriveCountCapAtomic(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	const racers, limit = 8, 2

	var arrived atomic.Int32
	all := make(chan struct{})
	defer store.SetMemberDriveCounted(func() {
		if arrived.Add(1) == racers {
			close(all)
		}
		select { // under the lock no second racer can arrive, so give up waiting
		case <-all:
		case <-time.After(300 * time.Millisecond):
		}
	})()

	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Go(func() {
			_, errs[i] = st.CreateMemberDrive(ctx, memberDrive("alice", "drive "+uuid.NewString()[:8]), limit)
		})
	}
	wg.Wait()

	created, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, store.ErrMemberDriveLimit):
			refused++
		default:
			t.Fatalf("create: %v", err)
		}
	}
	rows, err := st.ListMemberDrivesOf(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if created != limit || refused != racers-limit || len(rows) != limit {
		t.Errorf("created %d, refused %d, rows %d; want %d, %d, %d", created, refused, len(rows), limit, racers-limit, limit)
	}
}

// TestMemberDriveNamesPerOwner: member names are unique per owner, never
// across people or against admin drives; admin drives keep the global rule.
func TestMemberDriveNamesPerOwner(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()

	alice, err := st.CreateMemberDrive(ctx, memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatalf("alice Notes: %v", err)
	}
	if alice.OwnerSubject != "alice" || alice.ObjectScheme != types.DriveObjectSchemeID ||
		alice.HomeTemplate != types.HomeTemplateHash || alice.Reclaim != types.DriveReclaimDelete || !alice.Writable {
		t.Errorf("created row = %+v, want owner alice, id scheme, hash home, reclaim delete, writable", alice)
	}
	if _, err := st.CreateMemberDrive(ctx, memberDrive("bob", "Notes"), 5); err != nil {
		t.Errorf("bob Notes beside alice's: %v", err)
	}
	if _, err := st.CreateMemberDrive(ctx, memberDrive("alice", "notes"), 5); !errors.Is(err, store.ErrMemberDriveNameTaken) {
		t.Errorf("alice notes twice = %v, want ErrMemberDriveNameTaken", err)
	}

	if _, err := st.UpsertUserDrive(ctx, mintedDrive("notes"), false); err != nil {
		t.Errorf("admin notes beside two member Notes: %v", err)
	}
	if _, err := st.CreateMemberDrive(ctx, memberDrive("carol", "notes"), 5); err != nil {
		t.Errorf("member notes beside admin notes: %v", err)
	}
	if _, err := st.UpsertUserDrive(ctx, mintedDrive("Notes"), false); !errors.Is(err, store.ErrDriveSlugConflict) {
		t.Errorf("admin Notes beside admin notes = %v, want ErrDriveSlugConflict", err)
	}
	if _, err := st.UpsertUserDrive(ctx, mintedDrive("notes"), false); !errors.Is(err, store.ErrConflict) {
		t.Errorf("admin notes twice = %v, want ErrConflict", err)
	}
}

// TestMemberDriveDeleteConcurrent: of two concurrent Begins one wins and the
// other is busy; a tombstone older than a minute is taken over, and the stale
// one can then neither finish nor abort.
func TestMemberDriveDeleteConcurrent(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	d, err := st.CreateMemberDrive(ctx, memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() { _, _, errs[i] = st.BeginMemberDriveDelete(ctx, "alice", d.ID) })
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, store.ErrMemberDriveBusy):
			t.Fatalf("begin: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d Begins won, want 1 (%v)", won, errs)
	}

	if _, err := pool.Exec(ctx, `UPDATE user_drives SET delete_started_at = now() - interval '61 seconds' WHERE id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	var stale time.Time
	if err := pool.QueryRow(ctx, `SELECT delete_started_at FROM user_drives WHERE id = $1`, d.ID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	_, second, err := st.BeginMemberDriveDeleteAdmin(ctx, d.ID)
	if err != nil {
		t.Fatalf("take over a 61-second-old tombstone: %v", err)
	}
	if err := st.FinishMemberDriveDelete(ctx, d.ID, stale); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("finish with the taken-over tombstone = %v, want ErrNotFound", err)
	}
	if err := st.AbortMemberDriveDelete(ctx, d.ID, stale); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("abort with the taken-over tombstone = %v, want ErrNotFound", err)
	}
	if err := st.AbortMemberDriveDelete(ctx, d.ID, second); err != nil {
		t.Errorf("abort our own tombstone: %v", err)
	}
	_, third, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID)
	if err != nil {
		t.Fatalf("begin after an abort: %v", err)
	}
	if err := st.FinishMemberDriveDelete(ctx, d.ID, third); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, err := st.GetMemberDriveOf(ctx, "alice", d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after finish = %v, want ErrNotFound", err)
	}
}

// TestMemberDriveOwnerPredicates: no path reaches a drive across owners, and
// the admin drive paths never edit or delete a member drive.
func TestMemberDriveOwnerPredicates(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	d, err := st.CreateMemberDrive(ctx, memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := st.UpsertUserDrive(ctx, mintedDrive("team"), false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.GetMemberDriveOf(ctx, "bob", d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob gets alice's drive = %v, want ErrNotFound", err)
	}
	for _, id := range []uuid.UUID{d.ID, admin.ID, uuid.New()} {
		if _, _, err := st.BeginMemberDriveDelete(ctx, "bob", id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("bob begins deleting %s = %v, want ErrNotFound", id, err)
		}
	}
	if _, _, err := st.BeginMemberDriveDeleteAdmin(ctx, admin.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("member delete of an admin drive = %v, want ErrNotFound", err)
	}

	if err := st.DeleteUserDrive(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("admin delete of a member drive = %v, want ErrNotFound", err)
	}
	edit := d
	edit.Name, edit.OwnerSubject = "renamed", ""
	if _, err := st.UpsertUserDrive(ctx, edit, false); err == nil {
		t.Error("admin upsert over a member drive succeeded")
	}
	after, err := st.GetMemberDriveOf(ctx, "alice", d.ID)
	if err != nil {
		t.Fatalf("alice's drive after the admin writes: %v", err)
	}
	if after.Name != "Notes" || after.OwnerSubject != "alice" {
		t.Errorf("alice's drive after the admin writes = %+v, want it unchanged", after)
	}

	// The admin reads carry the owner, so an admin handler can refuse or filter a member drive.
	if got, err := st.GetUserDrive(ctx, d.ID); err != nil || got.OwnerSubject != "alice" {
		t.Errorf("GetUserDrive(member) owner = %q, %v; want alice", got.OwnerSubject, err)
	}
	listed, err := st.ListUserDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[uuid.UUID]string{}
	for _, it := range listed {
		owners[it.ID] = it.OwnerSubject
	}
	if owners[d.ID] != "alice" || owners[admin.ID] != "" {
		t.Errorf("ListUserDrives owners = %v; want alice's drive owned and the admin drive not", owners)
	}

	all, total, err := st.ListMemberDrives(ctx, store.Page{Limit: 10})
	if err != nil || total != 1 || len(all) != 1 || all[0].ID != d.ID {
		t.Errorf("ListMemberDrives = %d rows, total %d, %v; want alice's drive only", len(all), total, err)
	}
}

// TestMemberDriveOwnerStarting: only a run of the owner's in STARTING answers true; a running one, an
// ended one and another person's starting one do not. The owner compares in canonical form.
func TestMemberDriveOwnerStarting(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	run := func(createdBy string, state types.RunState) {
		now := time.Now().UTC()
		if _, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: createdBy,
			Agent: "claude-code", ConfinementClass: types.CC2, State: state, RunnerTarget: "docker"}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
	ask := func(owner string) bool {
		t.Helper()
		got, err := st.MemberDriveOwnerStarting(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	run("alice@example.com", types.RunRunning)
	run("alice@example.com", types.RunFailed)
	run("bob@example.com", types.RunStarting)
	if ask("alice@example.com") {
		t.Fatal("a running and a failed run read as starting")
	}
	run("Alice@Example.com", types.RunStarting)
	if !ask("alice@example.com") {
		t.Fatal("the owner's starting run was missed")
	}
}

// TestGetMemberDrive: the admin route's read finds a member drive by id
// whoever owns it, and never an organisation drive or an unknown id.
func TestGetMemberDrive(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	d, err := st.CreateMemberDrive(ctx, memberDrive("alice", "Notes"), 5)
	if err != nil {
		t.Fatal(err)
	}
	org, err := st.UpsertUserDrive(ctx, mintedDrive("team"), false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetMemberDrive(ctx, d.ID); err != nil || got.ID != d.ID || got.OwnerSubject != "alice" {
		t.Fatalf("GetMemberDrive(member) = %+v, %v; want alice's drive", got, err)
	}
	for name, id := range map[string]uuid.UUID{"organisation drive": org.ID, "unknown id": uuid.New()} {
		if _, err := st.GetMemberDrive(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetMemberDrive(%s) = %v, want ErrNotFound", name, err)
		}
	}
}

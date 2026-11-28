// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The held member drive delete list against a throwaway Postgres database. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retryEnv is a migrated throwaway database and a store over it.
type retryEnv struct {
	st   store.PG
	pool *pgxpool.Pool
}

func newRetryEnv(t *testing.T) retryEnv {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	return retryEnv{st: store.NewPG(pool), pool: pool}
}

// heldRow creates one of alice's drives on backend and, when held is not "", marks its delete that long ago.
func heldRow(t *testing.T, ctx context.Context, e retryEnv, name string, backend types.DriveBackend, held string) uuid.UUID {
	t.Helper()
	d, err := e.st.CreateMemberDrive(ctx, types.UserDrive{Name: name, Backend: backend, OwnerSubject: "alice", CreatedBy: "alice"}, 200)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if held != "" {
		if _, err := e.pool.Exec(ctx, `UPDATE user_drives SET delete_started_at = now() - $2::interval, delete_issues = 1 WHERE id = $1`, d.ID, held); err != nil {
			t.Fatal(err)
		}
	}
	return d.ID
}

func TestPG_ListHeldMemberDriveDeletes_Filter(t *testing.T) {
	e := newRetryEnv(t)
	ctx := context.Background()
	// A Mac disk marked ten minutes ago: its Mac finishes it, never this list.
	rn, err := e.st.CreateRunner(ctx, newRunner("alice"))
	if err != nil {
		t.Fatal(err)
	}
	mac, err := e.st.CreateMemberDrive(ctx, types.UserDrive{Name: "mac", Backend: types.DriveBackendMacDisk, OwnerSubject: "alice", CreatedBy: "alice", HomeRunnerID: &rn.ID}, 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE user_drives SET delete_started_at = now() - interval '10 minutes', delete_issues = 1 WHERE id = $1`, mac.ID); err != nil {
		t.Fatal(err)
	}
	// An unowned (administrator's) drive can carry no mark (0177's CHECK), so it is listed by neither filter.
	if _, err := e.st.UpsertUserDrive(ctx, types.UserDrive{Name: "share", Backend: types.DriveBackendHostPath, HostRoot: "/srv/share",
		HomeTemplate: types.HomeTemplateEmailLocal, Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com"}, false); err != nil {
		t.Fatal(err)
	}
	young := heldRow(t, ctx, e, "young", types.DriveBackendDockerVolume, "2 minutes")
	unmarked := heldRow(t, ctx, e, "unmarked", types.DriveBackendDockerVolume, "")
	older := heldRow(t, ctx, e, "older", types.DriveBackendDockerVolume, "9 minutes")
	old := heldRow(t, ctx, e, "old", types.DriveBackendDockerVolume, "6 minutes")
	k8s := heldRow(t, ctx, e, "k8s", types.DriveBackendK8sPVC, "6 minutes")

	docker := []string{string(types.DriveBackendDockerVolume), string(types.DriveBackendHostPath)}
	got, err := e.st.ListHeldMemberDriveDeletes(ctx, docker, 50)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for _, d := range got {
		ids = append(ids, d.ID)
	}
	if want := []uuid.UUID{older, old}; !slices.Equal(ids, want) {
		t.Errorf("docker backends listed %v, want the 9- then 6-minute docker rows %v (never the Mac disk %s, the 2-minute %s, the unmarked %s or the k8s row %s)",
			ids, want, mac.ID, young, unmarked, k8s)
	}
	if got, err := e.st.ListHeldMemberDriveDeletes(ctx, []string{string(types.DriveBackendK8sPVC)}, 50); err != nil || len(got) != 1 || got[0].ID != k8s {
		t.Errorf("k8s backends listed %d rows (err %v), want only the 6-minute k8s row", len(got), err)
	}
	if got, err := e.st.ListHeldMemberDriveDeletes(ctx, docker, 1); err != nil || len(got) != 1 || got[0].ID != older {
		t.Errorf("limit 1 listed %d rows (err %v), want the oldest only", len(got), err)
	}
	// A caller that names mac_disk among its backends still never gets a Mac disk: its Mac finishes that delete.
	if got, err := e.st.ListHeldMemberDriveDeletes(ctx, []string{string(types.DriveBackendMacDisk)}, 50); err != nil || len(got) != 0 {
		t.Errorf("mac_disk backend listed %d rows (err %v), want none", len(got), err)
	}
	if got, err := e.st.ListHeldMemberDriveDeletes(ctx, append(slices.Clone(docker), string(types.DriveBackendMacDisk)), 50); err != nil || len(got) != 2 {
		t.Errorf("docker backends plus mac_disk listed %d rows (err %v), want the two docker rows", len(got), err)
	}
	if got, err := e.st.ListHeldMemberDriveDeletes(ctx, nil, 50); err != nil || len(got) != 0 {
		t.Errorf("no backends listed %d rows (err %v), want none", len(got), err)
	}
}

// 51 older held rows of the other target must not fill a batch of 50 and starve the one this server can delete.
func TestPG_ListHeldMemberDriveDeletes_OtherTargetDoesNotFillTheBatch(t *testing.T) {
	e := newRetryEnv(t)
	ctx := context.Background()
	for i := range 51 {
		heldRow(t, ctx, e, fmt.Sprintf("k8s-%02d", i), types.DriveBackendK8sPVC, "10 minutes")
	}
	docker := heldRow(t, ctx, e, "docker", types.DriveBackendDockerVolume, "6 minutes")
	got, err := e.st.ListHeldMemberDriveDeletes(ctx, []string{string(types.DriveBackendDockerVolume), string(types.DriveBackendHostPath)}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != docker {
		t.Errorf("listed %d rows, want exactly the docker row %s", len(got), docker)
	}
}

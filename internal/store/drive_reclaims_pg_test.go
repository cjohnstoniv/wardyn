// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The drive reclaim fence (drive_reclaims) and the runs that hold an object (drive_object_holds) against a
// throwaway Postgres database. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ageDriveReclaim moves an object's claim past the one-minute takeover.
func ageDriveReclaim(t *testing.T, ctx context.Context, pool *pgxpool.Pool, object string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE drive_reclaims SET started_at = now() - interval '61 seconds' WHERE object_name = $1`, object); err != nil {
		t.Fatalf("age claim: %v", err)
	}
}

func wantDriveReclaimFenced(t *testing.T, ctx context.Context, s store.PG, object string, want bool) {
	t.Helper()
	got, err := s.DriveReclaimFenced(ctx, types.DriveBackendDockerVolume, object)
	if err != nil || got != want {
		t.Fatalf("DriveReclaimFenced(%s) = %v, %v; want %v", object, got, err, want)
	}
}

func TestPG_DriveReclaimFence_OneClaimAMinute(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	s := store.NewPG(pool)
	ctx := context.Background()
	const vol = types.DriveBackendDockerVolume
	started, n, err := s.BeginDriveReclaim(ctx, vol, "wardyn-drive-alice", uuid.New())
	if err != nil || n != 0 {
		t.Fatalf("Begin = %v, %d, %v; want a claim over 0", started, n, err)
	}
	wantDriveReclaimFenced(t, ctx, s, "wardyn-drive-alice", true)
	if _, _, err := s.BeginDriveReclaim(ctx, vol, "wardyn-drive-alice", uuid.New()); !errors.Is(err, store.ErrDriveReclaimBusy) {
		t.Fatalf("second Begin err = %v, want ErrDriveReclaimBusy", err)
	}
	wantDriveReclaimFenced(t, ctx, s, "wardyn-drive-alice", true)
	if _, err := pool.Exec(ctx, `UPDATE drive_reclaims SET issues = 1 WHERE object_name = 'wardyn-drive-alice'`); err != nil {
		t.Fatalf("set issues: %v", err)
	}
	ageDriveReclaim(t, ctx, pool, "wardyn-drive-alice")
	started2, n2, err := s.BeginDriveReclaim(ctx, vol, "wardyn-drive-alice", uuid.New())
	if err != nil || n2 != 1 || !started2.After(started) {
		t.Fatalf("takeover Begin = %v, %d, %v; want a later claim keeping issues 1 (first %v)", started2, n2, err, started)
	}
	wantDriveReclaimFenced(t, ctx, s, "wardyn-drive-alice", true)
}

func TestPG_DriveReclaimFence_OneIssuePerClaim(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	s := store.NewPG(pool)
	ctx := context.Background()
	const vol = types.DriveBackendDockerVolume
	started, n, err := s.BeginDriveReclaim(ctx, vol, "obj", uuid.New())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got, err := s.IssueDriveReclaim(ctx, vol, "obj", started, n); err != nil || got != 1 {
		t.Fatalf("Issue = %d, %v; want 1", got, err)
	}
	if _, err := s.IssueDriveReclaim(ctx, vol, "obj", started, n); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second Issue of the same pair err = %v, want ErrNotFound", err)
	}
	ageDriveReclaim(t, ctx, pool, "obj")
	started2, n2, err := s.BeginDriveReclaim(ctx, vol, "obj", uuid.New())
	if err != nil || n2 != 1 {
		t.Fatalf("takeover Begin = %d, %v; want issues 1", n2, err)
	}
	if got, err := s.IssueDriveReclaim(ctx, vol, "obj", started2, n2); err != nil || got != 3 {
		t.Fatalf("takeover Issue = %d, %v; want 3", got, err)
	}
	if _, err := s.IssueDriveReclaim(ctx, vol, "obj", started, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("first holder's late Issue err = %v, want ErrNotFound", err)
	}
}

func TestPG_DriveReclaimFence_SettleRules(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	s := store.NewPG(pool)
	ctx := context.Background()
	const vol = types.DriveBackendDockerVolume
	t.Run("a: clear before an issue", func(t *testing.T) {
		started, n, err := s.BeginDriveReclaim(ctx, vol, "a", uuid.New())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := s.ClearDriveReclaim(ctx, vol, "a", started, n); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "a", false)
	})
	t.Run("b: clear after its own issue was refused", func(t *testing.T) {
		started, n, err := s.BeginDriveReclaim(ctx, vol, "b", uuid.New())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		issued, err := s.IssueDriveReclaim(ctx, vol, "b", started, n)
		if err != nil || issued != 1 {
			t.Fatalf("Issue = %d, %v", issued, err)
		}
		if err := s.ClearDriveReclaimIssued(ctx, vol, "b", started, 0, 1); err != nil {
			t.Fatalf("ClearIssued: %v", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "b", false)
	})
	t.Run("c: a takeover over an odd count never clears", func(t *testing.T) {
		started, n, err := s.BeginDriveReclaim(ctx, vol, "c", uuid.New())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := s.IssueDriveReclaim(ctx, vol, "c", started, n); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		ageDriveReclaim(t, ctx, pool, "c")
		started2, claimN, err := s.BeginDriveReclaim(ctx, vol, "c", uuid.New())
		if err != nil || claimN != 1 {
			t.Fatalf("takeover Begin = %d, %v; want issues 1", claimN, err)
		}
		issued, err := s.IssueDriveReclaim(ctx, vol, "c", started2, claimN)
		if err != nil || issued != 3 {
			t.Fatalf("takeover Issue = %d, %v; want 3", issued, err)
		}
		if err := s.ClearDriveReclaimIssued(ctx, vol, "c", started2, claimN, issued); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ClearIssued over an odd claim err = %v, want ErrNotFound", err)
		}
		if err := s.ClearDriveReclaim(ctx, vol, "c", started2, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Clear over an odd count err = %v, want ErrNotFound", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "c", true)
	})
	t.Run("d: finish on confirmed gone", func(t *testing.T) {
		started, n, err := s.BeginDriveReclaim(ctx, vol, "d", uuid.New())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		issued, err := s.IssueDriveReclaim(ctx, vol, "d", started, n)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if err := s.FinishDriveReclaim(ctx, vol, "d", started, n); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Finish with a stale pair err = %v, want ErrNotFound", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "d", true)
		if err := s.FinishDriveReclaim(ctx, vol, "d", started, issued); err != nil {
			t.Fatalf("Finish: %v", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "d", false)
	})
	t.Run("e: an issued claim never settled stays fenced", func(t *testing.T) {
		started, n, err := s.BeginDriveReclaim(ctx, vol, "e", uuid.New())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := s.IssueDriveReclaim(ctx, vol, "e", started, n); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		wantDriveReclaimFenced(t, ctx, s, "e", true)
	})
}

func TestPG_DriveObjectHolds_CountWhileTheRunLives(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	s := store.NewPG(pool)
	ctx := context.Background()
	const vol = types.DriveBackendDockerVolume
	held := func(backend types.DriveBackend, object string) bool {
		t.Helper()
		got, err := s.DriveObjectHeld(ctx, backend, object)
		if err != nil {
			t.Fatalf("DriveObjectHeld: %v", err)
		}
		return got
	}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := s.HoldDriveObject(ctx, vol, "obj", run.ID); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !held(vol, "obj") {
		t.Fatal("a RUNNING run's hold is not counted")
	}
	if err := s.HoldDriveObject(ctx, vol, "obj", run.ID); err != nil {
		t.Fatalf("second Hold of the same triple: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'FAILED' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("end run: %v", err)
	}
	if held(vol, "obj") {
		t.Fatal("an ended run's hold is still counted")
	}
	starting := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := s.HoldDriveObject(ctx, vol, "obj", starting.ID); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if !held(vol, "obj") {
		t.Fatal("a STARTING run's hold is not counted")
	}
	if held(vol, "other") || held(types.DriveBackendK8sPVC, "obj") {
		t.Fatal("a hold counts for another object name or another backend")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, starting.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM drive_object_holds WHERE run_id = $1`, starting.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("holds of a deleted run = %d, %v; want 0", rows, err)
	}
}

// TestPG_DriveReclaimFence_HoldAndClaimSeeEachOther runs the two orders the reclaim and dispatch use, one after the
// other. Each write and each read is its own autocommit statement under READ COMMITTED: a run commits its hold and then
// reads the fence, and a reclaim commits its claim and then reads the holds. If the run's fence read misses the claim,
// the claim committed after that read began, so the hold had already committed before the reclaim's hold read; of two
// that overlap, at least one sees the other.
func TestPG_DriveReclaimFence_HoldAndClaimSeeEachOther(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	s := store.NewPG(pool)
	ctx := context.Background()
	const vol = types.DriveBackendDockerVolume

	// (A) the hold first: the reclaim's hold read sees it.
	run := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := s.HoldDriveObject(ctx, vol, "a", run.ID); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if _, _, err := s.BeginDriveReclaim(ctx, vol, "a", uuid.New()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got, err := s.DriveObjectHeld(ctx, vol, "a"); err != nil || !got {
		t.Fatalf("Held after hold then claim = %v, %v; want true", got, err)
	}

	// (B) the claim first: the run's fence read sees it.
	if _, _, err := s.BeginDriveReclaim(ctx, vol, "b", uuid.New()); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	run2 := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := s.HoldDriveObject(ctx, vol, "b", run2.ID); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if got, err := s.DriveReclaimFenced(ctx, vol, "b"); err != nil || !got {
		t.Fatalf("Fenced after claim then hold = %v, %v; want true", got, err)
	}
}

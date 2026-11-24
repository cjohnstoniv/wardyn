// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// retryReclaimer is the keep env's runner counting every reclaim and answering outcome ("" is a reply that names none, the
// unknown outcome that keeps a row held).
type retryReclaimer struct {
	*keepPoolRunner
	mu      sync.Mutex
	mounts  []types.DriveMount
	outcome runner.DriveReclaimOutcome
}

func (r *retryReclaimer) ReclaimDrive(_ context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mounts = append(r.mounts, m)
	return r.outcome, nil
}

func (r *retryReclaimer) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.mounts)
}

func (r *retryReclaimer) answer(o runner.DriveReclaimOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcome = o
}

// retryReplica is another server over the env's database, with its own runner and store, sweeping as leader or follower.
func (e *queueEnv) retryReplica(st store.Store, rn runner.Runner, leader bool) *Server {
	cfg := e.srv.cfg
	cfg.Store, cfg.Runner, cfg.SweeperLease = st, rn, staticLease{leader: leader}
	return New(cfg)
}

// retryHeld is owner's drive on backend whose delete was marked age ago ("" leaves it unmarked) with issues calls counted.
func (e *queueEnv) retryHeld(owner, name string, backend types.DriveBackend, age string, issues int) uuid.UUID {
	e.t.Helper()
	d, err := e.pg.CreateMemberDrive(context.Background(), types.UserDrive{Name: name, Backend: backend, OwnerSubject: owner, CreatedBy: owner}, 200)
	if err != nil {
		e.t.Fatalf("create %s: %v", name, err)
	}
	if age != "" {
		e.exec(`UPDATE user_drives SET delete_started_at = now() - $2::interval, delete_issues = $3 WHERE id = $1`, d.ID, age, issues)
	}
	return d.ID
}

func (e *queueEnv) driveExists(id uuid.UUID) bool {
	e.t.Helper()
	var n int
	if err := e.poolEnv.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_drives WHERE id = $1`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// retryCatalogue is the person's GET /me/drives.
func (e *queueEnv) retryCatalogue() client.MeDriveCatalogue {
	e.t.Helper()
	w := e.call(e.person, http.MethodGet, "/api/v1/me/drives", "")
	var cat client.MeDriveCatalogue
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &cat) != nil {
		e.t.Fatalf("GET /me/drives = %d %s", w.Code, w.Body)
	}
	return cat
}

// takeoverRecorder counts the takeovers the store refused.
type takeoverRecorder struct {
	store.PG
	mu       sync.Mutex
	notFound int
}

func (s *takeoverRecorder) BeginMemberDriveTakeover(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	d, at, err := s.PG.BeginMemberDriveTakeover(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.mu.Lock()
		s.notFound++
		s.mu.Unlock()
	}
	return d, at, err
}

// Two replicas that both listed the same held row (one on a stale lease) make one call between them: the second takeover
// finds the mark the first one re-stamped. The barrier holds both sweeps after their list, so they race at the takeover.
func TestPG_HeldDriveRetry_TwoSweepersOneCall(t *testing.T) {
	e, _ := keepEnv(t, 3)
	ctx := context.Background()
	rc := &retryReclaimer{keepPoolRunner: &keepPoolRunner{e.runner}}
	ra, rb := &takeoverRecorder{PG: e.pg}, &takeoverRecorder{PG: e.pg}
	a, b := e.retryReplica(ra, rc, true), e.retryReplica(rb, rc, true)
	owner := types.CanonicalUserSubject(e.personP)
	id := e.retryHeld(owner, "Held", types.DriveBackendDockerVolume, "6 minutes", 1)

	listed, release := make(chan struct{}, 2), make(chan struct{})
	prev := heldDrivesListed
	heldDrivesListed = func() { listed <- struct{}{}; <-release }
	t.Cleanup(func() { heldDrivesListed = prev })

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*Server{a, b} {
		wg.Go(func() { errs[i] = s.sweepHeldMemberDriveDeletes(ctx) })
	}
	for range 2 {
		select {
		case <-listed:
		case <-time.After(30 * time.Second):
			close(release)
			t.Fatal("both sweeps did not list the held row")
		}
	}
	close(release)
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("sweep errors: %v, %v", errs[0], errs[1])
	}
	if got := rc.calls(); got != 1 {
		t.Errorf("ReclaimDrive calls = %d, want exactly 1 between the two sweepers", got)
	}
	if got := ra.notFound + rb.notFound; got != 1 {
		t.Errorf("takeovers answered ErrNotFound = %d, want exactly 1 (the loser's)", got)
	}
	if marked, n := e.fenceMarkOf(id); !marked || n != 3 {
		t.Errorf("the row after the race: marked = %v, count = %d; want held at 3, the count the one issue returned", marked, n)
	}
}

// 55 older held rows of another runner target must not starve the one a Docker server can delete: the first pass calls the
// reclaim for the Docker row and takes over no Kubernetes row.
func TestPG_HeldDriveRetry_OtherTargetRowsDoNotStarve(t *testing.T) {
	e, _ := keepEnv(t, 3)
	ctx := context.Background()
	rc := &retryReclaimer{keepPoolRunner: &keepPoolRunner{e.runner}, outcome: runner.DriveReclaimDeleted}
	e.srv.cfg.Runner = rc
	owner := types.CanonicalUserSubject(e.personP)
	for i := range 55 {
		e.retryHeld(owner, fmt.Sprintf("k8s-%02d", i), types.DriveBackendK8sPVC, "10 minutes", 1)
	}
	docker := e.retryHeld(owner, "Docker", types.DriveBackendDockerVolume, "6 minutes", 1)

	if err := e.srv.sweepHeldMemberDriveDeletes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rc.calls() != 1 || rc.mounts[0].Backend != types.DriveBackendDockerVolume || rc.mounts[0].DriveID != docker {
		t.Errorf("reclaim calls = %+v, want one, for the docker row %s", rc.mounts, docker)
	}
	if e.driveExists(docker) {
		t.Error("the docker row is still there after the sweep")
	}
	var stamped int
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT count(*) FROM user_drives WHERE backend = 'k8s_pvc' AND delete_started_at < now() - interval '9 minutes'`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 55 {
		t.Errorf("kubernetes rows still marked ten minutes ago = %d, want 55 (the sweep took none over)", stamped)
	}
}

// A kept drive whose cleanup never ran is finished by the sweep. A cleanup that runs after the takeover finds the claim
// re-stamped and makes no call.
func TestPG_HeldDriveRetry_KeptDriveCleanupNeverRan(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	rc := &retryReclaimer{keepPoolRunner: &keepPoolRunner{e.runner}}
	e.srv.cfg.Runner = rc
	run, drive := e.fenceKeptRun(pool)
	e.fenceEndKept(run) // the claim is taken in the statement that ends the run; the cleanup never runs
	at := time.Now().Add(-6 * time.Minute).Truncate(time.Microsecond)
	e.exec(`UPDATE agent_runs SET ended_at = $2 WHERE id = $1`, run.ID, at)
	e.exec(`UPDATE user_drives SET delete_started_at = $2 WHERE id = $1`, drive, at)

	if err := e.srv.sweepHeldMemberDriveDeletes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if marked, n := e.fenceMarkOf(drive); rc.calls() != 1 || !marked || n != 1 {
		t.Fatalf("after a sweep that got no answer: calls = %d, marked = %v, count = %d; want 1, held at 1", rc.calls(), marked, n)
	}
	e.srv.finishQueuedKeptDrive(ctx, run.ID) // the cleanup that never ran, running late: the claim is no longer its run's end
	if rc.calls() != 1 {
		t.Errorf("calls after the late cleanup = %d, want 1: it found the claim re-stamped", rc.calls())
	}

	rc.answer(runner.DriveReclaimDeleted)
	e.exec(`UPDATE user_drives SET delete_started_at = now() - interval '6 minutes' WHERE id = $1`, drive)
	if err := e.srv.sweepHeldMemberDriveDeletes(ctx); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if e.keptDriveExists(run) || rc.calls() != 2 {
		t.Errorf("after the next sweep: exists = %v, calls = %d; want the kept drive gone by the second call", e.keptDriveExists(run), rc.calls())
	}
}

// A drive the sweep finishes leaves the person's count.
func TestPG_HeldDriveRetry_FinishedRowLeavesCount(t *testing.T) {
	e, _ := keepEnv(t, 3)
	ctx := context.Background()
	rc := &retryReclaimer{keepPoolRunner: &keepPoolRunner{e.runner}, outcome: runner.DriveReclaimDeleted}
	e.srv.cfg.Runner = rc
	owner := types.CanonicalUserSubject(e.personP)
	held := e.retryHeld(owner, "Held", types.DriveBackendDockerVolume, "6 minutes", 1)
	e.retryHeld(owner, "Other", types.DriveBackendDockerVolume, "", 0)

	before := e.retryCatalogue()
	var deleting bool
	for _, d := range before.Drives {
		deleting = deleting || d.ID == held.String() && d.UnavailableReason == client.MeDriveUnavailableDeleting
	}
	if before.Create == nil || before.Create.Count != 2 || !deleting {
		t.Fatalf("before the sweep: create = %+v, held row listed as deleting = %v; want count 2 and the held row Deleting", before.Create, deleting)
	}
	if err := e.srv.sweepHeldMemberDriveDeletes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if after := e.retryCatalogue(); after.Create == nil || after.Create.Count != 1 {
		t.Errorf("after the sweep: create = %+v, want count 1", after.Create)
	}
}

// startingOwner is a store whose owner always has a run starting, so a delete is refused before any issue.
type startingOwner struct{ store.PG }

func (startingOwner) MemberDriveOwnerStarting(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

// A sweep that is refused before any issue only re-stamps the mark: the count stays even, nothing is called, and the
// cleanup that never ran, running late, finds the claim is no longer its run's end and returns.
func TestPG_HeldDriveRetry_RefusedBeforeAnyIssueOnlyRestampsTheMark(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	rc := &retryReclaimer{keepPoolRunner: &keepPoolRunner{e.runner}, outcome: runner.DriveReclaimDeleted}
	e.srv.cfg.Runner = rc
	run, drive := e.fenceKeptRun(pool)
	e.fenceEndKept(run)
	at := time.Now().Add(-6 * time.Minute).Truncate(time.Microsecond)
	e.exec(`UPDATE agent_runs SET ended_at = $2 WHERE id = $1`, run.ID, at)
	e.exec(`UPDATE user_drives SET delete_started_at = $2 WHERE id = $1`, drive, at)

	if err := e.retryReplica(startingOwner{e.pg}, rc, true).sweepHeldMemberDriveDeletes(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var restamped bool
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT delete_started_at > now() - interval '1 minute' FROM user_drives WHERE id = $1`, drive).Scan(&restamped); err != nil {
		t.Fatal(err)
	}
	if marked, n := e.fenceMarkOf(drive); !marked || n != 0 || !restamped || rc.calls() != 0 {
		t.Fatalf("after the refused sweep: marked = %v, count = %d, re-stamped = %v, calls = %d; want marked at the even count 0, re-stamped, no call", marked, n, restamped, rc.calls())
	}
	e.srv.finishQueuedKeptDrive(ctx, run.ID)
	if marked, n := e.fenceMarkOf(drive); !marked || n != 0 || rc.calls() != 0 || !e.keptDriveExists(run) {
		t.Errorf("after the late cleanup: marked = %v, count = %d, calls = %d, exists = %v; want it to return at the re-stamp check, touching nothing", marked, n, rc.calls(), e.keptDriveExists(run))
	}
}

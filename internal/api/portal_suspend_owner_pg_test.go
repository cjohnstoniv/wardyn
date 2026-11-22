// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: who owns a suspension's kill claim, how a kill claimed from the queue is repaired, the owner of a
// pinned payload's key, a pin on a finished step and the owed refusal rows a portal sweep drains.

// claimedKill is a portal suspension's step one and the ledger row of one run, ready to claim.
type claimedKill struct {
	id    uuid.UUID
	key   store.JobKey
	bound time.Time
	run   uuid.UUID
	forms leaverForms
}

func killRowOf(t *testing.T, l *portalLife, principal string, run uuid.UUID) claimedKill {
	t.Helper()
	ctx := context.Background()
	id := l.ident(t, principal).ID
	first := portalOrigin(types.ActorHuman, "first-admin")
	ident, forms, _, err := l.h.srv.portalSuspendStepOne(withActor(ctx, first.actorType, first.actor), l.st, id, first, 0, nil)
	if err != nil {
		t.Fatalf("step one: %v", err)
	}
	key := store.JobKey{Step: jobStepKillRun, Target: run.String()}
	if err := l.st.EnsureSuspensionJobsAt(ctx, id, []store.JobKey{key}, *ident.DeactivatedAt); err != nil {
		t.Fatal(err)
	}
	jobs, err := l.st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := jobByKey(jobs, key.Step, key.Target)
	key.ID = j.ID
	return claimedKill{id: id, key: key, bound: *ident.DeactivatedAt, run: run, forms: forms}
}

// A kill claim belongs to the acquisition that made it. A worker that acquired nothing (it failed before it
// reached the claim) releases nothing, so the live claim of another worker stands and a third worker is refused;
// a worker whose lease ended and was taken over neither releases nor finishes its successor's row, while the
// successor does, once the receipt is stored.
func TestPortalSuspend_KillClaimBelongsToItsAcquisition_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	c := killRowOf(t, l, principal, run)
	sweep := portalSweepOrigin()
	claim := func(from types.RunState, claimID uuid.UUID) store.KillClaim {
		got, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run, From: from, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: claimID})
		if err != nil {
			t.Fatalf("claim from %s: %v", from, err)
		}
		return got
	}
	held := func() bool {
		return l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE id = $1 AND kill_claimed_at IS NOT NULL`, c.key.ID) == 1
	}
	a := claim(types.RunRunning, uuid.New())
	if !a.Applied {
		t.Fatal("control: the first worker's claim was not applied")
	}
	// A worker that failed before acquiring a claim has none to release; nor does one that names another's.
	for _, who := range []uuid.UUID{uuid.Nil, uuid.New()} {
		if err := l.st.ReleaseSuspensionKillAt(ctx, c.id, c.key, who); err != nil {
			t.Fatal(err)
		}
	}
	if !held() {
		t.Fatal("a release that was not the holder's ended the claim")
	}
	kills := l.run.killCount()
	sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), c.id, c.bound)
	if err := l.h.srv.killStep(sctx, sweep, l.st, c.id, c.forms.targets); !errors.Is(err, store.ErrKillHeld) || l.run.killCount() != kills {
		t.Fatalf("a third worker beside the live claim = %v with %d runner calls, want ErrKillHeld and none", err, l.run.killCount()-kills)
	}
	// The first worker's lease ends and a second worker takes the claim over; the first, still running, is fenced.
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, c.key.ID); err != nil {
		t.Fatal(err)
	}
	b := claim(types.RunKilled, uuid.New())
	if !b.Applied || b.ClaimID == a.ClaimID {
		t.Fatalf("the takeover = %+v, want applied under its own claim", b)
	}
	if err := l.st.ReleaseSuspensionKillAt(ctx, c.id, c.key, a.ClaimID); err != nil || !held() {
		t.Fatalf("the expired worker's release = %v, claim still held: %v; want its release to leave the successor's claim", err, held())
	}
	late := c.key
	late.Claim = a.ClaimID
	ledgerOf := func() string {
		var out string
		if err := l.pool.QueryRow(ctx, `SELECT state || '|' || attempts || '|' || last_error FROM deprovision_jobs WHERE id = $1`, c.key.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, late, nil, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("the expired worker's finish = %v, want ErrKillHeld", err)
	}
	// While the successor owns the row, a failure of the expired worker's, and one from a worker that acquired no
	// claim at all (it failed before the claim), mark nothing.
	before := ledgerOf()
	for who, key := range map[string]store.JobKey{"expired": late, "unclaimed": c.key} {
		if err := l.st.FailDeprovisionJob(ctx, c.id, store.JobKindSuspend, key, errors.New("late")); !errors.Is(err, store.ErrKillHeld) {
			t.Errorf("the %s worker's failure beside the live claim = %v, want ErrKillHeld", who, err)
		}
	}
	if got := ledgerOf(); got != before {
		t.Errorf("a failure that did not own the row changed it: %q -> %q", before, got)
	}
	// The successor's attempt fails and releases its claim: the row is unowned, which is no one's to finish.
	own := c.key
	own.Claim = b.ClaimID
	if err := l.st.ReleaseSuspensionKillAt(ctx, c.id, c.key, b.ClaimID); err != nil || held() {
		t.Fatalf("the successor's release = %v, still held: %v", err, held())
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, late, nil, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("the expired worker's finish after the successor released = %v, want ErrKillHeld", err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, own, nil, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("the released attempt's own finish = %v, want ErrKillHeld", err)
	}
	// The retry takes a new claim, stores the receipt and finishes.
	retry := claim(types.RunKilled, uuid.New())
	if !retry.Applied || retry.ReceiptID != b.ReceiptID {
		t.Fatalf("the retry's claim = %+v, want applied under the occurrence's receipt", retry)
	}
	own.Claim = retry.ClaimID
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, own, nil, c.bound); !errors.Is(err, store.ErrReceiptNotStored) {
		t.Errorf("the retry's finish before the receipt is stored = %v, want ErrReceiptNotStored", err)
	}
	ev := types.AuditEvent{ID: retry.ReceiptID, Time: retry.ReceiptAt, RunID: &run, ActorType: types.ActorSystem, Actor: portalLifecycleActor, Action: "run.kill", Target: run.String(), Outcome: "success"}
	if err := (store.Recorder{Pool: l.pool}).Record(store.WithAuditReplay(ctx), ev); err != nil {
		t.Fatal(err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, own, nil, c.bound); err != nil {
		t.Errorf("the retry's finish with the receipt stored = %v, want nil", err)
	}
	// After the finish, a late failure from either worker writes nothing.
	done := ledgerOf()
	for who, key := range map[string]store.JobKey{"expired": late, "unclaimed": c.key} {
		if err := l.st.FailDeprovisionJob(ctx, c.id, store.JobKindSuspend, key, errors.New("late")); err != nil {
			t.Errorf("the %s worker's failure after the finish = %v, want nil and no write", who, err)
		}
	}
	if got := ledgerOf(); got != done {
		t.Errorf("a failure after the finish changed the row: %q -> %q", done, got)
	}
}

// madeAfterReactivation makes the run one a reactivated person made after the suspension's bound: the person is active
// again and the database stamped the run later than the bound, so the tail's run selection spares it.
func madeAfterReactivation(t *testing.T, l *portalLife, principal string, c claimedKill) {
	t.Helper()
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != 200 {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if _, err := l.pool.Exec(context.Background(), `UPDATE agent_runs SET inserted_at = clock_timestamp() + interval '1 day' WHERE id = $1`, c.run); err != nil {
		t.Fatal(err)
	}
	spared, err := l.st.RunsInsertedAfter(context.Background(), c.id, []uuid.UUID{c.run}, c.bound)
	if err != nil || !spared[c.run] {
		t.Fatalf("control: the run is not one the selection spares (%v, %v)", spared, err)
	}
}

// jobOf is the kill row's state, claim and receipt columns and its recorded kill count, for comparing before and after.
func jobOf(t *testing.T, l *portalLife, id uuid.UUID) string {
	t.Helper()
	var out string
	if err := l.pool.QueryRow(context.Background(), `SELECT state || '|' || attempts || '|' || last_error || '|killed=' || COALESCE(detail->>'runs_killed', '-') || '|pinned=' || (audit_event_id IS NOT NULL)::text
		FROM deprovision_jobs WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A worker that applied a kill and died has killed the run, whatever its lease says: the claim pinned the run.kill
// receipt in the transaction that moved the run. When the person has been reactivated and the run is one the selection
// would now spare, the next pass still owes that kill: it takes the row over, finishes the teardown, stores the same
// receipt once and counts one kill, instead of closing the row with no kill and no receipt.
func TestPortalSuspend_AppliedKillOfALostWorkerIsRecoveredAfterReactivation_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	c := killRowOf(t, l, principal, run)
	lost, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run, From: types.RunRunning, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()})
	if err != nil || !lost.Applied {
		t.Fatalf("the lost worker's claim = %+v, %v", lost, err)
	}
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, c.key.ID); err != nil {
		t.Fatal(err)
	}
	madeAfterReactivation(t, l, principal, c)
	sweep := portalSweepOrigin()
	sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), c.id, c.bound)
	if err := l.h.srv.killStep(sctx, sweep, l.st, c.id, c.forms.targets); err != nil {
		t.Fatalf("the replacement pass: %v", err)
	}
	if got := jobOf(t, l, c.key.ID); got != "done|1||killed=1|pinned=true" {
		t.Errorf("row after the replacement = %q, want done under one kill with the receipt still pinned", got)
	}
	if n := l.count(t, `SELECT count(*) FROM audit_events WHERE id = $1 AND action = 'run.kill'`, lost.ReceiptID); n != 1 {
		t.Errorf("the pinned receipt is stored %d times, want once", n)
	}
}

// A run nobody claimed that a reactivated person made after the bound is owed no kill: its row closes with no teardown,
// no receipt and a count of zero, and the run is left alone.
func TestPortalSuspend_UntouchedRunMadeAfterReactivationIsSparedReceiptless_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	c := killRowOf(t, l, principal, run)
	madeAfterReactivation(t, l, principal, c)
	kills := l.run.killCount()
	sweep := portalSweepOrigin()
	sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), c.id, c.bound)
	if err := l.h.srv.killStep(sctx, sweep, l.st, c.id, c.forms.targets); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	if got := jobOf(t, l, c.key.ID); got != "done|1||killed=0|pinned=false" {
		t.Errorf("row after the pass = %q, want done, zero kills, no receipt", got)
	}
	if got := l.runState(t, run); got != types.RunRunning || l.run.killCount() != kills {
		t.Errorf("the spared run is %s after %d runner calls, want RUNNING and none", got, l.run.killCount()-kills)
	}
}

// Once a kill was applied, only a holder of a claim closes its row, and only while the row is pending: the former
// claimant's late finish after another worker took over is refused and leaves the final detail as it was; a claim-less
// finish (the spared path) of a row that pins a kill is refused whether the lease holds, ran out or was released; and a
// failure that named no claim is recorded once the lease ran out.
func TestPortalSuspend_FinishAfterATakeoverIsFencedToTheNewClaim_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	c := killRowOf(t, l, principal, run)
	claim := func(from types.RunState) store.KillClaim {
		got, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run, From: from, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()})
		if err != nil || !got.Applied {
			t.Fatalf("claim from %s = %+v, %v", from, got, err)
		}
		return got
	}
	age := func() {
		if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, c.key.ID); err != nil {
			t.Fatal(err)
		}
	}
	first := claim(types.RunRunning)
	spared := c.key // the spared path names no claim
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, spared, map[string]int{"runs_killed": 0}, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less finish inside the lease = %v, want ErrKillHeld", err)
	}
	age()
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, spared, map[string]int{"runs_killed": 0}, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less finish after the lease ran out = %v, want ErrKillHeld", err)
	}
	if err := l.st.FailDeprovisionJob(ctx, c.id, store.JobKindSuspend, spared, errors.New("before any claim")); err != nil {
		t.Errorf("a claim-less failure after the lease ran out = %v, want it recorded", err)
	}
	if got := jobOf(t, l, c.key.ID); got != "pending|1|before any claim|killed=-|pinned=true" {
		t.Errorf("row after the failure = %q, want pending, one attempt, the receipt still pinned", got)
	}
	// The claim is released after a failed teardown: the row has no claim, and still pins the kill it applied.
	if err := l.st.ReleaseSuspensionKillAt(ctx, c.id, c.key, first.ClaimID); err != nil {
		t.Fatal(err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, spared, map[string]int{"runs_killed": 0}, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less finish of a released row that applied a kill = %v, want ErrKillHeld", err)
	}
	// The replacement takes the repair claim, stores the receipt and finishes with its count.
	second := claim(types.RunKilled)
	ev := types.AuditEvent{ID: second.ReceiptID, Time: second.ReceiptAt, RunID: &run, ActorType: types.ActorSystem, Actor: portalLifecycleActor, Action: "run.kill", Target: run.String(), Outcome: "success"}
	if err := (store.Recorder{Pool: l.pool}).Record(store.WithAuditReplay(ctx), ev); err != nil {
		t.Fatal(err)
	}
	newKey := c.key
	newKey.Claim = second.ClaimID
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, newKey, map[string]int{"runs_killed": 1}, c.bound); err != nil {
		t.Fatalf("the replacement's finish = %v", err)
	}
	done := jobOf(t, l, c.key.ID)
	if want := "done|2||killed=1|pinned=true"; done != want {
		t.Fatalf("row after the replacement = %q, want %q", done, want)
	}
	// The former claimant finishes late with a different count; the replacement's own claim cannot finish twice either.
	oldKey := c.key
	oldKey.Claim = first.ClaimID
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, oldKey, map[string]int{"runs_killed": 0}, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("the former claimant's late finish = %v, want ErrKillHeld", err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, newKey, map[string]int{"runs_killed": 9}, c.bound); err != nil {
		t.Errorf("a second finish under the same claim = %v, want nil and no write", err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, spared, map[string]int{"runs_killed": 0}, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less finish of a finished kill row = %v, want ErrKillHeld", err)
	}
	if got := jobOf(t, l, c.key.ID); got != done {
		t.Errorf("a late finish changed the finished row: %q -> %q", done, got)
	}
}

// enqueueWithDrive enqueues a run for principal as the enqueue path does: its run row, its queue row and the member
// drive it keeps its work folder in, in one transaction.
func enqueueWithDrive(t *testing.T, l *portalLife, principal string) (types.AgentRun, types.UserDrive) {
	t.Helper()
	ctx := context.Background()
	maxRuns := 4
	pool, err := l.st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Queue " + uuid.NewString()[:8], Scope: types.RunnerPoolOrganization,
		Limits: &types.RunnerPoolLimits{
			Background: &types.RunnerPoolBackgroundLimits{CPUMillis: &types.RunnerPoolAmount{Default: 500, Cap: 2000}, MemoryMiB: &types.RunnerPoolAmount{Default: 512, Cap: 4096},
				Lifetime: &types.RunnerPoolDuration{DefaultSec: 3600, Unlimited: true}},
			Interactive: &types.RunnerPoolInteractiveLimits{CPUMillis: &types.RunnerPoolAmount{Default: 1000, Cap: 4000}, MemoryMiB: &types.RunnerPoolAmount{Default: 1024, Cap: 8192},
				Lifetime: &types.RunnerPoolDuration{DefaultSec: 1800, MaxSec: 28800}, Idle: &types.RunnerPoolDuration{DefaultSec: 600, MaxSec: 3600}},
			Barriers: []types.ConfinementClass{types.CC1, types.CC2}, MaxConcurrentRuns: &maxRuns,
		}}, "test")
	if err != nil {
		t.Fatalf("create a pool: %v", err)
	}
	now := time.Now().UTC()
	run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: principal, Agent: "claude-code", ConfinementClass: types.CC2, State: types.RunQueued,
		RunnerTarget: "docker", RunnerPoolID: &pool.ID, RunnerPoolRevision: pool.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit}
	drive := types.UserDrive{ID: keptWorkFolderDriveID(run.ID), Name: "Kept " + run.ID.String()[:8], Backend: types.DriveBackendDockerVolume, OwnerSubject: principal, CreatedBy: principal, KeptFromRun: &run.ID}
	if _, err := l.st.CreateQueuedRun(ctx, run, store.RunQueueEntry{PoolID: pool.ID, Owner: principal, QueuedAt: now, DeadlineAt: now.Add(time.Hour), KeptDrive: &drive, KeptDriveLimit: 10}, 0); err != nil {
		t.Fatalf("enqueue a run with its kept drive: %v", err)
	}
	return run, drive
}

// reclaimingPortalRunner is the portal fixture's runner able to delete a drive, so the repair of a queued kill can
// finish the delete of the drive the run kept (memberDrivesDeletable).
type reclaimingPortalRunner struct{ *scimRunner }

func (*reclaimingPortalRunner) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimAlreadyAbsent, nil
}

func (*reclaimingPortalRunner) CanManageDrives(context.Context) error { return nil }

// A suspension's kill of a run that is still waiting leaves the queue through the one queue exit, so the run's end
// and the claim on the drive it kept commit together (store.LeaveRunQueueIn). A claim from a stale bound or one
// that rolls back changes nothing. Once it commits the drive is out of every run's reach, admission is refused as
// drive_not_found, and the repair of the kill, which stops no sandbox for a run that never started, finishes the
// drive's delete. The run and its drive are made as an enqueue makes them.
func TestPortalSuspend_QueuedKillIsRepairedAsQueued_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.h.srv.cfg.Runner = &reclaimingPortalRunner{l.run}
	l.h.srv.cfg.RunnerTarget = "docker"
	principal, _ := l.signIn(t, "u1")
	run, drive := enqueueWithDrive(t, l, principal)
	c := killRowOf(t, l, principal, run.ID)
	queueRows := func() int { return l.count(t, `SELECT count(*) FROM run_queue WHERE run_id = $1`, run.ID) }
	driveRows := func() int { return l.count(t, `SELECT count(*) FROM user_drives WHERE id = $1`, drive.ID) }
	// claimOf is the drive's claim: when it was taken and the count of deletes issued, beside the run's end.
	claimOf := func() (started *time.Time, issues int, ended *time.Time) {
		t.Helper()
		if err := l.pool.QueryRow(ctx, `SELECT d.delete_started_at, d.delete_issues, r.ended_at
			FROM user_drives d JOIN agent_runs r ON r.id = d.kept_from_run WHERE d.id = $1`, drive.ID).Scan(&started, &issues, &ended); err != nil {
			t.Fatalf("read the drive's claim: %v", err)
		}
		return started, issues, ended
	}
	admit := func() *runRefusal {
		_, f := authorizeDrives(l.h.srv, personCtx(principal), drivesRequest(drive.ID.String()))
		return f
	}
	untouched := func(what string) {
		t.Helper()
		started, issues, ended := claimOf()
		if l.runState(t, run.ID) != types.RunQueued || queueRows() != 1 || started != nil || issues != 0 || ended != nil {
			t.Errorf("%s: run %s with %d queue rows, claim %v over %d issues, ended %v; want QUEUED, 1 row, no claim, 0 issues, not ended",
				what, l.runState(t, run.ID), queueRows(), started, issues, ended)
		}
	}
	// Control: the waiting run holds its drive (listed, held) and no claim stands.
	reach, err := l.h.srv.reachableDrives(personCtx(principal), 0)
	if err != nil || !slices.ContainsFunc(reach, func(d reachableDrive) bool { return d.winner.Drive.ID == drive.ID && d.held }) {
		t.Fatalf("control: the owner's reach = %d drives (%v), want the waiting run's drive held", len(reach), err)
	}
	untouched("control")
	job := jobOf(t, l, c.key.ID)
	kill := store.RunKill{RunID: run.ID, From: types.RunQueued, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()}
	// A claim bound to another instant is superseded before it writes anything.
	if _, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound.Add(-time.Microsecond), kill); !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("a claim from a stale bound = %v, want ErrSuperseded", err)
	}
	untouched("a stale claim")
	// A claim whose last write fails rolls back the queue exit with it: the run, its row and the drive's claim stay.
	trigger := "refuse_pin_" + strings.ReplaceAll(run.ID.String(), "-", "_")
	for _, q := range []string{
		`CREATE FUNCTION ` + trigger + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test: the pin fails'; END $$`,
		`CREATE TRIGGER ` + trigger + ` BEFORE UPDATE ON deprovision_jobs FOR EACH ROW
			WHEN (NEW.id = '` + c.key.ID.String() + `' AND NEW.kill_claim_id IS NOT NULL) EXECUTE FUNCTION ` + trigger + `()`,
	} {
		if _, err := l.pool.Exec(ctx, q); err != nil {
			t.Fatalf("plant the failing pin: %v", err)
		}
	}
	_, err = l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, kill)
	for _, q := range []string{`DROP TRIGGER ` + trigger + ` ON deprovision_jobs`, `DROP FUNCTION ` + trigger + `()`} {
		if _, derr := l.pool.Exec(ctx, q); derr != nil {
			t.Fatalf("remove the failing pin: %v", derr)
		}
	}
	if err == nil {
		t.Fatal("a claim whose pin failed = nil, want its error")
	}
	untouched("a claim that rolled back")
	if got := jobOf(t, l, c.key.ID); got != job {
		t.Errorf("a stale and a rolled-back claim changed the job: %q -> %q", job, got)
	}
	got, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, kill)
	if err != nil || !got.Applied {
		t.Fatalf("the worker's claim from the queue = %+v, %v", got, err)
	}
	// The committed kill left the queue and took the claim in one statement: the claim carries the run's end and
	// the issue count is still even.
	started, issues, ended := claimOf()
	if queueRows() != 0 || l.runState(t, run.ID) != types.RunKilled || started == nil || ended == nil || !started.Equal(*ended) || issues%2 != 0 {
		t.Fatalf("after the claim: %d queue rows, run %s, claim %v over %d issues, ended %v; want 0 rows, KILLED, claim equal to the end, even issues",
			queueRows(), l.runState(t, run.ID), started, issues, ended)
	}
	// Before the repair the drive is out of reach: the attach is refused 422 drive_not_found, the id as sent.
	if f := admit(); f == nil || f.status != http.StatusUnprocessableEntity || f.body.Reason != string(authz.ReasonDriveNotFound) || f.body.Error != driveNotFoundMsg(drive.ID.String()) {
		t.Errorf("admission before the repair = %+v, want 422 drive_not_found %q", f, driveNotFoundMsg(drive.ID.String()))
	}
	if driveRows() != 1 {
		t.Fatal("control: the stopped worker's drive was dropped before any repair")
	}
	// The worker stopped; its lease is over; the sweep repairs.
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, c.key.ID); err != nil {
		t.Fatal(err)
	}
	sweep := portalSweepOrigin()
	sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), c.id, c.bound)
	if err := l.h.srv.killStep(sctx, sweep, l.st, c.id, c.forms.targets); err != nil {
		t.Fatalf("repair: %v", err)
	}
	var data string
	var n int
	if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(data->>'queued'), '') FROM audit_events WHERE action = 'run.kill' AND id = $1`, kill.ReceiptID).Scan(&n, &data); err != nil {
		t.Fatal(err)
	}
	if n != 1 || data != "true" {
		t.Errorf("run.kill rows under the pinned id = %d with queued=%q, want 1 with queued=true", n, data)
	}
	if driveRows() != 0 {
		t.Error("the run's kept drive is still there after the repair, want its delete finished")
	}
}

// A pin on a step another worker already finished writes nothing: the finish cleared the payload, and a worker
// that read the step pending and pinned late neither restores a payload nor changes the event the row names. The
// finished row either names the event it was pinned to or, when a legacy finish left no receipt (0196 allows the
// state), names none; both come back Done, and neither takes the retry's payload.
func TestPortalSuspend_PinOnAFinishedStepStoresNothing_PG(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
	}{{"pinned finish", true}, {"legacy receiptless finish", false}} {
		t.Run(tc.name, func(t *testing.T) {
			l := newPortalLife(t)
			ctx := context.Background()
			principal, _ := l.signIn(t, "u1")
			run := l.seedRun(t, principal, types.RunRunning)
			c := killRowOf(t, l, principal, run)
			at := time.Now().UTC().Truncate(time.Microsecond)
			pinnedID := uuid.New()
			set := `state = 'done', audit_event_payload = NULL, audit_event_id = NULL, audit_event_at = NULL`
			args := []any{c.key.ID}
			if tc.pinned {
				set = `state = 'done', audit_event_payload = NULL, audit_event_id = $2, audit_event_at = $3`
				args = append(args, pinnedID, at)
			}
			if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET `+set+` WHERE id = $1`, args...); err != nil {
				t.Fatal(err)
			}
			held, err := l.st.PinPortalEvent(ctx, c.id, store.JobKindSuspend, c.key, uuid.New(), time.Now().UTC(), []byte(`{"actor":"late-admin"}`), &c.bound)
			if err != nil || !held.Done || len(held.Payload) != 0 {
				t.Fatalf("a pin on a done step = %+v, %v; want Done and no payload", held, err)
			}
			if tc.pinned && (held.ID != pinnedID || !held.At.Equal(at)) {
				t.Errorf("a pin on a pinned finish names %s at %s, want the finish's %s at %s", held.ID, held.At, pinnedID, at)
			}
			if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE id = $1 AND audit_event_payload IS NOT NULL`, c.key.ID); n != 0 {
				t.Errorf("a finished step holds a payload again (%d rows)", n)
			}
			want := 0
			if tc.pinned {
				want = 1
			}
			if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE id = $1 AND audit_event_id IS NOT NULL`, c.key.ID); n != want {
				t.Errorf("rows naming an event after the late pin = %d, want %d", n, want)
			}
		})
	}
}

// The key a pinned payload is sealed under is the principal the audit sealer resolves the actor to, so person
// erasure, which destroys the keys of the principal and the names it proved, reaches it. An administrator who acted
// under their email in other letter case is the case: the real erasure selector runs, and the retry that follows
// attributes the replayed event to no one (the system, as "erased") and keeps the pinned id and time.
func TestPortalSuspend_PinOwnerIsErasedByTheErasureSelector_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	withSubjectKeys(t, l)
	const principal, email = "admin-erased", "Erased.Admin@Example.com"
	seedIdentity(t, l.st, principal, uuid.NewString(), strings.ToLower(email))
	person, _ := l.signIn(t, "u1")
	id := l.ident(t, person).ID
	first := portalOrigin(types.ActorHuman, email)
	sweep := portalSweepOrigin()
	l.seedRun(t, person, types.RunRunning)

	l.h.srv.cfg.Audit = &heldAudit{inner: store.Recorder{Pool: l.pool}, mode: "nil"}
	if err := l.h.srv.runPortalSuspension(withActor(ctx, first.actorType, first.actor), l.st, id, first, 0, nil); err == nil {
		t.Fatal("control: the first replica's suspension reported done with no stored receipt")
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND audit_event_payload::text ILIKE '%erased.admin%'`, id); n != 0 {
		t.Fatalf("pending payloads that hold the administrator's address = %d, want 0", n)
	}
	if _, err := l.h.srv.eraseAuditSealKeys(ctx, principal, nil); err != nil {
		t.Fatalf("erase the administrator's audit keys: %v", err)
	}
	l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
	if err := l.h.srv.auditSuspension(withActor(ctx, sweep.actorType, sweep.actor), sweep, l.st, id, portalSlot(sweep), nil); err != nil {
		t.Fatalf("the second replica's audit step: %v", err)
	}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		var n int
		var actor, kind string
		if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), ''), COALESCE(max(actor_type), '') FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&n, &actor, &kind); err != nil {
			t.Fatal(err)
		}
		if n != 1 || actor != pinnedErasedActor || kind != string(types.ActorSystem) {
			t.Errorf("%s rows = %d under %s %q, want 1 under system %q", action, n, kind, actor, pinnedErasedActor)
		}
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs j JOIN audit_events a ON a.id = j.audit_event_id AND a.time = j.audit_event_at
		WHERE j.identity_id = $1 AND j.step IN ($2, $3) AND j.state = 'done'`, id, jobStepAuditDeact, jobStepAuditDeprov); n != 2 {
		t.Errorf("done steps with the pinned id and time stored = %d, want 2", n)
	}
}

// An owed refusal row of a portal person settles only once its event is in the shared audit table, whichever way
// the first replica's recorder fell short (it spooled the row, or answered success with a spool only), and the
// retry from a recorder that writes the table adds the one row, under the id the first drain pinned.
func TestPortalSuspend_OwedRefusalSettlesFromAnotherRecorder_PG(t *testing.T) {
	for _, mode := range []string{"spool", "nil"} {
		t.Run(mode, func(t *testing.T) {
			l := newPortalLife(t)
			ctx := context.Background()
			person, _ := l.signIn(t, "u-owed")
			if w := l.post(t, l.admin, person, "suspend", ""); w.Code != 200 {
				t.Fatalf("suspend = %d", w.Code)
			}
			id := l.ident(t, person).ID
			step := store.JobStepRefusalAudit + "failure/test"
			if err := oweRefusalAudit(ctx, l.st, id, store.JobKindSuspend, store.JobKey{Step: step}); err != nil {
				t.Fatal(err)
			}
			sweep := portalSweepOrigin()
			sctx := withActor(ctx, sweep.actorType, sweep.actor)
			owed := store.PendingRefusalAudit{IdentityID: id, Kind: store.JobKindSuspend}
			rows := func() int {
				return l.count(t, `SELECT count(*) FROM audit_events WHERE action = 'person.deprovision' AND target = $1 AND outcome = 'failure' AND data->>'reason' = 'test'`, id.String())
			}
			l.h.srv.cfg.Audit = &heldAudit{inner: store.Recorder{Pool: l.pool}, mode: mode}
			if err := l.h.srv.drainPortalOwedRefusals(sctx, l.st, owed); err == nil {
				t.Fatal("the first drain reported the owed row settled with its event in no shared table")
			}
			if rows() != 0 || l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND step = $2 AND state = 'pending' AND audit_event_id IS NOT NULL`, id, step) != 1 {
				t.Fatalf("after the first drain: %d shared rows; want 0, with the owed row pending and its event pinned", rows())
			}
			l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
			if err := l.h.srv.drainPortalOwedRefusals(sctx, l.st, owed); err != nil {
				t.Fatalf("the second drain: %v", err)
			}
			if rows() != 1 || l.count(t, `SELECT count(*) FROM deprovision_jobs j JOIN audit_events a ON a.id = j.audit_event_id AND a.time = j.audit_event_at WHERE j.identity_id = $1 AND j.step = $2 AND j.state = 'done'`, id, step) != 1 {
				t.Errorf("after the second drain: %d shared rows, want 1 under the pinned id with the owed row done", rows())
			}
		})
	}
}

// A waiting run leaves its queue in the claim's own transaction, which holds the suspension's ledger row. While that
// claim is stalled on the run's row, a replacement suspension's step one, which deletes that ledger row, waits for
// the claim to commit rather than getting ahead of the departure; once the run's row is free the run is KILLED with no
// queue row, and the replacement then goes through. A departure made after the claim's transaction would let the
// replacement return while the claim is still stalled.
func TestPG_PortalSuspendConcurrentReplacementWaitsForTheQueueDeparture(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run, _ := enqueueWithDrive(t, l, principal)
	second := portalOrigin(types.ActorHuman, "second-admin")
	c := killRowOf(t, l, principal, run.ID)
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != 200 {
		t.Fatalf("reactivate = %d", w.Code)
	}
	// The run's row is held by someone else, so the claim's UPDATE of it waits after it has locked the ledger row.
	hold, err := l.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if err := hold.Commit(ctx); err != nil {
				t.Errorf("release the run's row: %v", err)
			}
		}
	}
	defer release()
	var holderPID int
	if err := hold.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, run.ID); err != nil {
		t.Fatal(err)
	}
	// Each party runs on a pool of its own that names its connections, and the observer is a third pool, so no wait is
	// recognised by the text of a statement (the queued claim's begins WITH, another primitive's may not) and the
	// held transaction's pool is never asked for a second connection.
	claimer, claimerName := l.namedPool(t, "claimant")
	replacer, replacerName := l.namedPool(t, "replacement")
	watch, _ := l.namedPool(t, "observer")
	claimDone := make(chan error, 1)
	go func() {
		_, err := store.NewPG(claimer).ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run.ID, From: types.RunQueued, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()})
		claimDone <- err
	}()
	// The claimant is waiting on the run's row: its connection is blocked by the holder's backend.
	pollTrue(t, "the claim to wait on the run's row", func() bool {
		var n int
		if err := watch.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = $1 AND wait_event_type = 'Lock' AND $2 = ANY(pg_blocking_pids(pid))`,
			claimerName, holderPID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	})
	// While it waits it holds the suspension's ledger row: a lock request on that row that does not wait is refused.
	probe, err := watch.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec(ctx, `SELECT 1 FROM deprovision_jobs WHERE id = $1 FOR UPDATE NOWAIT`, c.key.ID)
	_ = probe.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the stalled claim's ledger row is lockable by another transaction (%v), want it held", err)
	}
	replaced := make(chan error, 1)
	go func() {
		_, _, _, err := l.h.srv.portalSuspendStepOne(withActor(ctx, second.actorType, second.actor), store.NewPG(replacer), c.id, second, 0, nil)
		replaced <- err
	}()
	// The replacement's connection waits on the claimant's, which waits on the holder's: it queues behind the claim.
	pollTrue(t, "the replacement to wait on the stalled claim", func() bool {
		var n int
		if err := watch.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity r JOIN pg_stat_activity c ON c.pid = ANY(pg_blocking_pids(r.pid))
			WHERE r.datname = current_database() AND r.application_name = $1 AND r.wait_event_type = 'Lock'
			  AND c.application_name = $2 AND $3 = ANY(pg_blocking_pids(c.pid))`,
			replacerName, claimerName, holderPID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	})
	select {
	case err := <-replaced:
		t.Fatalf("the replacement suspension went ahead of the stalled claim (%v)", err)
	case <-time.After(1500 * time.Millisecond):
	}
	release()
	if err := <-claimDone; err != nil {
		t.Fatalf("the claim: %v", err)
	}
	if err := <-replaced; err != nil {
		t.Fatalf("the replacement's step one after the claim: %v", err)
	}
	if got := l.runState(t, run.ID); got != types.RunKilled || l.count(t, `SELECT count(*) FROM run_queue WHERE run_id = $1`, run.ID) != 0 {
		t.Errorf("run = %s with %d queue rows, want KILLED with none", got, l.count(t, `SELECT count(*) FROM run_queue WHERE run_id = $1`, run.ID))
	}
}

// namedPool opens a pool of its own onto the test database whose connections carry a unique application name, and
// returns it with that name. It is closed with the test.
func (l *portalLife) namedPool(t *testing.T, role string) (*pgxpool.Pool, string) {
	t.Helper()
	name := role + "-" + uuid.NewString()[:8]
	cfg := l.pool.Config()
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, name
}

// pollTrue polls cond for up to ten seconds.
func pollTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

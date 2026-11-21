// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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

// A worker that claimed a kill and died leaves its claim id on the row. While the lease runs, a claim-less write (a
// failure before any claim, the finish of a step that owes no kill) is refused; once it has run out such a write owns
// the row, so the spared finish closes it on the next pass instead of being refused on every one, and it closes with no
// receipt pinned, since no kill is counted for it.
func TestPortalSuspend_ClaimLessWriteOwnsARowWhoseLeaseRanOut_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	c := killRowOf(t, l, principal, run)
	got, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run, From: types.RunRunning, ReceiptID: uuid.New(), ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()})
	if err != nil || !got.Applied {
		t.Fatalf("the dead worker's claim = %+v, %v", got, err)
	}
	row := func() string {
		var out string
		if err := l.pool.QueryRow(ctx, `SELECT state || '|' || attempts || '|' || last_error || '|' || (audit_event_id IS NOT NULL)::text FROM deprovision_jobs WHERE id = $1`, c.key.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Inside the lease the claim holds: neither a claim-less failure nor a claim-less finish takes the row.
	before := row()
	if err := l.st.FailDeprovisionJob(ctx, c.id, store.JobKindSuspend, c.key, errors.New("before any claim")); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less failure inside the lease = %v, want ErrKillHeld", err)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, c.key, nil, c.bound); !errors.Is(err, store.ErrKillHeld) {
		t.Errorf("a claim-less finish inside the lease = %v, want ErrKillHeld", err)
	}
	if after := row(); after != before {
		t.Fatalf("a refused write changed the row: %q -> %q", before, after)
	}
	// The worker is dead: its lease has run out.
	if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, c.key.ID); err != nil {
		t.Fatal(err)
	}
	if err := l.st.FailDeprovisionJob(ctx, c.id, store.JobKindSuspend, c.key, errors.New("before any claim")); err != nil {
		t.Errorf("a claim-less failure after the lease = %v, want it recorded", err)
	}
	if got := row(); got != "pending|1|before any claim|true" {
		t.Errorf("row after the claim-less failure = %q, want pending, one attempt, the error, the pinned receipt kept", got)
	}
	if err := l.st.FinishSuspensionJobAt(ctx, c.id, c.key, map[string]int{"runs_killed": 0}, c.bound); err != nil {
		t.Errorf("the spared finish after the lease = %v, want nil", err)
	}
	if got := row(); got != "done|2||false" {
		t.Errorf("row after the spared finish = %q, want done, two attempts, no error and no receipt pinned", got)
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

// A run that left the queue is repaired as a queued kill: a worker that moved a waiting run out of its queue and
// stopped leaves the run KILLED with its queue row gone, and the repair, which sees an ordinary KILLED run, still
// writes the queued kill's receipt under the pinned id, with no sandbox to stop, and drops the drive the run kept
// its work folder in. The run and its drive are made as an enqueue makes them.
func TestPortalSuspend_QueuedKillIsRepairedAsQueued_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	run, drive := enqueueWithDrive(t, l, principal)
	c := killRowOf(t, l, principal, run.ID)
	live := func(q string) int { return l.count(t, q, run.ID, drive.ID) }
	if live(`SELECT count(*) FROM run_queue WHERE run_id = $1 AND $2::uuid IS NOT NULL`) != 1 || live(`SELECT count(*) FROM user_drives WHERE $1::uuid IS NOT NULL AND id = $2 AND delete_started_at IS NULL`) != 1 {
		t.Fatal("control: the queued run has no queue row or its drive is gone")
	}
	receipt := uuid.New()
	got, err := l.st.ClaimSuspensionKillAt(ctx, c.id, c.key, c.bound, store.RunKill{RunID: run.ID, From: types.RunQueued, ReceiptID: receipt, ReceiptAt: time.Now().UTC(), ClaimID: uuid.New()})
	if err != nil || !got.Applied {
		t.Fatalf("the worker's claim from the queue = %+v, %v", got, err)
	}
	if live(`SELECT count(*) FROM run_queue WHERE run_id = $1 AND $2::uuid IS NOT NULL`) != 0 || l.runState(t, run.ID) != types.RunKilled {
		t.Fatal("the claim did not take the run out of the queue in its own transaction")
	}
	if live(`SELECT count(*) FROM user_drives WHERE $1::uuid IS NOT NULL AND id = $2 AND delete_started_at IS NULL`) != 1 {
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
	if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(data->>'queued'), '') FROM audit_events WHERE action = 'run.kill' AND id = $1`, receipt).Scan(&n, &data); err != nil {
		t.Fatal(err)
	}
	if n != 1 || data != "true" {
		t.Errorf("run.kill rows under the pinned id = %d with queued=%q, want 1 with queued=true", n, data)
	}
	if got := live(`SELECT count(*) FROM user_drives WHERE $1::uuid IS NOT NULL AND id = $2 AND delete_started_at IS NULL`); got != 0 {
		t.Errorf("the run's kept drive is untouched after the repair (%d), want it dropped", got)
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

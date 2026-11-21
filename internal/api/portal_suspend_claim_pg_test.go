// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: the points a portal suspension's tail is exposed to a replacement or to another worker, the run's kill
// and the audit receipts.

// claimHook is the store whose run-kill claim runs before, once, before it claims.
type claimHook struct {
	store.PG
	once   sync.Once
	before func()
}

func (h *claimHook) ClaimSuspensionKillAt(ctx context.Context, identity uuid.UUID, k store.JobKey, at time.Time, kill store.RunKill) (store.KillClaim, error) {
	h.once.Do(h.before)
	return h.PG.ClaimSuspensionKillAt(ctx, identity, k, at, kill)
}

// A tail whose ledger check passed does not kill the run when a second suspension replaced the ledger between that
// check and the run's transition, whether the run is live or still waiting in its queue: the claim that makes the
// transition locks the row again, so the stale tail makes no transition, tears nothing down and writes no kill row.
// The replacement happens in the hook with its step one only (the tail's claim holds the run's start fence while the
// hook runs); its tail then runs, and the run's one kill is its own, under its own actor.
func TestPortalSuspend_ReplacementBetweenCheckAndTransitionKillsNothing_PG(t *testing.T) {
	for _, state := range []types.RunState{types.RunRunning, types.RunQueued} {
		t.Run(string(state), func(t *testing.T) {
			l := newPortalLife(t)
			ctx := context.Background()
			principal, _ := l.signIn(t, "u1")
			id := l.ident(t, principal).ID
			first := portalOrigin(types.ActorHuman, "first-admin")
			second := portalOrigin(types.ActorHuman, "second-admin")
			run := l.seedRun(t, principal, state)

			fctx := withActor(ctx, first.actorType, first.actor)
			sctx := withActor(ctx, second.actorType, second.actor)
			ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
			if err != nil {
				t.Fatalf("first step one: %v", err)
			}
			kills := l.run.killCount()
			hits := 0
			st := &claimHook{PG: l.st, before: func() {
				hits++
				// The ledger check passed. Now the person is reactivated and suspended again by someone else.
				if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
					t.Errorf("reactivate = %d", w.Code)
				}
				if _, _, _, err := l.h.srv.portalSuspendStepOne(sctx, l.st, id, second, 0, nil); err != nil {
					t.Errorf("second step one: %v", err)
				}
			}}
			err = l.h.srv.killStep(withPortalSuspensionBound(fctx, id, *ident.DeactivatedAt), first, st, id, forms.targets)
			if hits != 1 {
				t.Fatalf("the replacement ran %d times, want 1", hits)
			}
			if !errors.Is(err, store.ErrSuperseded) {
				t.Errorf("the first suspension's kill step = %v, want ErrSuperseded", err)
			}
			if got := l.runState(t, run); got != state {
				t.Errorf("the run after the stale tail = %s, want %s untouched", got, state)
			}
			if got := l.run.killCount() - kills; got != 0 || len(l.rows("run.kill")) != 0 {
				t.Errorf("the stale tail tore down %d times and wrote %d run.kill rows, want none", got, len(l.rows("run.kill")))
			}
			// The second suspension's own tail does the kill, once, under its own actor.
			if err := l.h.srv.runPortalSuspension(sctx, l.st, id, second, 0, nil); err != nil {
				t.Fatalf("second suspension: %v", err)
			}
			if got := l.runState(t, run); got != types.RunKilled {
				t.Errorf("the run after the second suspension = %s, want KILLED", got)
			}
			evs := l.rows("run.kill")
			if len(evs) != 1 || evs[0].Actor != "second-admin" {
				t.Errorf("run.kill rows = %d with actors %v, want 1 under second-admin", len(evs), actorsOf(evs))
			}
			if state == types.RunRunning {
				if got := l.run.killCount() - kills; got != 1 {
					t.Errorf("runner kills = %d, want 1", got)
				}
			}
		})
	}
}

// Two workers of one suspension never tear one run down twice. While a worker's claim of the run's kill is live, a
// second worker (the sweep) is refused (ErrKillHeld) before it calls the runner, rewrites the run or writes a receipt;
// the first worker's one run.kill row is its own and its step finishes. A worker that stopped after its claim leaves
// the same claim, and once its lease is over the repair tears down once, writes the occurrence's one pinned receipt
// under its own actor and does not write the run again.
func TestPortalSuspend_OverlappingAndCrashedKillClaims_PG(t *testing.T) {
	t.Run("overlap", func(t *testing.T) {
		l := newPortalLife(t)
		ctx := context.Background()
		principal, _ := l.signIn(t, "u1")
		id := l.ident(t, principal).ID
		first := portalOrigin(types.ActorHuman, "first-admin")
		sweep := portalSweepOrigin()
		run := l.seedRun(t, principal, types.RunRunning)
		fctx := withActor(ctx, first.actorType, first.actor)
		ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
		if err != nil {
			t.Fatalf("step one: %v", err)
		}
		bound := *ident.DeactivatedAt
		var sweepErr error
		var once sync.Once
		sweeps := 0
		l.h.srv.cfg.Runner = &killHook{scimRunner: l.run, on: func() {
			once.Do(func() {
				sweeps++
				kills := l.run.killCount()
				sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), id, bound)
				sweepErr = l.h.srv.killStep(sctx, sweep, l.st, id, forms.targets)
				if got := l.run.killCount(); got != kills {
					t.Errorf("the overlapping worker called the runner %d times, want 0", got-kills)
				}
			})
		}}
		if err := l.h.srv.killStep(withPortalSuspensionBound(fctx, id, bound), first, l.st, id, forms.targets); err != nil {
			t.Fatalf("first worker's kill step: %v", err)
		}
		if sweeps != 1 || !errors.Is(sweepErr, store.ErrKillHeld) {
			t.Errorf("overlapping worker = %v after %d runs, want ErrKillHeld once", sweepErr, sweeps)
		}
		if got := l.runState(t, run); got != types.RunKilled {
			t.Errorf("run = %s, want KILLED", got)
		}
		evs := l.rows("run.kill")
		if len(evs) != 1 || evs[0].Actor != "first-admin" {
			t.Errorf("run.kill rows = %d with actors %v, want 1 under first-admin", len(evs), actorsOf(evs))
		}
		if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND step = $2 AND state = 'done' AND audit_event_id IS NOT NULL`, id, jobStepKillRun); n != 1 {
			t.Errorf("finished kill rows holding the receipt id = %d, want 1", n)
		}
	})

	t.Run("crash", func(t *testing.T) {
		l := newPortalLife(t)
		ctx := context.Background()
		principal, _ := l.signIn(t, "u1")
		id := l.ident(t, principal).ID
		first := portalOrigin(types.ActorHuman, "first-admin")
		sweep := portalSweepOrigin()
		run := l.seedRun(t, principal, types.RunRunning)
		fctx := withActor(ctx, first.actorType, first.actor)
		ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
		if err != nil {
			t.Fatalf("step one: %v", err)
		}
		bound := *ident.DeactivatedAt
		// A worker claims the kill and stops: the run is KILLED, nothing is torn down, no receipt is written.
		key := store.JobKey{Step: jobStepKillRun, Target: run.String()}
		if err := l.st.EnsureSuspensionJobsAt(ctx, id, []store.JobKey{key}, bound); err != nil {
			t.Fatal(err)
		}
		jobs, err := l.st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
		if err != nil {
			t.Fatal(err)
		}
		j, _ := jobByKey(jobs, key.Step, key.Target)
		key.ID = j.ID
		receipt := uuid.New()
		c, err := l.st.ClaimSuspensionKillAt(ctx, id, key, bound, store.RunKill{RunID: run, From: types.RunRunning, ReceiptID: receipt, ReceiptAt: time.Now().UTC()})
		if err != nil || !c.Applied {
			t.Fatalf("the crashed worker's claim = %+v, %v", c, err)
		}
		var movedAt time.Time
		if err := l.pool.QueryRow(ctx, `SELECT updated_at FROM agent_runs WHERE id = $1`, run).Scan(&movedAt); err != nil {
			t.Fatal(err)
		}
		kills := l.run.killCount()
		sctx := withPortalSuspensionBound(withActor(ctx, sweep.actorType, sweep.actor), id, bound)
		if err := l.h.srv.killStep(sctx, sweep, l.st, id, forms.targets); !errors.Is(err, store.ErrKillHeld) || l.run.killCount() != kills {
			t.Fatalf("repair inside the lease = %v with %d runner calls, want ErrKillHeld and none", err, l.run.killCount()-kills)
		}
		if _, err := l.pool.Exec(ctx, `UPDATE deprovision_jobs SET kill_claimed_at = now() - interval '10 minutes' WHERE id = $1`, key.ID); err != nil {
			t.Fatal(err)
		}
		if err := l.h.srv.killStep(sctx, sweep, l.st, id, forms.targets); err != nil {
			t.Fatalf("repair after the lease: %v", err)
		}
		var after time.Time
		if err := l.pool.QueryRow(ctx, `SELECT updated_at FROM agent_runs WHERE id = $1`, run).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if !after.Equal(movedAt) {
			t.Errorf("the repair wrote the KILLED run again (updated_at %v -> %v)", movedAt, after)
		}
		if got := l.run.killCount() - kills; got != 1 {
			t.Errorf("runner kills by the repair = %d, want 1", got)
		}
		var n int
		var actor string
		if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), '') FROM audit_events WHERE action = 'run.kill' AND id = $1`, receipt).Scan(&n, &actor); err != nil {
			t.Fatal(err)
		}
		if n != 1 || actor != portalLifecycleActor {
			t.Errorf("run.kill rows with the pinned receipt id = %d under %q, want 1 under %s", n, actor, portalLifecycleActor)
		}
		if got := len(l.rows("run.kill")); got != 1 {
			t.Errorf("run.kill rows = %d, want 1", got)
		}
	})
}

// A tail that read the first suspension's ledger after the second suspension had replaced it, and pins audit events
// for the second suspension's rows under the first one's actor, is refused at the pin: the pin names the first
// suspension's deactivation, which no longer holds. It writes no row, and the second suspension's own rows carry its
// own actor.
func TestPortalSuspend_ReplacementBeforeListWritesNothing_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	first := portalOrigin(types.ActorHuman, "first-admin")
	second := portalOrigin(types.ActorHuman, "second-admin")
	l.seedRun(t, principal, types.RunRunning)

	fctx := withActor(ctx, first.actorType, first.actor)
	ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
	if err != nil {
		t.Fatalf("first step one: %v", err)
	}
	bound := withPortalSuspensionBound(fctx, id, *ident.DeactivatedAt)
	if err := l.h.srv.killStep(bound, first, l.st, id, forms.targets); err != nil {
		t.Fatalf("first kill step: %v", err)
	}
	// The second suspension replaces the ledger before the first suspension's audit step lists it.
	if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d", w.Code)
	}
	if _, _, _, err := l.h.srv.portalSuspendStepOne(withActor(ctx, second.actorType, second.actor), l.st, id, second, 0, nil); err != nil {
		t.Fatalf("second step one: %v", err)
	}
	err = l.h.srv.auditSuspension(bound, first, l.st, id, portalSlot(first), nil)
	if !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("the first suspension's audit step = %v, want ErrSuperseded", err)
	}
	rows := func(action string) int {
		return l.count(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, id.String())
	}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		if n := rows(action); n != 0 {
			t.Errorf("%s rows written by the stale tail = %d, want 0", action, n)
		}
	}
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND step LIKE 'audit%' AND audit_event_id IS NOT NULL`, id); n != 0 {
		t.Errorf("audit events pinned by the stale tail = %d, want 0", n)
	}
	if err := l.h.srv.runPortalSuspension(withActor(ctx, second.actorType, second.actor), l.st, id, second, 0, nil); err != nil {
		t.Fatalf("second suspension: %v", err)
	}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		var n int
		var actor string
		if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), '') FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&n, &actor); err != nil {
			t.Fatal(err)
		}
		if n != 1 || actor != "second-admin" {
			t.Errorf("%s rows = %d under actor %q, want 1 under second-admin", action, n, actor)
		}
	}
}

// heldAudit is the recorder of a replica that never writes the portal lifecycle's two completion rows to the shared
// table: in "spool" mode its store write failed and its spool took them (audit.ErrSpooled); in "nil" mode it answers
// success having appended them to a local spool only, as a sealing recorder does while a subject key is pending.
// Every other row is written.
type heldAudit struct {
	inner store.Recorder
	mode  string
	mu    sync.Mutex
	held  int
}

func (h *heldAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action != "identity.deactivate" && ev.Action != "person.deprovision" {
		return h.inner.Record(ctx, ev)
	}
	h.mu.Lock()
	h.held++
	h.mu.Unlock()
	if h.mode == "spool" {
		return audit.ErrSpooled
	}
	return nil
}

// withSubjectKeys gives the server a real audit-key service, as a full-sealing deployment has.
func withSubjectKeys(t *testing.T, l *portalLife) *subjectkey.Manager {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(l.pool, ident)
	if err != nil {
		t.Fatal(err)
	}
	keys := sec.SubjectKeys()
	l.h.srv.cfg.SubjectKeys = keys
	return keys
}

// A suspension's audit step settles in the shared ledger only once its row is in the shared audit table, whether the
// first replica's recorder says so (ErrSpooled) or answers success for a row only its spool holds. The step stays
// pending with the event's id, time and payload pinned, and the payload names the administrator only sealed under their
// audit key. Another replica that finds the step pending writes that same event, with the first replica's actor and
// data, once; the step is done and the payload is cleared in the same statement, keeping the id and time. When the
// administrator was erased meanwhile, the other replica keeps the pinned id and time and writes its own actor, and
// no row or ledger column names the erased administrator.
func TestPortalSuspend_UnstoredReceiptSettlesFromAnotherReplica_PG(t *testing.T) {
	for _, mode := range []string{"spool", "nil"} {
		for _, erased := range []bool{false, true} {
			name := mode
			if erased {
				name += "/erased"
			}
			t.Run(name, func(t *testing.T) {
				l := newPortalLife(t)
				ctx := context.Background()
				keys := withSubjectKeys(t, l)
				principal, _ := l.signIn(t, "u1")
				id := l.ident(t, principal).ID
				first := portalOrigin(types.ActorHuman, "first-admin")
				sweep := portalSweepOrigin()
				l.seedRun(t, principal, types.RunRunning)

				held := &heldAudit{inner: store.Recorder{Pool: l.pool}, mode: mode}
				l.h.srv.cfg.Audit = held
				if err := l.h.srv.runPortalSuspension(withActor(ctx, first.actorType, first.actor), l.st, id, first, 0, nil); err == nil {
					t.Fatal("the first replica's suspension reported done with its receipts in no shared table")
				}
				if held.held == 0 {
					t.Fatal("control: the first replica never tried to record a receipt")
				}
				actions := []string{"identity.deactivate", "person.deprovision"}
				for _, action := range actions {
					if n := l.count(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()); n != 0 {
						t.Errorf("%s rows in the shared table after the first replica = %d, want 0", action, n)
					}
				}
				audits := func(extra string) int {
					return l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step IN ($3, $4) `+extra, id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov)
				}
				if n := audits(`AND state = 'pending' AND audit_event_id IS NOT NULL AND audit_event_at IS NOT NULL AND audit_event_payload IS NOT NULL`); n != 2 {
					t.Errorf("pinned pending audit steps = %d, want 2", n)
				}
				if n := audits(`AND audit_event_payload::text LIKE '%first-admin%'`); n != 0 {
					t.Errorf("pending payloads that hold the administrator's name = %d, want 0 (sealed)", n)
				}
				if erased {
					if _, err := keys.Destroy(ctx, "first-admin", subjectkey.PurposeAuditSeal); err != nil {
						t.Fatalf("erase the administrator's audit key: %v", err)
					}
				}

				// Another replica, with the shared table, finds the steps pending and settles them.
				l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
				sctx := withActor(ctx, sweep.actorType, sweep.actor)
				if err := l.h.srv.auditSuspension(sctx, sweep, l.st, id, portalSlot(sweep), nil); err != nil {
					t.Fatalf("the second replica's audit step: %v", err)
				}
				want := "first-admin"
				if erased {
					want = portalLifecycleActor
				}
				for _, action := range actions {
					var n int
					var actor string
					if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), '') FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&n, &actor); err != nil {
						t.Fatal(err)
					}
					if n != 1 || actor != want {
						t.Errorf("%s rows = %d under actor %q, want 1 under %s", action, n, actor, want)
					}
				}
				if n := l.count(t, `SELECT count(*) FROM deprovision_jobs j JOIN audit_events a ON a.id = j.audit_event_id AND a.time = j.audit_event_at
					WHERE j.identity_id = $1 AND j.kind = $2 AND j.step IN ($3, $4) AND j.state = 'done' AND j.audit_event_payload IS NULL`, id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov); n != 2 {
					t.Errorf("done steps whose pinned id and time are the stored row's and whose payload is cleared = %d, want 2", n)
				}
				if erased {
					if n := l.count(t, `SELECT count(*) FROM audit_events WHERE target = $1 AND (actor = 'first-admin' OR data::text LIKE '%first-admin%')`, id.String()); n != 0 {
						t.Errorf("audit rows of the suspension that name the erased administrator = %d, want 0", n)
					}
				}
			})
		}
	}
}

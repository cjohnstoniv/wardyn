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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: the points a portal suspension's tail is exposed to a replacement, the run's kill and the audit
// receipts.

// claimHook is the store whose run-kill claim runs before, once, before it claims.
type claimHook struct {
	store.PG
	once   sync.Once
	before func()
}

func (h *claimHook) ClaimSuspensionKillAt(ctx context.Context, identity uuid.UUID, k store.JobKey, at time.Time, kill store.RunKill) (bool, store.KeptMachineDeletion, error) {
	h.once.Do(h.before)
	return h.PG.ClaimSuspensionKillAt(ctx, identity, k, at, kill)
}

// A tail whose ledger check passed does not kill the run when a second suspension replaced the ledger between that
// check and the run's transition: the claim that makes the transition locks the row again, so the stale tail makes no
// transition, tears nothing down and writes no kill row, and the run's one kill is the second suspension's.
func TestPortalSuspend_ReplacementBetweenCheckAndTransitionKillsNothing_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	first := portalOrigin(types.ActorHuman, "first-admin")
	run := l.seedRun(t, principal, types.RunRunning)

	fctx := withActor(ctx, first.actorType, first.actor)
	ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
	if err != nil {
		t.Fatalf("first step one: %v", err)
	}
	kills := l.run.killCount()
	hits := 0
	st := &claimHook{PG: l.st, before: func() {
		hits++
		// The ledger check passed. Now the person is reactivated and suspended again by someone else, whose tail kills
		// the run and finishes.
		if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d", w.Code)
		}
		if w := l.post(t, l.secAdmin, principal, "suspend", ""); w.Code != http.StatusOK {
			t.Errorf("second suspend = %d %s", w.Code, w.Body.String())
		}
	}}
	err = l.h.srv.killStep(withPortalSuspensionBound(fctx, id, *ident.DeactivatedAt), first, st, id, forms.targets)
	if hits != 1 {
		t.Fatalf("the replacement ran %d times, want 1", hits)
	}
	if !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("the first suspension's kill step = %v, want ErrSuperseded", err)
	}
	if got := l.runState(t, run); got != types.RunKilled {
		t.Errorf("the run = %s, want KILLED by the second suspension", got)
	}
	if got := l.run.killCount() - kills; got != 1 {
		t.Errorf("runner kills = %d, want 1 (the second suspension's)", got)
	}
	evs := l.rows("run.kill")
	if len(evs) != 1 || evs[0].Actor == "first-admin" {
		t.Errorf("run.kill rows = %d with actors %v, want 1 under the second suspension's actor", len(evs), actorsOf(evs))
	}
}

// A tail that read the first suspension's ledger after the second suspension had replaced it, and pins audit events
// for the second suspension's rows under the first one's actor, is refused at the pin: the pin names the first
// suspension's deactivation, which no longer holds. It writes no row, and the second suspension's own rows carry its
// own actor.
func TestPortalSuspend_ReplacementBeforeListWritesNothing_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
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
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND audit_event_id IS NOT NULL`, id); n != 0 {
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

// spoolOnly is the audit recorder of a replica whose store write fails and whose local spool takes every event: it
// writes nothing to the shared table.
type spoolOnly struct {
	mu sync.Mutex
	n  int
}

func (s *spoolOnly) Record(context.Context, types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return audit.ErrSpooled
}

// A suspension's audit step settles in the shared ledger only once its row is in the shared audit table. A replica
// whose store write failed and whose spool took the event leaves the step pending, with the event's id, time and
// payload pinned in the ledger; another replica that finds the step pending writes that same event, with the first
// replica's actor and data, once, and the step is done.
func TestPortalSuspend_SpooledReceiptSettlesFromAnotherReplica_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	first := portalOrigin(types.ActorHuman, "first-admin")
	sweep := portalSweepOrigin()
	l.seedRun(t, principal, types.RunRunning)

	spool := &spoolOnly{}
	l.h.srv.cfg.Audit = spool
	if err := l.h.srv.runPortalSuspension(withActor(ctx, first.actorType, first.actor), l.st, id, first, 0, nil); err == nil {
		t.Fatal("the first replica's suspension reported done with its receipts only in a spool")
	}
	if spool.n == 0 {
		t.Fatal("control: the first replica never tried to record a receipt")
	}
	actions := []string{"identity.deactivate", "person.deprovision"}
	for _, action := range actions {
		if n := l.count(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()); n != 0 {
			t.Errorf("%s rows in the shared table after the spool-only replica = %d, want 0", action, n)
		}
	}
	for _, step := range []string{jobStepAuditDeact, jobStepAuditDeprov} {
		if n := l.count(t, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step = $3 AND state = 'pending'
			AND audit_event_id IS NOT NULL AND audit_event_at IS NOT NULL AND audit_event_payload IS NOT NULL`, id, store.JobKindSuspend, step); n != 1 {
			t.Errorf("step %s pinned and pending rows = %d, want 1", step, n)
		}
	}

	// Another replica, with the shared table, finds the steps pending and settles them.
	l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
	sctx := withActor(ctx, sweep.actorType, sweep.actor)
	if err := l.h.srv.auditSuspension(sctx, sweep, l.st, id, portalSlot(sweep), nil); err != nil {
		t.Fatalf("the second replica's audit step: %v", err)
	}
	for _, action := range actions {
		var n int
		var actor string
		if err := l.pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(actor), '') FROM audit_events WHERE action = $1 AND target = $2`, action, id.String()).Scan(&n, &actor); err != nil {
			t.Fatal(err)
		}
		if n != 1 || actor != "first-admin" {
			t.Errorf("%s rows = %d under actor %q, want 1 under the first replica's actor", action, n, actor)
		}
	}
	// Each row is the pinned event: its id, its time and its data.
	if n := l.count(t, `SELECT count(*) FROM deprovision_jobs j JOIN audit_events a ON a.id = j.audit_event_id AND a.time = j.audit_event_at AND a.data = j.audit_event_payload->'data'
		WHERE j.identity_id = $1 AND j.kind = $2 AND j.step IN ($3, $4) AND j.state = 'done'`, id, store.JobKindSuspend, jobStepAuditDeact, jobStepAuditDeprov); n != 2 {
		t.Errorf("done steps whose pinned id, time and data are the stored row's = %d, want 2", n)
	}
}

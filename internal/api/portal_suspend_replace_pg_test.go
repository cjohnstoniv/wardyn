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

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hosted-only: a portal suspension's tail that read one suspension's ledger while another replaced it. The
// replacement lands through the store's own boundary, between the tail's read and its write.

// listHook is the store whose read of a person's live runs runs after, once, before it answers what it read.
type listHook struct {
	store.PG
	once  sync.Once
	after func()
}

func (h *listHook) ListNonTerminalRunsBy(ctx context.Context, by string) ([]types.AgentRun, error) {
	runs, err := h.PG.ListNonTerminalRunsBy(ctx, by)
	h.once.Do(h.after)
	return runs, err
}

// pinHook is the store whose audit-event pin runs before, once, before it pins.
type pinHook struct {
	store.PG
	once   sync.Once
	before func()
}

func (h *pinHook) PinPortalEvent(ctx context.Context, identity uuid.UUID, kind string, k store.JobKey, id uuid.UUID, at time.Time, payload []byte, bound *time.Time) (store.PinnedEvent, error) {
	h.once.Do(h.before)
	return h.PG.PinPortalEvent(ctx, identity, kind, k, id, at, payload, bound)
}

// A tail that counted a run's kill done in the first suspension's ledger, and saw the run live again, does not
// reopen the row of a second suspension that replaced the ledger between its read and its reopen, so the second
// suspension's finished kill is not torn down again by the stale tail, and the run keeps its two kill rows, one under
// each suspension's actor.
func TestPortalSuspend_ReplacementBetweenReadAndReopenIsLeftAlone_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := withActor(context.Background(), types.ActorSystem, portalLifecycleActor)
	principal, _ := l.signIn(t, "u1")
	run := l.seedRun(t, principal, types.RunRunning)
	if w := l.post(t, l.admin, principal, "suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("first suspend = %d %s", w.Code, w.Body.String())
	}
	id := l.ident(t, principal).ID
	first := *l.ident(t, principal).DeactivatedAt
	if st := l.runState(t, run); st != types.RunKilled {
		t.Fatalf("control: the first suspension left the run %s", st)
	}
	revive := func() {
		if _, err := l.pool.Exec(context.Background(), `UPDATE agent_runs SET state = $2, ended_at = NULL WHERE id = $1`, run, string(types.RunRunning)); err != nil {
			t.Fatalf("revive the run: %v", err)
		}
	}
	revive()
	var killRowID uuid.UUID
	if err := l.pool.QueryRow(context.Background(), `SELECT id FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4 AND state = 'done'`,
		id, store.JobKindSuspend, jobStepKillRun, run.String()).Scan(&killRowID); err != nil {
		t.Fatalf("control: the first suspension's done kill row: %v", err)
	}
	hits := 0
	st := &listHook{PG: l.st, after: func() {
		hits++
		// Between the tail's read of the first ledger and its reopen, the person is reactivated and suspended again by
		// someone else, whose tail kills the run again and finishes.
		if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d", w.Code)
		}
		revive()
		if w := l.post(t, l.secAdmin, principal, "suspend", ""); w.Code != http.StatusOK {
			t.Errorf("second suspend = %d %s", w.Code, w.Body.String())
		}
	}}
	o := portalOrigin(types.ActorSystem, portalLifecycleActor)
	err := l.h.srv.killStep(withPortalSuspensionBound(ctx, id, first), o, st, id, []string{principal})
	if hits != 1 {
		t.Fatalf("the replacement ran %d times, want 1", hits)
	}
	if !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("the first suspension's kill step = %v, want ErrSuperseded", err)
	}
	var kills []types.AuditEvent
	for _, ev := range l.rows("run.kill") {
		kills = append(kills, ev)
		if ev.Actor == portalLifecycleActor {
			t.Errorf("a run.kill row is the stale tail's (actor %q)", ev.Actor)
		}
	}
	if len(kills) != 2 || kills[0].Actor == kills[1].Actor {
		t.Errorf("run.kill rows = %d with actors %v, want 2 under two different actors", len(kills), actorsOf(kills))
	}
	var state string
	var rowID uuid.UUID
	if err := l.pool.QueryRow(context.Background(), `SELECT id, state FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4`,
		id, store.JobKindSuspend, jobStepKillRun, run.String()).Scan(&rowID, &state); err != nil {
		t.Fatalf("the second suspension's kill row: %v", err)
	}
	if state != "done" || rowID == killRowID {
		t.Errorf("kill row = state %s, same row as the first suspension's %v; want the second suspension's own row, done", state, rowID == killRowID)
	}
}

func actorsOf(evs []types.AuditEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Actor)
	}
	return out
}

// A worker that read the first suspension's ledger and counts, and is about to pin its audit events, does not pin
// the second suspension's rows when that suspension replaced the ledger between its read and its pin: it writes no
// row, and the second suspension's own rows carry its own actor and its own run count.
func TestPortalSuspend_ReplacementBetweenSnapshotAndPinWritesNothing_PG(t *testing.T) {
	l := newPortalLife(t)
	ctx := context.Background()
	l.h.srv.cfg.Audit = store.Recorder{Pool: l.pool}
	principal, _ := l.signIn(t, "u1")
	id := l.ident(t, principal).ID
	first := portalOrigin(types.ActorHuman, "first-admin")
	second := portalOrigin(types.ActorHuman, "second-admin")
	sweep := portalSweepOrigin()
	l.seedRun(t, principal, types.RunRunning)

	// The first suspension: step one and its kill step (one run), with its audit steps still to write.
	fctx := withActor(ctx, first.actorType, first.actor)
	ident, forms, _, err := l.h.srv.portalSuspendStepOne(fctx, l.st, id, first, 0, nil)
	if err != nil {
		t.Fatalf("first step one: %v", err)
	}
	if err := l.h.srv.killStep(withPortalSuspensionBound(fctx, id, *ident.DeactivatedAt), first, l.st, id, forms.targets); err != nil {
		t.Fatalf("first kill step: %v", err)
	}
	rows := func(action string) int {
		return l.count(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND target = $2`, action, id.String())
	}
	hits := 0
	st := &pinHook{PG: l.st, before: func() {
		hits++
		// The second suspension replaces the ledger, by someone else, over two live runs, and has not audited.
		if w := l.post(t, l.admin, principal, "reactivate", ""); w.Code != http.StatusOK {
			t.Errorf("reactivate = %d", w.Code)
		}
		l.seedRun(t, principal, types.RunRunning)
		l.seedRun(t, principal, types.RunRunning)
		sctx := withActor(ctx, second.actorType, second.actor)
		if _, _, _, err := l.h.srv.portalSuspendStepOne(sctx, l.st, id, second, 0, nil); err != nil {
			t.Errorf("second step one: %v", err)
		}
	}}
	// A sweep worker audits the first suspension from the ledger it read before the replacement.
	err = l.h.srv.auditSuspension(withActor(ctx, sweep.actorType, sweep.actor), sweep, st, id, portalSlot(sweep), nil)
	if hits != 1 {
		t.Fatalf("the replacement ran %d times, want 1", hits)
	}
	if !errors.Is(err, store.ErrSuperseded) {
		t.Errorf("the stale audit step = %v, want ErrSuperseded", err)
	}
	for _, action := range []string{"identity.deactivate", "person.deprovision"} {
		if n := rows(action); n != 0 {
			t.Errorf("%s rows written by the stale worker = %d, want 0", action, n)
		}
	}
	// The second suspension finishes: its rows carry its own actor and its own run count.
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
	var killed string
	if err := l.pool.QueryRow(ctx, `SELECT data->>'runs_killed' FROM audit_events WHERE action = 'person.deprovision' AND target = $1`, id.String()).Scan(&killed); err != nil || killed != "2" {
		t.Errorf("the second suspension's runs_killed = %q, %v; want 2 (the first suspension's one run is not counted in it)", killed, err)
	}
}

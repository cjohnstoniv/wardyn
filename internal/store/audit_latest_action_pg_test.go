// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for B8-F5: /healthz reports the eBPF sensor DEGRADED, with a stale
// last_heartbeat, while beats are arriving normally.
//
// LatestAuditEventByAction answered "the latest kernel.sensor.heartbeat" with
// ORDER BY seq DESC — insertion order, not event order. The audit spool replays
// at-least-once and replays KEEP their original ev.Time, so a beat that was
// spooled during a database blip comes back later with the HIGHEST seq and an
// hours-old time. The unauthenticated /healthz then reads that row's time and
// publishes a degraded sensor for a sensor that never stopped.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly without one.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_LatestAuditEventByActionAnswersByEventTimeNotInsertionOrder(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	// An action nobody else writes, so the window this reads is exactly the rows
	// seeded here — audit_events is append-only and shared by every test in the
	// lane's database.
	action := "test.heartbeat." + uuid.NewString()
	beat := func(at time.Time) uuid.UUID {
		t.Helper()
		ev := types.AuditEvent{
			ID: uuid.New(), Time: at, ActorType: types.ActorSystem,
			Actor: "b8-f5-probe", Action: action, Outcome: "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("insert beat at %s: %v", at, err)
		}
		return ev.ID
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	fresh := beat(now)
	// THE REPLAY. Inserted AFTER the fresh beat, so it holds the higher seq, and
	// carrying its ORIGINAL time, which is what makes a spool replay a replay.
	beat(now.Add(-2 * time.Hour))

	got, err := pg.LatestAuditEventByAction(ctx, action)
	if err != nil {
		t.Fatalf("LatestAuditEventByAction: %v", err)
	}
	if got.ID != fresh {
		t.Errorf("the latest %q is the row with the newest SEQ (%s, time %s) rather than the newest TIME (%s, time %s).\n"+
			"A spool replay keeps its original time and gets a fresh seq, so /healthz — which is unauthenticated — "+
			"publishes the eBPF sensor as degraded with an hours-old last_heartbeat while beats are arriving",
			action, got.ID, got.Time, fresh, now)
	}
	if !got.Time.Equal(now) {
		t.Errorf("time = %s, want the freshest beat's %s", got.Time, now)
	}

	// The ordinary case stays ordinary: a genuinely newer beat wins, and it is
	// the one with both the newest time AND the highest seq.
	newest := beat(now.Add(time.Second))
	got, err = pg.LatestAuditEventByAction(ctx, action)
	if err != nil {
		t.Fatalf("LatestAuditEventByAction after a newer beat: %v", err)
	}
	if got.ID != newest {
		t.Errorf("a beat newer in both time and seq was not returned; got %s, want %s", got.ID, newest)
	}

	// An action with no rows is still ErrNotFound, not an empty event.
	if _, err := pg.LatestAuditEventByAction(ctx, "test.heartbeat."+uuid.NewString()); err == nil {
		t.Error("an action with no rows answered nil error; /healthz tells 'never seen' apart from 'seen long ago' by exactly this")
	}
}

// TestPG_LatestAuditEventByActionFallsBackPastTheWindow pins the RESIDUAL as
// documented behaviour rather than leaving the comment claiming a bound the
// query does not hold.
//
// The window is twenty rows, so it absorbs a replay burst of up to twenty. A
// spool drain replays in batches and loops until the backlog clears, so an
// outage longer than a few heartbeat intervals produces more than that — and
// then the fresh beat is pushed out of the window and /healthz reads the stale
// one again, until the next live beat arrives and takes the top of the window
// back. That is B8-F5's accepted transience ("self-heals at the next beat"),
// bounded by one heartbeat interval, and the difference between "the finding is
// closed" and "the finding is bounded" is exactly what this asserts.
func TestPG_LatestAuditEventByActionFallsBackPastTheWindow(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	action := "test.heartbeat." + uuid.NewString()
	beat := func(at time.Time) uuid.UUID {
		t.Helper()
		ev := types.AuditEvent{
			ID: uuid.New(), Time: at, ActorType: types.ActorSystem,
			Actor: "b8-f5-window-probe", Action: action, Outcome: "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("insert beat at %s: %v", at, err)
		}
		return ev.ID
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	fresh := beat(now)
	// TWENTY stale replays still leave the fresh beat inside the window, so the
	// fix holds — this is the half the bound really covers.
	for i := range 19 {
		beat(now.Add(-time.Duration(i+1) * time.Hour))
	}
	got, err := pg.LatestAuditEventByAction(ctx, action)
	if err != nil {
		t.Fatalf("LatestAuditEventByAction with the fresh beat still in the window: %v", err)
	}
	if got.ID != fresh {
		t.Errorf("with 19 stale replays above it (20 rows total) the fresh beat was not returned; the window is "+
			"twenty rows and this is inside it (got %s, want %s)", got.ID, fresh)
	}

	// TWO MORE push it out, and the documented fallback is the old answer: the
	// newest row by time WITHIN the window, which is the youngest stale replay.
	// Asserted so the comment's claim and the query cannot drift apart.
	for i := range 2 {
		beat(now.Add(-time.Duration(20+i) * time.Hour))
	}
	got, err = pg.LatestAuditEventByAction(ctx, action)
	if err != nil {
		t.Fatalf("LatestAuditEventByAction past the window: %v", err)
	}
	if got.ID == fresh {
		t.Fatalf("21 stale replays did not push the fresh beat out of a twenty-row window — the window is not the " +
			"size the comment says it is")
	}
	if !got.Time.Before(now) {
		t.Errorf("past the window the answer is %s, which is not one of the stale replays; the fallback is the "+
			"newest row by time inside the window", got.Time)
	}

	// AND IT SELF-HEALS AT THE NEXT BEAT, which is the whole reason the residual
	// was accepted: one live beat takes the top of the window back.
	next := beat(now.Add(time.Second))
	got, err = pg.LatestAuditEventByAction(ctx, action)
	if err != nil {
		t.Fatalf("LatestAuditEventByAction after the next live beat: %v", err)
	}
	if got.ID != next {
		t.Errorf("the next live beat did not restore the answer (got %s, want %s) — the residual would not be "+
			"transient, and /healthz would stay degraded", got.ID, next)
	}
}

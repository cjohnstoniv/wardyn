// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_MaxAgeStopsAnOldRunThroughTheRealAdapter: against Postgres, a RUNNING run created past
// WARDYN_RUN_MAX_AGE is stopped and torn down even though it was touched a moment ago and its
// auto-stop is off; a young run and a kept run (lost_at set) beside it are left alone; and the
// stop writes a run.max_age.expire audit row. Guarded by WARDYN_TEST_PG.
func TestPG_MaxAgeStopsAnOldRunThroughTheRealAdapter(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	mk := func(createdAgo time.Duration) uuid.UUID {
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := pg.CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now.Add(-createdAgo), UpdatedAt: now,
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "max-age probe",
			ConfinementClass: types.CC2, State: types.RunRunning,
			SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
			SandboxRef: "wardyn-agent-" + id.String(),
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
		return id
	}
	old, young, kept := mk(3*time.Hour), mk(10*time.Minute), mk(3*time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'ended' WHERE id=$1`, kept); err != nil {
		t.Fatalf("mark the kept run: %v", err)
	}

	rn := &countingRunner{}
	rec := &fakeAuditRecorder{}
	own := ownRuns{Store: lifecycleStore{pool: pool}, ids: map[uuid.UUID]bool{old: true, young: true, kept: true}}
	reaper := lifecycle.New(own, lifecycleStopper{pool: pool, runner: rn}, rec,
		lifecycle.Config{MaxAge: time.Hour})
	reaper.Tick(ctx)

	for id, want := range map[uuid.UUID]types.RunState{old: types.RunStopped, young: types.RunRunning, kept: types.RunRunning} {
		if got, err := pg.GetRun(ctx, id); err != nil || got.State != want {
			t.Errorf("run %s state = %v (err %v), want %s", id, got.State, err, want)
		}
	}
	if got, _ := pg.GetRun(ctx, old); got.EndedAt == nil {
		t.Error("the max-age stop left ended_at unset")
	}
	if rn.stops != 1 {
		t.Errorf("teardowns = %d, want 1 (only the old run)", rn.stops)
	}
	if rec.calls != 1 || rec.last.Action != "run.max_age.expire" || rec.last.RunID == nil || *rec.last.RunID != old {
		t.Errorf("audit = %d rows, last %+v; want one run.max_age.expire for %s", rec.calls, rec.last, old)
	}
}

// ownRuns narrows the real adapter's scan to one test's runs: the shared test
// database holds other tests' RUNNING runs, which this tick must neither stop
// nor count.
type ownRuns struct {
	lifecycle.Store
	ids map[uuid.UUID]bool
}

func (o ownRuns) ListRunningWithPolicy(ctx context.Context) ([]lifecycle.RunSummary, time.Time, error) {
	all, now, err := o.Store.ListRunningWithPolicy(ctx)
	var mine []lifecycle.RunSummary
	for _, r := range all {
		if o.ids[r.ID] {
			mine = append(mine, r)
		}
	}
	return mine, now, err
}

// TestPG_MaxAgeStopsAnOutageKeptLiveAgent: a run kept after a control-plane outage before its
// end still runs its agent, so WARDYN_RUN_MAX_AGE ends it like a live run: STOPPED, torn down
// and audited run.max_age.expire. A run kept with its agent already stopped (after a reboot, or
// an outage past its end) is left to its files grace, by the scan and by the transition alike.
func TestPG_MaxAgeStopsAnOutageKeptLiveAgent(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	mk := func(reason types.LostReason, endsAt *time.Time) uuid.UUID {
		id := uuid.New()
		now := time.Now().UTC()
		if _, err := pg.CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "max-age kept probe",
			ConfinementClass: types.CC2, State: types.RunRunning, Interactive: true, EndsAt: endsAt,
			SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
			SandboxRef: "wardyn-agent-" + id.String(),
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id=$1`, id, string(reason)); err != nil {
			t.Fatalf("keep the run: %v", err)
		}
		return id
	}
	past := time.Now().Add(-time.Minute)
	live, pastEnd, rebooted := mk(types.LostOutage, nil), mk(types.LostOutage, &past), mk(types.LostReboot, nil)

	rn := &countingRunner{}
	rec := &fakeAuditRecorder{}
	stopper := lifecycleStopper{pool: pool, runner: rn}
	own := ownRuns{Store: lifecycleStore{pool: pool}, ids: map[uuid.UUID]bool{live: true, pastEnd: true, rebooted: true}}
	if err := lifecycle.New(own, stopper, rec, lifecycle.Config{MaxAge: time.Hour}).Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	for id, want := range map[uuid.UUID]types.RunState{live: types.RunStopped, pastEnd: types.RunRunning, rebooted: types.RunRunning} {
		if got, err := pg.GetRun(ctx, id); err != nil || got.State != want {
			t.Errorf("run %s state = %v (err %v), want %s", id, got.State, err, want)
		}
	}
	if rn.stops != 1 {
		t.Errorf("teardowns = %d, want 1 (only the outage-kept live agent)", rn.stops)
	}
	if rec.calls != 1 || rec.last.Action != "run.max_age.expire" || rec.last.RunID == nil || *rec.last.RunID != live {
		t.Errorf("audit = %d rows, last %+v; want one run.max_age.expire for %s", rec.calls, rec.last, live)
	}
	// A row listed before its agent was stopped still is not ended by the transition.
	for _, id := range []uuid.UUID{pastEnd, rebooted} {
		if out, err := stopper.StopRunMaxAge(ctx, id, time.Now()); err != nil || out.Applied {
			t.Errorf("StopRunMaxAge(%s) = %+v, %v; want not applied", id, out, err)
		}
	}
}

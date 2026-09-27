// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestPG_RunRevive pins migration 0084 through the revive surface: the sandbox
// ref records the release that started the proxy; a revive claims a run lost
// to an outage (clearing the mark and stamping a fresh token so the lapsed
// sweep leaves it alone), one lost to a reboot (refreshing its watcher lease,
// so the watcher sweep leaves its not-yet-started agent alone) or a live one,
// only as it read it, but never one lost to its end, nor a terminal one; the claim leaves
// the release alone until the new proxy runs; the listing carries every live
// run with a sandbox.
func TestPG_RunRevive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	outage, rebooted, live, finished := newRun(types.RunRunning), newRun(types.RunRunning), newRun(types.RunRunning), newRun(types.RunCompleted)
	ended := newRun(types.RunRunning)
	for _, r := range []types.AgentRun{outage, rebooted, live, finished, ended} {
		persistRun(t, ctx, pool, r)
		if err := pg.SetSandboxRef(ctx, r.ID, "wardyn-agent-"+r.ID.String()); err != nil {
			t.Fatalf("SetSandboxRef: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() - interval '2 hours', proxy_release = '' WHERE id=$1`, outage.ID); err != nil {
		t.Fatalf("age the outage run: %v", err)
	}
	if ok, err := pg.MarkRunLost(ctx, outage.ID, types.LostOutage, now, time.Hour); err != nil || !ok {
		t.Fatalf("MarkRunLost(outage) = %v, %v", ok, err)
	}
	if ok, err := pg.MarkRunLost(ctx, rebooted.ID, types.LostReboot, now, 0); err != nil || !ok {
		t.Fatalf("MarkRunLost(reboot) = %v, %v", ok, err)
	}
	if ok, err := pg.MarkRunLost(ctx, ended.ID, types.LostEnded, now, 0); err != nil || !ok {
		t.Fatalf("MarkRunLost(ended) = %v, %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET watcher_heartbeat = now() - interval '1 hour' WHERE id=$1`, rebooted.ID); err != nil {
		t.Fatalf("age the rebooted run's watcher lease: %v", err)
	}

	releases := func() map[string]string {
		t.Helper()
		rows, err := pg.ListRunProxyReleases(ctx)
		if err != nil {
			t.Fatalf("ListRunProxyReleases: %v", err)
		}
		m := map[string]string{}
		for _, r := range rows {
			m[r.RunID.String()] = r.Release
		}
		return m
	}
	got := releases()
	if got[live.ID.String()] != version.Version || got[outage.ID.String()] != "" {
		t.Errorf("releases = %v; want the live run on %s and the aged one unrecorded", got, version.Version)
	}
	if _, ok := got[finished.ID.String()]; ok {
		t.Error("a terminal run is listed")
	}

	for _, c := range []struct {
		name string
		run  types.AgentRun
		from types.LostReason
	}{
		{"rebooted as outage", rebooted, types.LostOutage}, {"rebooted as live", rebooted, ""},
		{"finished", finished, ""}, {"outage as live", outage, ""}, {"live as outage", live, types.LostOutage},
		{"ended", ended, types.LostEnded}, {"ended as live", ended, ""},
	} {
		if ok, err := pg.MarkRunRevived(ctx, c.run.ID, c.from, nil); err != nil || ok {
			t.Errorf("MarkRunRevived(%s) = %v, %v; want false", c.name, ok, err)
		}
	}
	if ok, err := pg.MarkRunRevived(ctx, rebooted.ID, types.LostReboot, nil); err != nil || !ok {
		t.Fatalf("MarkRunRevived(reboot) = %v, %v; want true", ok, err)
	}
	var leaseFresh bool
	if err := pool.QueryRow(ctx, `SELECT lost_at IS NULL AND watcher_heartbeat > now() - interval '1 minute' FROM agent_runs WHERE id=$1`,
		rebooted.ID).Scan(&leaseFresh); err != nil || !leaseFresh {
		t.Errorf("revived rebooted run: live with a fresh watcher lease = %v (%v); want true, or the watcher sweep loses it again before its agent starts", leaseFresh, err)
	}
	if ok, err := pg.MarkRunRevived(ctx, outage.ID, types.LostOutage, nil); err != nil || !ok {
		t.Fatalf("MarkRunRevived(outage) = %v, %v; want true", ok, err)
	}
	run, err := pg.GetRun(ctx, outage.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.LostAt != nil || run.LostReason != "" || run.State != types.RunRunning {
		t.Errorf("revived run = lost %v %q state %s; want live and RUNNING", run.LostAt, run.LostReason, run.State)
	}
	lapsed, err := pg.ListLapsedTokenRuns(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ListLapsedTokenRuns: %v", err)
	}
	if slices.ContainsFunc(lapsed, func(r types.AgentRun) bool { return r.ID == outage.ID }) {
		t.Error("a revived run is listed as lapsed: its token stamp was not refreshed")
	}
	if ok, err := pg.MarkRunRevived(ctx, live.ID, "", nil); err != nil || !ok {
		t.Errorf("MarkRunRevived(live) = %v, %v; want true — a restart", ok, err)
	}
	if got := releases(); got[outage.ID.String()] != "" || got[live.ID.String()] != version.Version {
		t.Errorf("releases after the claims = %v; want them unchanged until a new proxy runs", got)
	}
	for _, r := range []types.AgentRun{outage, live} {
		if err := pg.SetRunProxyRelease(ctx, r.ID, "9.9.9"); err != nil {
			t.Fatalf("SetRunProxyRelease: %v", err)
		}
	}
	if got := releases(); got[outage.ID.String()] != "9.9.9" || got[live.ID.String()] != "9.9.9" {
		t.Errorf("releases after revive = %v; want both on 9.9.9", got)
	}
}

// TestPG_EndedRunReviveNeedsFutureEnd pins #1061's revive claim: a run its own
// end stopped is claimed only under the ended condition, for the exact mark
// read, with its files grace live and its end after the claim's own Now, so
// an end or a grace that runs out after the revive's admission still refuses.
func TestPG_EndedRunReviveNeedsFutureEnd(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	r, endedAt := endedRun(t, ctx, pg, now)

	if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostEnded, keptAt(endedAt, now)); err != nil || ok {
		t.Fatalf("revive before the end moved = %v, %v; want false", ok, err)
	}
	later := now.Add(time.Hour)
	if ok, err := pg.SetRunEndAndWait(ctx, r.ID, r.EndsAt, r.WaitBudgetSec, &later, r.WaitBudgetSec, keptAt(endedAt, now)); err != nil || !ok {
		t.Fatalf("extend = %v, %v; want true", ok, err)
	}
	refused := func(name string, ended *store.EndedKept) {
		t.Helper()
		if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostEnded, ended); err != nil || ok {
			t.Errorf("%s: MarkRunRevived = %v, %v; want false", name, ok, err)
		}
	}
	refused("without the ended condition", nil)
	refused("another mark", keptAt(endedAt.Add(time.Second), now))
	refused("the new end has passed, the grace still live", keptAt(endedAt, later))
	// An end past the grace's own end, so only the grace refuses the next claim.
	far := endedAt.Add(2 * testEndedGrace)
	if ok, err := pg.SetRunEndAndWait(ctx, r.ID, &later, r.WaitBudgetSec, &far, r.WaitBudgetSec, keptAt(endedAt, now)); err != nil || !ok {
		t.Fatalf("extend again = %v, %v; want true", ok, err)
	}
	later = far
	refused("the grace has run out, the end still ahead", keptAt(endedAt, endedAt.Add(testEndedGrace)))
	if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostEnded, keptAt(endedAt, now)); err != nil || !ok {
		t.Fatalf("revive inside the grace before the new end = %v, %v; want true", ok, err)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.LostAt != nil || got.LostReason != "" || got.State != types.RunRunning || got.EndsAt == nil || !got.EndsAt.Equal(later) {
		t.Errorf("revived run = lost %v %q state %s ends %v; want live, RUNNING, ending at %v", got.LostAt, got.LostReason, got.State, got.EndsAt, later)
	}

	noEnd, noEndAt := endedRun(t, ctx, pg, now)
	if ok, err := pg.SetRunEndAndWait(ctx, noEnd.ID, noEnd.EndsAt, noEnd.WaitBudgetSec, nil, noEnd.WaitBudgetSec, keptAt(noEndAt, now)); err != nil || !ok {
		t.Fatalf("extend to No end = %v, %v; want true", ok, err)
	}
	if ok, err := pg.MarkRunRevived(ctx, noEnd.ID, types.LostEnded, keptAt(noEndAt, now)); err != nil || !ok {
		t.Errorf("revive with No end = %v, %v; want true", ok, err)
	}
}

// TestPG_EndedRunExtendReviveRacesTerminalTransition: the lease's expiry
// (StopKeptRunIf) and a kill against an ended run's extension and revive, in
// both orders; whichever lands first holds.
func TestPG_EndedRunExtendReviveRacesTerminalTransition(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	later := now.Add(time.Hour)
	state := func(id uuid.UUID) types.RunState {
		t.Helper()
		r, err := pg.GetRun(ctx, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r.State
	}

	for name, terminate := range map[string]func(r types.AgentRun, endedAt time.Time) (bool, error){
		"the grace expires": func(r types.AgentRun, endedAt time.Time) (bool, error) {
			return pg.StopKeptRunIf(ctx, r.ID, types.RunStopped, &endedAt, types.LostEnded, r.EndsAt)
		},
		"a kill": func(r types.AgentRun, _ time.Time) (bool, error) {
			return pg.UpdateRunStateIf(ctx, r.ID, types.RunRunning, types.RunKilled)
		},
	} {
		t.Run(name+" first", func(t *testing.T) {
			r, endedAt := endedRun(t, ctx, pg, now)
			if ok, err := terminate(r, endedAt); err != nil || !ok {
				t.Fatalf("terminate = %v, %v; want true", ok, err)
			}
			want := state(r.ID)
			if ok, err := pg.SetRunEndAndWait(ctx, r.ID, r.EndsAt, r.WaitBudgetSec, &later, r.WaitBudgetSec, keptAt(endedAt, now)); err != nil || ok {
				t.Errorf("extend after = %v, %v; want false", ok, err)
			}
			if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostEnded, keptAt(endedAt, now)); err != nil || ok {
				t.Errorf("revive after = %v, %v; want false", ok, err)
			}
			if got := state(r.ID); got != want || !got.IsTerminal() {
				t.Errorf("state = %s, want the terminal %s kept", got, want)
			}
		})
	}

	t.Run("the extension and revive first", func(t *testing.T) {
		r, endedAt := endedRun(t, ctx, pg, now)
		stale := r.EndsAt // what a sweep that listed the run before the extension holds
		if ok, err := pg.SetRunEndAndWait(ctx, r.ID, r.EndsAt, r.WaitBudgetSec, &later, r.WaitBudgetSec, keptAt(endedAt, now)); err != nil || !ok {
			t.Fatalf("extend = %v, %v; want true", ok, err)
		}
		if ok, err := pg.StopKeptRunIf(ctx, r.ID, types.RunStopped, &endedAt, types.LostEnded, stale); err != nil || ok {
			t.Errorf("a stale expiry after the extension = %v, %v; want false", ok, err)
		}
		if ok, err := pg.MarkRunRevived(ctx, r.ID, types.LostEnded, keptAt(endedAt, now)); err != nil || !ok {
			t.Fatalf("revive = %v, %v; want true", ok, err)
		}
		if ok, err := pg.StopKeptRunIf(ctx, r.ID, types.RunStopped, &endedAt, types.LostEnded, &later); err != nil || ok {
			t.Errorf("an expiry of the ended mark after the revive = %v, %v; want false", ok, err)
		}
		if got := state(r.ID); got != types.RunRunning {
			t.Errorf("state = %s, want RUNNING", got)
		}
	})
}

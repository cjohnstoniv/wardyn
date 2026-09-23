// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestPG_RunRevive pins migration 0072 through the revive surface: the sandbox
// ref records the release that started the proxy; a revive claims a run lost
// to an outage (clearing the mark and stamping a fresh token so the lapsed
// sweep leaves it alone) or a live one, only as it read it (live or lost), but
// never one lost to a reboot or its end, nor a terminal one; the claim leaves
// the release alone until the new proxy runs; the listing carries every live
// run with a sandbox.
func TestPG_RunRevive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	outage, rebooted, live, finished := newRun(types.RunRunning), newRun(types.RunRunning), newRun(types.RunRunning), newRun(types.RunCompleted)
	for _, r := range []types.AgentRun{outage, rebooted, live, finished} {
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
		run      types.AgentRun
		fromLost bool
	}{{rebooted, true}, {finished, false}, {outage, false}, {live, true}} {
		if ok, err := pg.MarkRunRevived(ctx, c.run.ID, c.fromLost); err != nil || ok {
			t.Errorf("MarkRunRevived(%s, fromLost %v) = %v, %v; want false", c.run.State, c.fromLost, ok, err)
		}
	}
	if ok, err := pg.MarkRunRevived(ctx, outage.ID, true); err != nil || !ok {
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
	if ok, err := pg.MarkRunRevived(ctx, live.ID, false); err != nil || !ok {
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

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A pool's Interactive idle stop reaches the run row the idle reaper reads. Against a real Postgres:
// guarded by WARDYN_TEST_PG, skipped cleanly when unset, must PASS when set.

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_PoolIdleStopBoundsAnInteractiveRun(t *testing.T) {
	const interactive = `"title":"t","experience":"interactive","tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`
	for _, tc := range []struct {
		name         string
		policy, want int // the deployment policy's auto_stop_after_sec, and the run's
	}{
		{"a policy with no idle stop gets the pool's default", 0, 300},
		{"the pool's cap cuts the policy's idle stop", 3600, 600},
		{"a policy value within the cap stands", 120, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			e.srv.cfg.DefaultPolicy.AutoStopAfterSec = tc.policy
			p := e.createPool("Idle", types.RunnerPoolOrganization)
			e.addExecutors(p.ID, "alpha", "beta")
			limits := types.RunnerPoolLimits{
				Interactive: &types.RunnerPoolInteractiveLimits{Idle: &types.RunnerPoolDuration{DefaultSec: 300, MaxSec: 600}},
				Barriers:    []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
			}
			if _, err := e.pg.UpdateRunnerPool(context.Background(), p.ID, e.revision(p.ID), nil, nil, &limits); err != nil {
				t.Fatalf("set pool limits: %v", err)
			}
			created := e.create(`{` + interactive + `,"runner_pool_id":"` + p.ID.String() + `"}`)
			stored, err := e.pg.GetRun(context.Background(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if created.AutoStopAfterSec != tc.want || stored.AutoStopAfterSec != tc.want {
				t.Errorf("idle stop = created %d stored %d, want %d", created.AutoStopAfterSec, stored.AutoStopAfterSec, tc.want)
			}
		})
	}
}

func TestPoolIdleStop(t *testing.T) {
	bounded := types.EffectiveLimit{Default: types.LimitAmount{Value: 300}, Cap: types.LimitAmount{Value: 600}}
	unlimited := types.EffectiveLimit{Default: types.LimitAmount{Unlimited: true}, Cap: types.LimitAmount{Unlimited: true}}
	for _, tc := range []struct {
		name      string
		sec, want int
		idle      types.EffectiveLimit
	}{
		{"no policy value takes the pool's default", 0, 300, bounded},
		{"an explicit never takes the pool's default", -1, 300, bounded},
		{"a value under the cap stands", 120, 120, bounded},
		{"a value over the cap is cut", 3600, 600, bounded},
		{"an unlimited pool changes nothing", 3600, 3600, unlimited},
		{"an unlimited pool leaves never alone", -1, -1, unlimited},
	} {
		if got := poolIdleStop(tc.sec, tc.idle); got != tc.want {
			t.Errorf("%s: poolIdleStop(%d) = %d, want %d", tc.name, tc.sec, got, tc.want)
		}
	}
}

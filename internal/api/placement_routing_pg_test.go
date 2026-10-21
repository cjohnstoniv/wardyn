// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The routing invariant at the run-create API against a real Postgres (#110): a placement the person asked for
// is honoured or refused before anything is created, and never answered by another placement. Each refusal row
// leaves no run row, no run.create audit row and no create on any runner, at create, Review and the preview.
// The reasons and sentences are pinned where they are decided and are not re-asserted here:
// TestPG_OwnOfflinePinAnswersRunnerOffline (runner_offline), TestDenyPlacements_RefusedAtEveryDoor
// (placement_denied), TestPG_HeterogeneousRemotePoolNeverWidens (placement_capability on a remote pool).
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// creates is every create the deployment's runner was asked for, on any executor.
func (p *poolRunner) creates() int {
	p.fakeRunner.mu.Lock()
	defer p.fakeRunner.mu.Unlock()
	return p.fakeRunner.createCalls + len(p.onExecutor)
}

func (e *placementEnv) auditCount(match func(types.AuditEvent) bool) int {
	n := 0
	for _, ev := range e.h.audit.snapshot() {
		if match(ev) {
			n++
		}
	}
	return n
}

func TestPG_PlacementRouting_RefusedBeforeCreateNeverSubstituted(t *testing.T) {
	const walled = "walled"
	for _, tc := range []struct {
		name string
		// setup returns the request body; the deployment always has a remote pool that could answer instead.
		setup func(e *placementEnv, remotePool string) string
		// status and reason are create's answer; a refusal is the same at Review and the preview.
		status int
		reason string
		// names is what the refusal must name: the profile or the pool the person chose.
		names   string
		deniedP bool // the refusal is the deny_placements door and writes authz.denied
		// wantPlacement is the placement of the one row a permitted create inserts.
		wantPlacement types.Placement
	}{
		{
			name: "1 local placement on an offline runner is runner_offline",
			setup: func(e *placementEnv, _ string) string {
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				asleep := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
				e.addToPool(e.person, laptops.ID, asleep)
				e.online[asleep] = false
				return runBody(`"placement":"local","runner_id":"` + asleep.String() + `","runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusUnprocessableEntity, reason: string(placement.ReasonRunnerOffline), names: "Laptops",
		},
		{
			name: "2a a profile that denies local refuses placement local",
			setup: func(e *placementEnv, _ string) string {
				e.assignProfile(walled, types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}})
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
				e.addToPool(e.person, laptops.ID, awake)
				return runBody(`"placement":"local","runner_id":"` + awake.String() + `","runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusForbidden, reason: string(placement.ReasonPlacementDenied), names: walled, deniedP: true,
		},
		{
			name: "2b a profile that permits remote runs an explicit placement remote as asked",
			setup: func(e *placementEnv, remotePool string) string {
				e.assignProfile(walled, types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementLocal}})
				return runBody(`"placement":"remote","runner_pool_id":"` + remotePool + `"`)
			},
			status: http.StatusCreated, wantPlacement: types.PlacementRemote,
		},
		{
			// The size no longer refuses (it is reduced to the runner's capacity); until local runs are
			// enabled the local gate answers, and the run is never moved to the remote pool.
			name: "3 a size the asked runner cannot hold is reduced on that runner, not refused nor run on the remote pool that can",
			setup: func(e *placementEnv, _ string) string {
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				small := e.laptop(e.personP, 1000, 1024, types.CC1, types.CC2, types.CC3)
				e.addToPool(e.person, laptops.ID, small)
				return runBody(`"placement":"local","resources":{"cpu_millis":4000,"memory_mib":8192},"runner_id":"` + small.String() + `","runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusCreated, wantPlacement: types.PlacementLocal,
		},
		{
			name: "4 a profile that pauses idle runs is placement_capability on a runner that cannot pause, not a remote run",
			setup: func(e *placementEnv, _ string) string {
				e.assignProfile(walled, types.GovernanceLimits{RunLimits: types.RunLimits{PauseIdleAfterSec: 300}})
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
				e.stream(awake, types.CC1, types.CC2, types.CC3)
				e.addToPool(e.person, laptops.ID, awake)
				// The pool, not the request, decides local: the demand is held against the runner it would place on.
				return runBody(`"runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusUnprocessableEntity, reason: string(placement.ReasonPlacementCapability), names: "pause_idle_after_sec",
		},
		{
			name: "5 a barrier the runner lacks is placement_capability, not a run on the remote pool that has it",
			setup: func(e *placementEnv, _ string) string {
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				weak := e.laptop(e.personP, 4000, 8192, types.CC1)
				e.addToPool(e.person, laptops.ID, weak)
				return runBody(`"placement":"local","confinement_class":"CC3","runner_id":"` + weak.String() + `","runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusUnprocessableEntity, reason: string(placement.ReasonPlacementCapability), names: "Laptops",
		},
		{
			name: "6 a local run on the person's own online runner is created there",
			setup: func(e *placementEnv, _ string) string {
				laptops := e.createPool("Laptops", types.RunnerPoolUser)
				awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
				e.addToPool(e.person, laptops.ID, awake)
				return runBody(`"placement":"local","runner_id":"` + awake.String() + `","runner_pool_id":"` + laptops.ID.String() + `"`)
			},
			status: http.StatusCreated, wantPlacement: types.PlacementLocal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, remote.ID))
			body := tc.setup(e, remote.ID.String())

			stored := func() []types.AgentRun {
				rows, err := e.pg.ListRuns(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			runs := func() int { return len(stored()) }
			isRunCreate := func(ev types.AuditEvent) bool { return ev.Action == "run.create" && ev.Outcome == "success" }
			isDenied := func(ev types.AuditEvent) bool { return ev.Action == authz.AuditAction && ev.Target == "runs.placement" }
			if runs() != 0 || e.runner.creates() != 0 {
				t.Fatalf("fixture is not empty: %d runs, %d creates", runs(), e.runner.creates())
			}

			doors := e.answers(e.person, body)
			if refused := tc.status >= http.StatusBadRequest; refused {
				first := sameEverywhere(t, tc.name, doors, tc.status, tc.reason)
				if !strings.Contains(first, tc.names) {
					t.Errorf("the refusal %s does not name %q", first, tc.names)
				}
				if n := runs(); n != 0 {
					t.Errorf("a refused request left %d run row(s)", n)
				}
				if n := e.runner.creates(); n != 0 {
					t.Errorf("a refused request reached a runner %d time(s)", n)
				}
				if n := e.auditCount(isRunCreate); n != 0 {
					t.Errorf("a refused request wrote %d run.create row(s)", n)
				}
				if got, want := e.auditCount(isDenied), map[bool]int{true: len(placementDoors), false: 0}[tc.deniedP]; got != want {
					t.Errorf("authz.denied{runs.placement} rows = %d, want %d", got, want)
				}
				return
			}

			if w := doors[placementDoors[0]]; w.Code != tc.status {
				t.Fatalf("create = %d %s, want %d", w.Code, w.Body, tc.status)
			}
			for _, door := range placementDoors[1:] {
				if w := doors[door]; w.Code >= http.StatusBadRequest {
					t.Errorf("%s = %d %s, want it to agree with create", door, w.Code, w.Body)
				}
			}
			if n := runs(); n != 1 {
				t.Fatalf("a permitted create left %d run row(s), want exactly one (Review and the preview insert none)", n)
			}
			if row := stored()[0]; row.Placement != tc.wantPlacement || row.PlacementFilled {
				t.Errorf("row placement = %q filled = %v, want %q asked (filled false)", row.Placement, row.PlacementFilled, tc.wantPlacement)
			}
			if n := e.auditCount(isRunCreate); n != 1 {
				t.Errorf("a permitted create wrote %d run.create row(s), want 1", n)
			}
			if n := e.auditCount(isDenied); n != 0 {
				t.Errorf("a permitted create wrote %d authz.denied row(s)", n)
			}
		})
	}
}

// assignProfile gives the person a governance profile with the limits and no other constraint.
func (e *placementEnv) assignProfile(name string, limits types.GovernanceLimits) {
	e.t.Helper()
	p, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{Name: name, CreatedBy: "seed", Limits: limits})
	if err != nil {
		e.t.Fatalf("seed profile %q: %v", name, err)
	}
	e.seedAssignment(p, e.personP)
}

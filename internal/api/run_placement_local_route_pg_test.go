// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Self-hosted pools end to end at the run-create API against a real Postgres: a run the pool places on the
// person's own runner is created, bound and dispatched to that runner, and deny_placements holds the placement
// the pool decided, not the request's. Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stream connects the runner's own endpoint to a served runner over loopback, as the hub does on
// admission, reporting classes: dispatch can lease the run to it and a reader of its own capabilities
// (the pause demand) has an answer. The sandbox itself is still the deployment's fake runner.
func (e *placementEnv) stream(id uuid.UUID, classes ...types.ConfinementClass) {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)
	fake := runnertest.NewFake()
	fake.Support.Classes = classes
	ep := e.srv.runnerEndpoint(id)
	org, run := runnerwire.Loopback()
	ep.link.Attach(ctx, org)
	served := runnertest.Serve(ctx, run, id.String(), fake)
	if err := served.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, []string{"/srv/dev/work"}, "test"); err != nil {
		e.t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, ok := ep.sub.Caps(); ok {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("the runner's caps never arrived")
		}
	}
}

// localCreate is the one run row a create inserted, after create answered 201.
func (e *placementEnv) localCreate(body string) types.AgentRun {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		e.t.Fatal(err)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return run
}

// LE-1: a run the pool places on the person's own runner is bound to that runner at create and dispatched to
// it, never to the deployment's own executors. Its barrier is held to that runner's classes, not to the
// deployment's, which here enforce CC1 alone.
func TestPG_LocalRun_DispatchedToItsBoundRunner(t *testing.T) {
	e := newPlacementEnv(t)
	e.runner.fakeRunner.capsClasses = []types.ConfinementClass{types.CC1}
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2, types.CC3)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

	run := e.localCreate(runBody(`"confinement_class":"CC3"`))
	if run.Placement != types.PlacementLocal || !run.PlacementFilled || run.RunnerID == nil || *run.RunnerID != awake ||
		run.EvidenceSource != types.RunEvidenceRunnerAsserted || run.ConfinementClass != types.CC3 {
		t.Fatalf("row = placement %q filled %v runner %v evidence %q class %s, want local filled on %s runner_asserted CC3",
			run.Placement, run.PlacementFilled, run.RunnerID, run.EvidenceSource, run.ConfinementClass, awake)
	}
	spec := e.dispatchedSpec(run.ID).RunnerID
	if spec != awake.String() {
		t.Fatalf("dispatch created for runner %q, the run is bound to %s; audit: %s", spec, awake, auditDump(e.h.audit.snapshot(), run.ID))
	}
	// Dispatch leased the run to its runner under a generation and bound the agent's Exec to it.
	var cur store.RunnerExecution
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && cur.ExecID == ""; time.Sleep(20 * time.Millisecond) {
		cur, _ = e.pg.RunnerExecutionOf(context.Background(), run.ID)
	}
	if cur.RunnerID != awake || cur.Operation != runnerwire.MethodExec || cur.Generation == uuid.Nil || cur.ExecID == "" {
		t.Errorf("execution = %+v, want an Exec generation on %s bound to the agent's exec", cur, awake)
	}
	if n := len(e.runner.executorCalls()); n != 0 {
		t.Errorf("a self-hosted run reached the deployment's executors %d time(s)", n)
	}
	for _, ev := range e.h.audit.snapshot() {
		if ev.RunID != nil && *ev.RunID == run.ID && ev.Action == "run.dispatch" && ev.Outcome == "failure" {
			t.Errorf("dispatch refused the bound run: %s", ev.Data)
		}
	}
}

// #2972 C1: a run on the person's own runner is created (201) and dispatched with WARDYN_DESKTOP_SIGNINS off,
// and on. Both sign-in fields are classified for local placement, and neither is set on such a run
// (signinTarget refuses a run bound to a runner).
func TestPG_LocalRun_CreatedWhateverTheDesktopSigninsKnob(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(fmt.Sprintf("desktop_signins=%v", on), func(t *testing.T) {
			e := newPlacementEnv(t)
			e.srv.cfg.DesktopSignins = on
			e.runner.fakeRunner.capsClasses = []types.ConfinementClass{types.CC1}
			laptops := e.createPool("Laptops", types.RunnerPoolUser)
			awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
			e.addToPool(e.person, laptops.ID, awake)
			e.stream(awake, types.CC1, types.CC2, types.CC3)
			e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

			run := e.localCreate(runBody(`"confinement_class":"CC3"`))
			if run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != awake {
				t.Fatalf("row = placement %q runner %v, want local on %s", run.Placement, run.RunnerID, awake)
			}
			if spec := e.dispatchedSpec(run.ID); spec.DesktopSignins || spec.ProxyConfig.DesktopSignins {
				t.Fatalf("a run on a person's runner was dispatched with sign-ins: store %v listener %v", spec.DesktopSignins, spec.ProxyConfig.DesktopSignins)
			}
		})
	}
}

// LE-3 and LE-10 (R2-0132): deny_placements holds the placement the pool decides. A member whose profile
// denies remote launches on a self-hosted pool, however the pool was chosen; a remote pool, named or
// defaulted, is the same 403 and authz.denied row as an explicit ask at every door.
func TestPG_DenyRemote_LaunchesOnASelfHostedPool(t *testing.T) {
	const walled = "no-remote"
	for _, tc := range []struct {
		name   string
		setup  func(e *placementEnv, laptops, remotePool types.RunnerPool) string
		denied bool
		filled bool
	}{
		{"the person's default is the self-hosted pool", func(e *placementEnv, laptops, _ types.RunnerPool) string {
			e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))
			return runBody("")
		}, false, true},
		{"the request names the self-hosted pool", func(_ *placementEnv, laptops, _ types.RunnerPool) string {
			return runBody(`"runner_pool_id":"` + laptops.ID.String() + `"`)
		}, false, true},
		{"the request names a remote pool", func(_ *placementEnv, _, remotePool types.RunnerPool) string {
			return runBody(`"runner_pool_id":"` + remotePool.ID.String() + `"`)
		}, true, false},
		{"the organisation's default is a remote pool", func(_ *placementEnv, _, _ types.RunnerPool) string {
			return runBody("")
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			remotePool := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remotePool.ID, "alpha", "beta")
			e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, remotePool.ID))
			laptops := e.createPool("Laptops", types.RunnerPoolUser)
			awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
			e.addToPool(e.person, laptops.ID, awake)
			e.assignProfile(walled, types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementRemote}})
			body := tc.setup(e, laptops, remotePool)

			isDenied := func(ev types.AuditEvent) bool { return ev.Action == authz.AuditAction && ev.Target == "runs.placement" }
			doors := e.answers(e.person, body)
			if tc.denied {
				first := sameEverywhere(t, tc.name, doors, http.StatusForbidden, string(placement.ReasonPlacementDenied))
				if !strings.Contains(first, walled) {
					t.Errorf("the refusal %s does not name the profile %q", first, walled)
				}
				if got := e.auditCount(isDenied); got != len(placementDoors) {
					t.Errorf("authz.denied{runs.placement} rows = %d, want one per door (%d)", got, len(placementDoors))
				}
				if rows, err := e.pg.ListRuns(context.Background()); err != nil || len(rows) != 0 {
					t.Errorf("a denied placement left %d run row(s) (%v)", len(rows), err)
				}
				return
			}
			for _, door := range placementDoors {
				if w := doors[door]; w.Code >= http.StatusBadRequest {
					t.Fatalf("%s = %d %s, want the self-hosted pool to admit it", door, w.Code, w.Body)
				}
			}
			rows, err := e.pg.ListRuns(context.Background())
			if err != nil || len(rows) != 1 {
				t.Fatalf("runs = %d (%v), want the one create", len(rows), err)
			}
			if r := rows[0]; r.Placement != types.PlacementLocal || r.PlacementFilled != tc.filled || r.RunnerID == nil || *r.RunnerID != awake {
				t.Errorf("row = placement %q filled %v runner %v, want local on %s", r.Placement, r.PlacementFilled, r.RunnerID, awake)
			}
			if got := e.auditCount(isDenied); got != 0 {
				t.Errorf("an admitted run wrote %d authz.denied row(s)", got)
			}
		})
	}
}

// dispatchedSpec waits for dispatch's one create on the deployment's runner and returns its spec.
func (e *placementEnv) dispatchedSpec(runID uuid.UUID) runner.SandboxSpec {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		e.runner.fakeRunner.mu.Lock()
		calls, spec := e.runner.fakeRunner.createCalls, e.runner.fakeRunner.lastSpec
		e.runner.fakeRunner.mu.Unlock()
		if calls > 0 {
			return spec
		}
	}
	e.t.Fatalf("dispatch never created the sandbox; audit: %s", auditDump(e.h.audit.snapshot(), runID))
	return runner.SandboxSpec{}
}

// LE-7: a repository attaches to a run on the person's own runner through the organisation's git broker:
// the github_token grant is delivered via_org, so the runner's proxy holds the repository allowlist and a
// via_org route, never an installation token.
func TestPG_LocalRun_RepositoryGoesThroughTheBroker(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2, types.CC3)
	e.srv.cfg.DefaultPolicy.EligibleGrants = append(e.srv.cfg.DefaultPolicy.EligibleGrants, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"]}`)})
	if _, err := e.pg.PutCredentialDeliveryPolicy(context.Background(), json.RawMessage(`{"classes":{"github_token":{"mode":"via_org"}}}`), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := runBody(`"runner_pool_id":"` + laptops.ID.String() + `","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["github.com"],` +
		`"eligible_grants":[{"kind":"github_token","scope":{"repos":["acme/widgets"]}}]}`)
	run := e.localCreate(body)
	if run.RunnerID == nil || *run.RunnerID != awake {
		t.Fatalf("run bound to %v, want %s", run.RunnerID, awake)
	}
	spec := e.dispatchedSpec(run.ID)
	if spec.RunnerID != awake.String() {
		t.Fatalf("dispatched to runner %q, want %s", spec.RunnerID, awake)
	}
	grant, ok := spec.ProxyConfig.GitGrants["acme/widgets"]
	if !ok {
		t.Fatalf("the runner's proxy has no git grant for the repository: %+v", spec.ProxyConfig.GitGrants)
	}
	if i := slices.IndexFunc(spec.ProxyConfig.ViaOrgRoutes, func(r proxy.ViaOrgRoute) bool { return r.GrantID == grant }); i < 0 {
		t.Errorf("the repository's grant %s has no via_org route: %+v", grant, spec.ProxyConfig.ViaOrgRoutes)
	}
	rows := mustListGrants(t, e.pg, run.ID)
	if len(rows) != 1 || rows[0].Delivery != types.GrantDeliveryViaOrg {
		t.Errorf("stored grants = %+v, want the one github_token grant delivered via_org", rows)
	}
}

// LE-6: on the person's own runner, a Background run that asks for more CPU and memory than the runner
// holds is created, reduced to the runner's capacity with source runner_capacity, and dispatched to that
// runner at the reduced size.
func TestPG_LocalRun_OverAskReducedToRunnerCapacity(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	small := e.laptop(e.personP, 2000, 2048, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, small)
	e.stream(small, types.CC1, types.CC2, types.CC3)
	e.assignSizedProfile("eight", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 16384}, types.GovernanceLimits{})
	created := e.create(limitsBody(`"placement":"local","runner_id":"` + small.String() + `","runner_pool_id":"` + laptops.ID.String() +
		`","resources":{"cpu_millis":4000,"memory_mib":8192}`))
	if created.RunnerID == nil || *created.RunnerID != small || created.ResourcesApplied == nil {
		t.Fatalf("created = %+v, want a run on %s with its applied size", created, small)
	}
	wantAmount(t, "cpu", created.ResourcesApplied.CPUMillis, 2000, true, types.LimitSourceRunner, "")
	wantAmount(t, "memory", created.ResourcesApplied.MemoryMiB, 2048, true, types.LimitSourceRunner, "")
	spec := e.dispatchedSpec(created.ID)
	if spec.RunnerID != small.String() || spec.Resources.CPUMillis != 2000 || spec.Resources.MemoryMiB != 2048 {
		t.Errorf("dispatched to %q with %+v, want %s at 2000 m / 2048 MiB", spec.RunnerID, spec.Resources, small)
	}
}

// A run's effective CPU, memory and disk are within the per-run limit of the runner it is placed on and
// the strictest of governance and the pool: the runner source is the largest per-run limit of the pool's
// runners, so the doors offer the biggest run one can take, and placement keeps only a runner whose own
// per-run limit holds the applied size. What the run reserves on that runner is that size.
func TestEffectiveLimitsIncludePlacedRunner(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	small := e.laptop(e.personP, 2000, 2048, types.CC1, types.CC2, types.CC3)
	big := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	for _, id := range []uuid.UUID{small, big} {
		e.addToPool(e.person, laptops.ID, id)
		e.stream(id, types.CC1, types.CC2, types.CC3)
	}
	narrow := types.RunnerPoolLimits{Background: &types.RunnerPoolBackgroundLimits{MemoryMiB: &types.RunnerPoolAmount{Default: 1024, Cap: 6144}}}
	if _, err := e.pg.UpdateRunnerPool(context.Background(), laptops.ID, e.revision(laptops.ID), nil, nil, &narrow, nil); err != nil {
		t.Fatal(err)
	}
	e.assignSizedProfile("eight", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 16384}, types.GovernanceLimits{})
	created := e.create(limitsBody(`"placement":"local","runner_pool_id":"` + laptops.ID.String() + `","resources":{"cpu_millis":8000,"memory_mib":16384}`))
	if created.RunnerID == nil || *created.RunnerID != big || created.ResourcesApplied == nil {
		t.Fatalf("created = %+v, want the run on the runner whose per-run limit holds it, %s", created, big)
	}
	wantAmount(t, "cpu", created.ResourcesApplied.CPUMillis, 4000, true, types.LimitSourceRunner, "")
	wantAmount(t, "memory", created.ResourcesApplied.MemoryMiB, 6144, true, types.LimitSourcePool, "Laptops")
	row, err := e.pg.GetRunner(context.Background(), big)
	if err != nil || row.Limits == nil {
		t.Fatal(err)
	}
	var cpu, mem, disk int64
	if err := e.pool.QueryRow(context.Background(), `SELECT reserved_cpu_millis, reserved_memory_mib, reserved_disk_mib FROM agent_runs WHERE id = $1`,
		created.ID).Scan(&cpu, &mem, &disk); err != nil {
		t.Fatal(err)
	}
	if cpu != 4000 || mem != 6144 || disk < 1 || disk > row.Limits.PerRun.DiskMiB {
		t.Fatalf("reserved %d m / %d MiB / %d MiB disk, want 4000 / 6144 / at most the runner's per-run %d", cpu, mem, disk, row.Limits.PerRun.DiskMiB)
	}
}

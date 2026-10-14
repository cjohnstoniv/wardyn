// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A run's CPU and memory are bounded after placement by the pool, governance, the deployment and the
// runner's capacity, and every door reports what applied (DS-5 §5.2). Against a real Postgres: guarded by
// WARDYN_TEST_PG, skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// limitsBackground is a 0.9 Background request, which a pool that sets limits takes.
const limitsBackground = `"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true`

func limitsBody(extra string) string {
	if extra == "" {
		return `{` + limitsBackground + `}`
	}
	return `{` + limitsBackground + `,` + extra + `}`
}

// sizedPool is a remote pool over both executors that takes Background runs with the given CPU limits.
func (e *placementEnv) sizedPool(name string, cpu types.RunnerPoolAmount) types.RunnerPool {
	e.t.Helper()
	p := e.createPool(name, types.RunnerPoolOrganization)
	e.addExecutors(p.ID, "alpha", "beta")
	limits := types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{CPUMillis: &cpu, MemoryMiB: &types.RunnerPoolAmount{Default: 1024, Cap: 8192},
			Lifetime: &types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}},
		Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
	}
	out, err := e.pg.UpdateRunnerPool(context.Background(), p.ID, e.revision(p.ID), nil, nil, &limits)
	if err != nil {
		e.t.Fatalf("set pool limits: %v", err)
	}
	return out
}

// assignSizedProfile gives the person a governance profile with this ceiling size and these limits.
func (e *placementEnv) assignSizedProfile(name string, size *types.ResourceLimits, limits types.GovernanceLimits) {
	e.t.Helper()
	p, err := e.pg.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{Name: name, CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{Resources: size}, Limits: limits})
	if err != nil {
		e.t.Fatalf("seed profile %q: %v", name, err)
	}
	e.seedAssignment(p, e.personP)
}

func (e *placementEnv) preflight(body string) preflightResponse {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", body)
	if w.Code != http.StatusOK {
		e.t.Fatalf("preflight = %d %s", w.Code, w.Body)
	}
	return decodeJSON[preflightResponse](e.t, w)
}

func (e *placementEnv) create(body string) createRunResponse {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	return decodeJSON[createRunResponse](e.t, w)
}

// dispatched is the size the sandbox was created with, once dispatch has run.
func (e *placementEnv) dispatched() runner.Resources {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); e.runner.creates() == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	e.runner.fakeRunner.mu.Lock()
	defer e.runner.fakeRunner.mu.Unlock()
	if e.runner.fakeRunner.createCalls+len(e.runner.onExecutor) == 0 {
		e.t.Fatal("dispatch never created a sandbox")
	}
	return e.runner.fakeRunner.lastSpec.Resources
}

// placeOnOwnRunner resolves a local request in the person's self-hosted pool exactly as stepPlacement
// does. Local runs are refused at every door until local runs are enabled (localPlacementRefusal), so a
// self-hosted run's size is read here, at the placement step that decides it.
func (e *placementEnv) placeOnOwnRunner(pool types.RunnerPool, runnerID fmt.Stringer, res *client.RunResources) (runPlacement, governanceCeiling) {
	e.t.Helper()
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerID: runnerID.String(), RunnerPoolID: pool.ID.String(), Resources: res}
	plan, refusal := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling, ask: resourceAsk(res, types.ResourceLimits{})})
	if refusal != nil {
		e.t.Fatalf("placement refused: %d %+v", refusal.status, refusal.body)
	}
	return plan, ceiling
}

func wantAmount(t *testing.T, what string, got client.AppliedAmount, applied int, capped bool, src types.LimitSource, name string) {
	t.Helper()
	if got.Applied != applied || got.Capped != capped || got.Source != src || got.SourceName != name {
		t.Errorf("%s = %+v, want applied %d capped %v source %q name %q", what, got, applied, capped, src, name)
	}
}

// T1: a pool that caps CPU at 2 under a governance ceiling of 8 cuts an ask of 4 to 2 and names the pool.
func TestPreflightAppliesPoolCPUCap(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.sizedPool("Small", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	e.assignSizedProfile("eight", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 8192}, types.GovernanceLimits{})
	pf := e.preflight(limitsBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":4000}`, pool.ID)))
	if pf.ResourcesApplied == nil || pf.Limits == nil {
		t.Fatalf("preflight carries no size: %+v %+v", pf.ResourcesApplied, pf.Limits)
	}
	wantAmount(t, "cpu", pf.ResourcesApplied.CPUMillis, 2000, true, types.LimitSourcePool, "Small")
	if pf.ResourcesApplied.CPUMillis.Requested != 4000 {
		t.Errorf("requested = %d, want 4000", pf.ResourcesApplied.CPUMillis.Requested)
	}
	if l := pf.Limits.CPUMillis; l.Cap.Value != 2000 || l.CapSource != types.LimitSourcePool {
		t.Errorf("limits cpu = %+v, want the pool's 2000", l)
	}
}

// T2: the size the 201 reports is the size dispatch creates the sandbox with, for a profile maximum above
// the default, a pool cap and a runner's capacity, and for a maximum under the default (#1949: the member
// cap is never below the deployment's default, so dispatch must not cut what the fold applied).
func TestCreateAndDispatchAgreeOnAppliedSize(t *testing.T) {
	def := int(sizing.EffectiveLimits().CPUMillis)
	for _, tc := range []struct {
		name  string
		setup func(e *placementEnv) string
		want  int
		src   types.LimitSource
	}{{
		name: "(a) profile max 8, no ceiling size, an ask of 8",
		setup: func(e *placementEnv) string {
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			e.assignSizedProfile("max-eight", nil, types.GovernanceLimits{MaxCPUMillis: 8000})
			return runBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":8000}`, remote.ID))
		},
		// #1949: the profile sets no size of its own, so its maximum is the size and the ask stands.
		want: 8000, src: "",
	}, {
		name: "(b) a pool cap",
		setup: func(e *placementEnv) string {
			pool := e.sizedPool("Small", types.RunnerPoolAmount{Default: 1000, Cap: 1500})
			return limitsBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":4000}`, pool.ID))
		},
		want: 1500, src: types.LimitSourcePool,
	}, {
		name: "(d) a profile maximum under the default, no ask",
		setup: func(e *placementEnv) string {
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			e.assignSizedProfile("under", nil, types.GovernanceLimits{MaxCPUMillis: def / 2})
			return runBody(fmt.Sprintf(`"runner_pool_id":%q`, remote.ID))
		},
		// No ask and no authored size: the deployment's default, which the maximum does not cut.
		want: def, src: types.LimitSourceDeployment,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			created := e.create(tc.setup(e))
			got := created.ResourcesApplied
			if got == nil {
				t.Fatal("the 201 carries no resources_applied")
			}
			if got.CPUMillis.Applied != tc.want || got.CPUMillis.Source != tc.src {
				t.Errorf("201 cpu = %+v, want %d from %s", got.CPUMillis, tc.want, tc.src)
			}
			res := e.dispatched()
			if int(res.CPUMillis) != got.CPUMillis.Applied || int(res.MemoryMiB) != got.MemoryMiB.Applied {
				t.Fatalf("DISAGREE: the 201 applied %d m / %d MiB, dispatch created %d m / %d MiB",
					got.CPUMillis.Applied, got.MemoryMiB.Applied, res.CPUMillis, res.MemoryMiB)
			}
		})
	}
}

// T2 (c): a self-hosted run's size is the runner's capacity, and dispatch's own size phase keeps it. The
// run cannot be created on a runner yet (localPlacementRefusal), so the placement step's answer is
// handed to dispatch's size path directly.
func TestCreateAndDispatchAgreeOnAppliedSize_RunnerCapacity(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	small := e.laptop(e.personP, 1200, 2048, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, small)
	// A profile maximum under the runner's size: the member cap is never below the default, so neither
	// placement nor dispatch cuts the runner's 1200.
	e.assignSizedProfile("under", nil, types.GovernanceLimits{MaxCPUMillis: 1000})
	plan, ceiling := e.placeOnOwnRunner(laptops, small, &client.RunResources{CPUMillis: 4000})
	wantAmount(t, "cpu", plan.applied.CPUMillis, 1200, true, types.LimitSourceRunner, "")
	policy := types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: plan.applied.CPUMillis.Applied, MemoryMiB: plan.applied.MemoryMiB.Applied}}
	res, _, _ := ephemeralDispatch(t, policy, types.SiteConfig{}, ceiling)
	if int(res.CPUMillis) != plan.applied.CPUMillis.Applied || int(res.MemoryMiB) != plan.applied.MemoryMiB.Applied {
		t.Fatalf("DISAGREE: placement applied %+v, dispatch sized %d m / %d MiB", plan.applied, res.CPUMillis, res.MemoryMiB)
	}
}

// T3: an ask of 4 CPU on the person's own 2 CPU runner is reduced to 2 at placement, never refused
// placement_capability; the runner stays eligible.
func TestOverAskOnSmallRunnerIsReducedNotRefused(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	small := e.laptop(e.personP, 2000, 2048, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, small)
	e.assignSizedProfile("eight", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 16384}, types.GovernanceLimits{})
	plan, _ := e.placeOnOwnRunner(laptops, small, &client.RunResources{CPUMillis: 4000, MemoryMiB: 8192})
	wantAmount(t, "cpu", plan.applied.CPUMillis, 2000, true, types.LimitSourceRunner, "")
	wantAmount(t, "memory", plan.applied.MemoryMiB, 2048, true, types.LimitSourceRunner, "")
	if len(plan.eligible) != 1 || plan.eligible[0].id != small.String() {
		t.Errorf("eligible = %+v, want the person's runner", plan.eligible)
	}
}

// T4: a pool's default sizes a run that asks for nothing and whose policy sets no size.
func TestPoolDefaultSizesUntouchedRun(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.sizedPool("Pool", types.RunnerPoolAmount{Default: 1000, Cap: 4000})
	created := e.create(limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)))
	wantAmount(t, "cpu", created.ResourcesApplied.CPUMillis, 1000, false, types.LimitSourcePool, "Pool")
	if res := e.dispatched(); res.CPUMillis != 1000 {
		t.Errorf("dispatched %d m, want the pool default 1000", res.CPUMillis)
	}
}

// A profiled member who asks for nothing, under a profile that sets no size of its own, gets the
// pool's lower default: the ceiling's inherited size is not the policy's ask.
func TestPoolDefaultSizesProfiledMemberWhoAsksNothing(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.sizedPool("Pool", types.RunnerPoolAmount{Default: 1000, Cap: 4000})
	e.assignSizedProfile("max-eight", nil, types.GovernanceLimits{MaxCPUMillis: 8000})
	created := e.create(limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)))
	wantAmount(t, "cpu", created.ResourcesApplied.CPUMillis, 1000, false, types.LimitSourcePool, "Pool")
	if res := e.dispatched(); res.CPUMillis != 1000 {
		t.Errorf("dispatched %d m, want the pool default 1000", res.CPUMillis)
	}
}

// DR-0112 (a): the ask is the source policy's own size, never the clamp's fill. An inline policy that
// names no size, in a pool whose default is 1 CPU, gets 1 CPU, not the size the member clamp filled in.
func TestPoolDefaultBeatsTheClampFill(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.sizedPool("Pool", types.RunnerPoolAmount{Default: 1000, Cap: 4000})
	pf := e.preflight(limitsBody(fmt.Sprintf(`"runner_pool_id":%q,"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]}`, pool.ID)))
	wantAmount(t, "cpu", pf.ResourcesApplied.CPUMillis, 1000, false, types.LimitSourcePool, "Pool")
	if pf.ResourcesApplied.CPUMillis.Requested != 0 {
		t.Errorf("requested = %d, want 0 (the policy names no size)", pf.ResourcesApplied.CPUMillis.Requested)
	}
}

// T5: the limits the pool list will show for a run type are the builder's, the same preflight answers
// for a request that names no policy and no size, governed and not. The list itself lands with
// B-pool-catalogue; this pins that the builder takes the caller's REAL ceiling.
func TestPoolListLimitsMatchPreflight(t *testing.T) {
	for _, governed := range []bool{true, false} {
		t.Run(fmt.Sprintf("governed=%v", governed), func(t *testing.T) {
			e := newPlacementEnv(t)
			pool := e.sizedPool("Pool", types.RunnerPoolAmount{Default: 1000, Cap: 16000})
			e.assignSizedProfile("six", &types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 8192}, types.GovernanceLimits{})
			body := limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID))
			var pf preflightResponse
			ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
			if governed {
				pf = e.preflight(body)
			} else {
				w := e.admin(http.MethodPost, "/api/v1/runs/preflight", body)
				if w.Code != http.StatusOK {
					t.Fatalf("preflight = %d %s", w.Code, w.Body)
				}
				pf = decodeJSON[preflightResponse](t, w)
				ctx = context.Background() // the admin token: no person, outside governance
			}
			ceiling, err := e.srv.effectiveCeiling(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// The pool's one runner is the test's remote-hosted runner with roomy limits: its per-run limit is the runner source.
			each := roomyRemoteLimits().PerRun
			runners := []runnerpool.RunnerCapacity{{CPUMillis: each.CPUMillis, MemoryMiB: each.MemoryMiB, DiskMiB: each.DiskMiB}}
			sources, refusal, err := e.srv.limitSources(ctx, types.RunnerPoolRunBackground, &pool, ceiling, types.ResourceLimits{}, runners)
			if err != nil || refusal != nil {
				t.Fatalf("limitSources: %v %v", refusal, err)
			}
			list, err := runnerpool.Effective(types.RunnerPoolRunBackground, sources...)
			if err != nil {
				t.Fatal(err)
			}
			if pf.Limits == nil || !reflect.DeepEqual(*pf.Limits, list) {
				t.Fatalf("preflight limits %+v, the builder answers %+v", pf.Limits, list)
			}
			wantCap, wantSrc := 16000, types.LimitSourcePool
			if governed {
				wantCap, wantSrc = 6000, types.LimitSourceGovernance
			}
			if l := list.CPUMillis; l.Cap.Value != wantCap || l.CapSource != wantSrc {
				t.Errorf("cpu cap = %+v, want %d from %s", l, wantCap, wantSrc)
			}
		})
	}
}

// T8: run.create carries the size the 201 reports.
func TestRunCreateAuditCarriesAppliedResources(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.sizedPool("Small", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	created := e.create(limitsBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":4000}`, pool.ID)))
	rows := e.audits("run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create rows = %d, want 1", len(rows))
	}
	var data struct {
		ResourcesApplied *client.AppliedResources `json:"resources_applied"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ResourcesApplied == nil || *data.ResourcesApplied != *created.ResourcesApplied {
		t.Fatalf("run.create resources_applied = %+v, the 201 says %+v", data.ResourcesApplied, created.ResourcesApplied)
	}
	wantAmount(t, "audited cpu", data.ResourcesApplied.CPUMillis, 2000, true, types.LimitSourcePool, "Small")
}

// The cap warning and resources_applied agree, on the 201 and on run.create: an inline policy asking 8
// CPU under a profile whose maximum is 8 and which sets no size is not cut and not warned (#1949); the
// same ask under a maximum of 4 is cut to 4 and warned.
func TestCapWarningAgreesWithAppliedResources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		max    int
		want   int
		capped bool
	}{
		{"a maximum of 8 with no size of its own keeps an ask of 8", 8000, 8000, false},
		{"a maximum of 4 cuts an ask of 8", 4000, 4000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			e.assignSizedProfile("sized", nil, types.GovernanceLimits{MaxCPUMillis: tc.max})
			created := e.create(runBody(fmt.Sprintf(`"runner_pool_id":%q,"inline_policy":{"min_confinement_class":"CC2","resources":{"cpu_millis":8000}}`, remote.ID)))
			got := created.ResourcesApplied.CPUMillis
			if got.Applied != tc.want || got.Capped != tc.capped {
				t.Fatalf("cpu = %+v, want applied %d capped %v", got, tc.want, tc.capped)
			}
			if warned := slices.Contains(created.Warnings, composer.WarnResourcesCapped); warned != tc.capped {
				t.Errorf("201 warnings %v carry the cap sentence = %v, resources_applied capped = %v", created.Warnings, warned, tc.capped)
			}
			rows := e.audits("run.create")
			if len(rows) != 1 {
				t.Fatalf("run.create rows = %d, want 1", len(rows))
			}
			var data struct {
				ResourcesApplied *client.AppliedResources `json:"resources_applied"`
				ClampWarnings    []string                 `json:"clamp_warnings"`
			}
			if err := json.Unmarshal(rows[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.ResourcesApplied == nil || data.ResourcesApplied.CPUMillis != got {
				t.Errorf("run.create resources_applied = %+v, the 201 says %+v", data.ResourcesApplied, got)
			}
			if warned := slices.Contains(data.ClampWarnings, composer.WarnResourcesCapped); warned != tc.capped {
				t.Errorf("run.create clamp_warnings %v carry the cap sentence = %v, resources_applied capped = %v", data.ClampWarnings, warned, tc.capped)
			}
		})
	}
}

// T2, a deployment whose default policy (4000m / 8192 MiB) is above the platform default (2000m /
// 4096 MiB): every way a member or an operator reaches an untouched size gets the deployment's own
// size, an ask up to it is not cut or warned, a profile maximum below it does not cut it (#1949), and
// dispatch creates the sandbox at exactly what the 201 reported.
func TestCreateAndDispatchAgreeOnAppliedSize_DeploymentDefaultAbovePlatform(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	for _, tc := range []struct {
		name    string
		profile *types.GovernanceLimits // nil: the person is unassigned
		admin   bool
		extra   func(e *placementEnv) string
	}{
		{"profiled member, inline policy with no size", &types.GovernanceLimits{}, false,
			func(*placementEnv) string { return `"inline_policy":{"min_confinement_class":"CC2"}` }},
		{"profiled member, stored policy with no size", &types.GovernanceLimits{}, false, func(e *placementEnv) string {
			p, err := e.pg.CreatePolicy(context.Background(), types.RunPolicy{ID: uuid.New(), Name: "plain", Spec: types.RunPolicySpec{MinConfinementClass: types.CC2}})
			if err != nil {
				e.t.Fatal(err)
			}
			return fmt.Sprintf(`"policy_id":%q`, p.ID)
		}},
		{"profiled member, inline policy asking 4000", &types.GovernanceLimits{}, false,
			func(*placementEnv) string {
				return `"inline_policy":{"min_confinement_class":"CC2","resources":{"cpu_millis":4000}}`
			}},
		{"profiled member, the profile as policy", &types.GovernanceLimits{}, false, func(*placementEnv) string { return "" }},
		{"unassigned member", nil, false, func(*placementEnv) string { return "" }},
		{"operator", nil, true, func(*placementEnv) string { return "" }},
		{"profile maximum 3000, inline policy with no size", &types.GovernanceLimits{MaxCPUMillis: 3000}, false,
			func(*placementEnv) string { return `"inline_policy":{"min_confinement_class":"CC2"}` }},
		{"profile maximum 3000, the profile as policy", &types.GovernanceLimits{MaxCPUMillis: 3000}, false,
			func(*placementEnv) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			e.srv.cfg.DefaultPolicy.Resources = &types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}
			remote := e.createPool("Remote", types.RunnerPoolOrganization)
			e.addExecutors(remote.ID, "alpha", "beta")
			if tc.profile != nil {
				e.assignSizedProfile("plain", nil, *tc.profile)
			}
			extra := fmt.Sprintf(`"runner_pool_id":%q`, remote.ID)
			if x := tc.extra(e); x != "" {
				extra += "," + x
			}
			var w *httptest.ResponseRecorder
			if tc.admin {
				w = e.admin(http.MethodPost, "/api/v1/runs", runBody(extra))
			} else {
				w = e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(extra))
			}
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			created := decodeJSON[createRunResponse](t, w)
			got := created.ResourcesApplied
			if got.CPUMillis.Applied != 4000 || got.MemoryMiB.Applied != 8192 || got.CPUMillis.Capped || got.MemoryMiB.Capped {
				t.Errorf("201 = %+v, want 4000m / 8192 MiB, not capped", got)
			}
			if slices.Contains(created.Warnings, composer.WarnResourcesCapped) {
				t.Errorf("201 warns %q for a size nobody cut", composer.WarnResourcesCapped)
			}
			res := e.dispatched()
			if int(res.CPUMillis) != got.CPUMillis.Applied || int(res.MemoryMiB) != got.MemoryMiB.Applied {
				t.Fatalf("DISAGREE: the 201 applied %d m / %d MiB, dispatch created %d m / %d MiB",
					got.CPUMillis.Applied, got.MemoryMiB.Applied, res.CPUMillis, res.MemoryMiB)
			}
		})
	}
}

// A runner of either kind that declares no limits is never a candidate: a launch that could only go
// there is refused runner_limits_unset at every door, never a server error, and a pool with a runner
// that declares them places on it alone. No resource of the run that does place is unlimited: the
// placed runner's per-run limit bounds CPU, memory and the main drive.
func TestRunnerWithoutLimitsIsNeverACandidate(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	bare := e.laptop(e.personP, 0, 0, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, bare)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerPoolID: laptops.ID.String()}
	_, refusal := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
	if refusal == nil || refusal.status != http.StatusUnprocessableEntity || refusal.body.Reason != string(placement.ReasonRunnerLimitsUnset) ||
		refusal.body.Error != runnerpool.RunnerLimitsUnsetMsg("Laptops") {
		t.Fatalf("placement on a runner with no limits = %+v, want 422 runner_limits_unset", refusal)
	}
	body := runBody(fmt.Sprintf(`"placement":"local","runner_pool_id":%q`, laptops.ID))
	for _, door := range placementDoors {
		if w := e.call(e.person, http.MethodPost, door, body); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(placement.ReasonRunnerLimitsUnset)) {
			t.Errorf("%s = %d %s, want 422 runner_limits_unset", door, w.Code, w.Body)
		}
	}

	sized := e.laptop(e.personP, 2000, 4096, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, sized)
	plan, refusal := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
	if refusal != nil {
		t.Fatalf("placement beside a runner with limits = %+v", refusal.body)
	}
	if len(plan.eligible) != 1 || plan.eligible[0].id != sized.String() {
		t.Fatalf("eligible = %v, want only the runner that declares limits", ids(plan.eligible))
	}
	for name, l := range map[string]types.EffectiveLimit{"cpu": plan.limits.CPUMillis, "memory": plan.limits.MemoryMiB, "disk": plan.limits.DiskMiB} {
		if l.Cap.Unlimited || l.Cap.Value < 1 {
			t.Errorf("%s cap = %+v, want bounded by the placed runner", name, l.Cap)
		}
	}

	// A remote-hosted runner that declares no limits is no candidate either.
	remote := e.srv.configuredExecutors()[0]
	if _, err := e.pool.Exec(context.Background(), `UPDATE remote_runners SET max_runs = NULL, total_cpu_millis = NULL, total_memory_mib = NULL,
		total_disk_mib = NULL, run_cpu_millis = NULL, run_memory_mib = NULL, run_disk_mib = NULL WHERE id = $1`, remote); err != nil {
		t.Fatal(err)
	}
	org := e.createPool("Org", types.RunnerPoolOrganization)
	e.addExecutors(org.ID, remote)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", runBody(fmt.Sprintf(`"runner_pool_id":%q`, org.ID)))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), string(placement.ReasonRunnerLimitsUnset)) {
		t.Fatalf("a remote-hosted runner with no limits = %d %s, want 422 runner_limits_unset", w.Code, w.Body)
	}
}

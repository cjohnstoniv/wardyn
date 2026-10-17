// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The enriched pool list (GET /runner-pools) against a real Postgres. Guarded by WARDYN_TEST_PG:
// skipped cleanly when unset, must PASS when set.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// list is GET /runner-pools as c (nil: the admin token) with the query; it must answer 200.
func (e *placementEnv) list(c *http.Cookie, query string) (client.RunnerPoolList, string) {
	e.t.Helper()
	path := "/api/v1/runner-pools" + query
	var w *httptest.ResponseRecorder
	if c == nil {
		w = e.admin(http.MethodGet, path, "")
	} else {
		w = e.call(c, http.MethodGet, path, "")
	}
	if w.Code != http.StatusOK {
		e.t.Fatalf("list %s = %d %s", query, w.Code, w.Body)
	}
	return decodeJSON[client.RunnerPoolList](e.t, w), w.Body.String()
}

func entryOf(t *testing.T, list client.RunnerPoolList, id uuid.UUID) client.RunnerPoolCatalogueEntry {
	t.Helper()
	for _, p := range list.Pools {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("pool %s is not listed", id)
	return client.RunnerPoolCatalogueEntry{}
}

func (e *placementEnv) runnersOn(on bool) {
	e.t.Helper()
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: on}}); err != nil {
		e.t.Fatal(err)
	}
}

// T9: each reason a usable pool may be unavailable for, in the order the list checks them.
func TestPoolListAvailabilityReasons(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Remote", types.RunnerPoolOrganization)
	e.addExecutors(remote.ID, "alpha")
	empty := e.createPool("Empty", types.RunnerPoolOrganization)
	off := e.createPool("Off", types.RunnerPoolOrganization)
	if w := e.admin(http.MethodPut, poolPath(off.ID), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, e.revision(off.ID))); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	background := e.limitedPool("Background only", types.CC1, types.CC2, types.CC3)
	home := e.createPool("Home server", types.RunnerPoolUser)
	e.addToPool(e.person, home.ID, e.claimedRunner(e.personP)) // claimed, never online
	bare := e.createOwnPool(e.person, "Bare")

	reasonOf := func(c *http.Cookie, query string, id uuid.UUID) runnerpool.Reason {
		t.Helper()
		list, _ := e.list(c, query)
		got := entryOf(t, list, id)
		if (got.Reason == "") != (got.Availability == client.RunnerPoolAvailable) {
			t.Errorf("%s reads %s with reason %q", got.Name, got.Availability, got.Reason)
		}
		return got.Reason
	}
	want := func(name string, got, want runnerpool.Reason) {
		t.Helper()
		if got != want {
			t.Errorf("%s: reason %q, want %q", name, got, want)
		}
	}
	want("switched off", reasonOf(e.person, "", off.ID), runnerpool.ReasonUnavailable)
	want("no configured executor", reasonOf(e.person, "", empty.ID), runnerpool.ReasonNoEligibleMember)
	want("an available remote pool", reasonOf(e.person, "", remote.ID), "")
	want("own runners all offline", reasonOf(e.person, "", home.ID), runnerpool.AvailabilityNoOnlineRunner)
	want("no own runner in the pool", reasonOf(e.person, "", bare.ID), runnerpool.ReasonNoEligibleMember)
	if list, _ := e.list(e.other, ""); slices.ContainsFunc(list.Pools, func(p client.RunnerPoolCatalogueEntry) bool { return p.ID == home.ID }) {
		t.Error("another person's list shows the person's own Self-Hosted pool")
	}
	e.runnersOn(false)
	want("runners switched off", reasonOf(e.person, "", home.ID), runnerpool.AvailabilityPlacementUnavailable)
	e.runnersOn(true)

	want("?run_type= the pool refuses", reasonOf(e.person, "?run_type=interactive", background.ID), runnerpool.ReasonRunTypeNotAllowed)
	list, _ := e.list(e.person, "?run_type=background")
	if got := entryOf(t, list, background.ID); got.Reason != "" || len(got.Limits) != 1 || got.Limits[0].RunType != types.RunnerPoolRunBackground {
		t.Errorf("?run_type=background on a Background pool: %+v", got)
	}
	if got := entryOf(t, list, remote.ID); len(got.Limits) != 1 || got.RunTypesSource != client.RunTypesSourceNone || len(got.RunTypes) != 2 {
		t.Errorf("?run_type=background on a pool with no limits: %+v", got)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/runner-pools?run_type=x", ""); w.Code != http.StatusBadRequest || wireReason(t, w) != string(runnerpool.ReasonInvalid) {
		t.Errorf("?run_type=x = %d %s, want 400 runner_pool_invalid", w.Code, w.Body)
	}

	e.assignProfile("no remote", types.GovernanceLimits{DenyPlacements: []types.Placement{types.PlacementRemote}})
	want("deny_placements remote", reasonOf(e.person, "", remote.ID), runnerpool.AvailabilityPlacementDenied)
	want("deny wins over the run type", reasonOf(e.person, "?run_type=interactive", background.ID), runnerpool.AvailabilityPlacementDenied)
	want("a self-hosted pool under a remote deny", reasonOf(e.person, "", home.ID), runnerpool.AvailabilityNoOnlineRunner)
	// LE-11: an online, runner-enabled self-hosted pool is available, never placement_unavailable.
	e.addToPool(e.person, home.ID, e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3))
	want("an own runner online", reasonOf(e.person, "", home.ID), "")
}

// T10: another person's runner in the same pool (a membership no route can write) is never named, never
// counted as capacity and never makes the pool available to the caller.
func TestPoolListNeverNamesAnotherPersonsRunner(t *testing.T) {
	e := newPlacementEnv(t)
	home := e.createPool("Home server", types.RunnerPoolUser)
	mine := e.claimedRunner(e.personP)
	e.addToPool(e.person, home.ID, mine)
	theirs := e.laptop(e.otherP, 8000, 16384, types.CC1, types.CC2, types.CC3)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, home.ID, theirs); err != nil {
		t.Fatal(err)
	}

	list, raw := e.list(e.person, "")
	got := entryOf(t, list, home.ID)
	if strings.Contains(raw, theirs.String()) {
		t.Fatalf("the list names another person's runner: %s", raw)
	}
	if got.Reason != runnerpool.AvailabilityNoOnlineRunner {
		t.Errorf("another person's online runner flipped the pool: %+v", got)
	}
	if want := []client.PoolRunner{{ID: mine, Name: "laptop", Online: false}}; !reflect.DeepEqual(got.MyRunners, want) {
		t.Errorf("my_runners = %+v, want %+v", got.MyRunners, want)
	}
	for _, o := range got.BarrierOptions {
		if o.Available || o.ReasonCode == runnerpool.BarrierNotOffered {
			t.Errorf("barrier %+v is judged on another person's runner", o)
		}
	}
	for _, l := range got.Limits {
		if l.CPUMillis.CapSource == types.LimitSourceRunner {
			t.Errorf("another person's runner is counted as capacity: %+v", l.CPUMillis)
		}
	}

	// Once the caller's own runner is online, its own capacity bounds the pool, never the other's.
	e.online[mine] = true
	e.runner.runnerFacts[mine.String()] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC2},
		CapacityAdvertised: true, CPUMillisMax: 1000, MemoryMiBMax: 2048}
	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET run_cpu_millis = 1000, run_memory_mib = 2048 WHERE id = $1`, mine); err != nil {
		t.Fatal(err)
	}
	list, _ = e.list(e.person, "")
	got = entryOf(t, list, home.ID)
	if got.Availability != client.RunnerPoolAvailable || len(got.MyRunners) != 1 || !got.MyRunners[0].Online {
		t.Fatalf("with the caller's runner online: %+v", got)
	}
	for _, l := range got.Limits {
		if l.CPUMillis.Cap.Value != 1000 || l.CPUMillis.CapSource != types.LimitSourceRunner {
			t.Errorf("%s cpu = %+v, want the caller's own 1000 from runner_capacity", l.RunType, l.CPUMillis)
		}
	}
}

// T11: a pool a security admin may only manage carries no limits, run types, barriers, runners or slot counts.
func TestPoolListManagerRowsCarryNoFacts(t *testing.T) {
	e := newPlacementEnv(t)
	narrowed := e.sizedPool("Narrowed", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`); w.Code >= 300 {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	list, raw := e.list(e.alice, "")
	got := entryOf(t, list, narrowed.ID)
	if got.Reason != runnerpool.ReasonNotFound || got.Availability != client.RunnerPoolUnavailable {
		t.Fatalf("manager row = %+v, want unavailable runner_pool_not_found", got)
	}
	if len(got.Limits) != 0 || len(got.RunTypes) != 0 || len(got.BarrierOptions) != 0 || got.MyRunners != nil || got.RunTypesSource != "" || got.Slots != nil {
		t.Errorf("manager row carries facts: %+v", got)
	}
	if !strings.Contains(raw, `"run_types":[]`) || !strings.Contains(raw, `"barrier_options":[]`) {
		t.Errorf("manager row's empty sets are not [] on the wire: %s", raw)
	}
}

// DR-0091: the fields the dry doors' runner_pools carry are the list's, for the same pool and caller.
func TestPoolListSharedFieldsMatchTheDryDoors(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.sizedPool("Remote", types.RunnerPoolAmount{Default: 1000, Cap: 2000})
	e.createPool("Empty", types.RunnerPoolOrganization)
	home := e.createPool("Home server", types.RunnerPoolUser)
	e.addToPool(e.person, home.ID, e.claimedRunner(e.personP))
	e.createPool("Not mine", types.RunnerPoolUser)

	list, _ := e.list(e.person, "")
	body := limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, remote.ID))
	for _, door := range []string{"/api/v1/runs/preflight", "/api/v1/runs/policy-preview"} {
		w := e.call(e.person, http.MethodPost, door, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body)
		}
		answer := decodeJSON[struct {
			RunnerPools    []client.RunnerPoolChoice `json:"runner_pools"`
			BarrierOptions []client.BarrierOption    `json:"barrier_options"`
		}](t, w)
		if len(answer.RunnerPools) != len(list.Pools) {
			t.Fatalf("%s has %d pools, the list %d", door, len(answer.RunnerPools), len(list.Pools))
		}
		for _, choice := range answer.RunnerPools {
			listed := entryOf(t, list, choice.ID).RunnerPoolChoice
			listed.Limits, listed.Barriers = nil, nil
			if !reflect.DeepEqual(choice, listed) {
				t.Errorf("%s says %+v, the list %+v", door, choice, listed)
			}
		}
		if door == "/api/v1/runs/preflight" && len(answer.BarrierOptions) != 3 {
			t.Errorf("preflight barrier_options = %+v, want the pool's three", answer.BarrierOptions)
		}
	}
	if got := entryOf(t, list, home.ID).Reason; got != runnerpool.AvailabilityNoOnlineRunner {
		t.Errorf("the self-hosted pool reads %q, want runner_pool_no_online_runner on both", got)
	}
}

// T5 (list half): the list's limits for a run type are preflight's for a request that names no policy
// and no size, governed, ungoverned, and under a profile whose size is below the deployment default.
func TestPoolListLimitsMatchPreflightForAnUntouchedRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   *types.ResourceLimits
		limits types.GovernanceLimits
		gov    bool
	}{
		{"governed", &types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 8192}, types.GovernanceLimits{}, true},
		{"below the default", &types.ResourceLimits{CPUMillis: 500, MemoryMiB: 512}, types.GovernanceLimits{}, true},
		// The profile's own size is wider than its maximums: the run gets the maximums, and so must the list.
		{"authored wider than its max", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 16384}, types.GovernanceLimits{MaxCPUMillis: 1000, MaxMemoryMiB: 1024}, true},
		{"ungoverned", nil, types.GovernanceLimits{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			pool := e.sizedPool("Pool", types.RunnerPoolAmount{Default: 1000, Cap: 16000})
			var c *http.Cookie
			if tc.gov {
				e.assignSizedProfile("sized", tc.size, tc.limits)
				c = e.person
			}
			path := "/api/v1/runs/preflight"
			body := limitsBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID))
			w := e.admin(http.MethodPost, path, body)
			if c != nil {
				w = e.call(c, http.MethodPost, path, body)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("preflight = %d %s", w.Code, w.Body)
			}
			pf := decodeJSON[preflightResponse](t, w)
			list, _ := e.list(c, "?run_type=background")
			got := entryOf(t, list, pool.ID)
			if pf.Limits == nil || len(got.Limits) != 1 || !reflect.DeepEqual(got.Limits[0], *pf.Limits) {
				t.Fatalf("list limits %+v, preflight %+v", got.Limits, pf.Limits)
			}
		})
	}
}

// Only the dry doors answer the caller's pool choices; create never reads them, so a pool the run is
// not using (a self-hosted pool whose runner reads fail, say) cannot fail its create.
func TestCreateNeverReadsThePoolChoices(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Remote", types.RunnerPoolOrganization)
	e.addExecutors(remote.ID, "alpha")
	e.createPool("Other", types.RunnerPoolUser)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", RunnerPoolID: remote.ID.String()}
	for _, dry := range []bool{false, true} {
		plan, refusal := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling, dryDoor: dry})
		if refusal != nil {
			t.Fatalf("dryDoor=%v: placement refused: %d %+v", dry, refusal.status, refusal.body)
		}
		want := 0
		if dry {
			want = 2 // Remote and Other
		}
		if got := len(plan.choices); got != want {
			t.Errorf("dryDoor=%v: %d pool choices read, want %d", dry, got, want)
		}
	}
}

// countingInspector counts how often each executor's facts are asked for.
type countingInspector struct {
	*poolRunner
	asked map[string]int
}

func (c *countingInspector) ExecutorFacts(ctx context.Context, name string) (runner.TargetFacts, bool) {
	c.asked[name]++
	return c.poolRunner.ExecutorFacts(ctx, name)
}

// One list asks each executor for its facts at most once, however many pools hold it, so a failing
// executor (whose answer the orchestrator does not cache) is probed once per list, not once per pool.
func TestPoolListAsksEachExecutorOnce(t *testing.T) {
	e := newPlacementEnv(t)
	for _, name := range []string{"One", "Two", "Three"} {
		p := e.createPool(name, types.RunnerPoolOrganization)
		e.addExecutors(p.ID, "alpha", "beta")
	}
	counter := &countingInspector{poolRunner: e.runner, asked: map[string]int{}}
	e.srv.cfg.Runner = counter
	e.list(e.person, "")
	for _, ex := range []string{"alpha", "beta"} {
		if n := counter.asked[ex]; n != 1 {
			t.Errorf("executor %s asked %d times in one list, want 1", ex, n)
		}
	}
}

// FUG-L402: every list row carries the pool's own state, revision and raw limits, the ones
// GET /runner-pools/{id} answers, including an oversight row (a person's pool the administrator may
// only manage), so the Pools tab reads no pool one by one.
func TestPoolListCarriesEachPoolsOwnRecord(t *testing.T) {
	e := newPlacementEnv(t)
	limited := e.limitedPool("Limited", types.CC1, types.CC2)
	off := e.createPool("Off", types.RunnerPoolOrganization)
	if w := e.admin(http.MethodPut, poolPath(off.ID), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, e.revision(off.ID))); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	theirs := e.createOwnPool(e.other, "Theirs")

	list, _ := e.list(nil, "")
	for _, p := range []types.RunnerPool{limited, off, theirs} {
		got := entryOf(t, list, p.ID)
		w := e.admin(http.MethodGet, poolPath(p.ID), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p.Name, w.Code, w.Body)
		}
		want := decodeJSON[types.RunnerPool](t, w)
		if got.State != want.State || got.Revision != want.Revision || !reflect.DeepEqual(got.PoolLimits, want.Limits) {
			t.Errorf("%s: list row state=%q revision=%d limits=%+v, the pool's own read says %q %d %+v",
				p.Name, got.State, got.Revision, got.PoolLimits, want.State, want.Revision, want.Limits)
		}
	}
	if got := entryOf(t, list, theirs.ID); got.Revision == 0 || got.Owner == "" {
		t.Errorf("the oversight row = %+v, want its owner and revision", got)
	}
	if got := entryOf(t, list, limited.ID); got.PoolLimits == nil || got.State != types.RunnerPoolActive {
		t.Errorf("the limited pool's row = %+v, want active with its raw limits", got)
	}
	if got := entryOf(t, list, off.ID); got.State != types.RunnerPoolDisabled {
		t.Errorf("the switched-off pool's row state = %q, want disabled", got.State)
	}
}

// FR-0130: both dry doors answer the caller's own runners in each pool (my_runners), as the pool list
// does, and never another person's, so Review can say how many of them are online.
func TestDryDoorsCarryTheCallersOwnRunnersInEachPool(t *testing.T) {
	e := newPlacementEnv(t)
	home := e.createPool("Home server", types.RunnerPoolUser)
	mine := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.maxRuns(mine, e.personP, types.RunnerMaxRunsCeiling)
	e.addToPool(e.person, home.ID, mine)
	e.online[mine] = true
	theirs := e.laptop(e.otherP, 8000, 16384, types.CC1, types.CC2, types.CC3)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, home.ID, theirs); err != nil {
		t.Fatal(err)
	}
	bare := e.createOwnPool(e.person, "Bare")
	want := []client.PoolRunner{{ID: mine, Name: "laptop", Online: true}}

	for _, door := range placementDoors[1:] {
		w := e.call(e.person, http.MethodPost, door, runBody(`"runner_pool_id":"`+home.ID.String()+`"`))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), theirs.String()) {
			t.Fatalf("%s names another person's runner: %s", door, w.Body)
		}
		choices := decodeJSON[struct {
			RunnerPools []client.RunnerPoolChoice `json:"runner_pools"`
		}](t, w).RunnerPools
		var sawHome, sawBare bool
		for _, c := range choices {
			switch c.ID {
			case home.ID:
				sawHome = true
				if !reflect.DeepEqual(c.MyRunners, want) {
					t.Errorf("%s: my_runners = %+v, want %+v", door, c.MyRunners, want)
				}
			case bare.ID:
				sawBare = true
				if len(c.MyRunners) != 0 {
					t.Errorf("%s: a pool with none of the caller's runners lists %+v", door, c.MyRunners)
				}
			}
		}
		if !sawHome || !sawBare {
			t.Errorf("%s: choices %+v, want both pools", door, choices)
		}
	}
}

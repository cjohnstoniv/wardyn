// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lifetimePool is a remote-provided pool on executor alpha whose Background lifetime is lifetime (a
// RunnerPoolDuration as JSON); an empty lifetime is a pool with no limits.
func (e *placementEnv) lifetimePool(name, lifetime string) types.RunnerPool {
	e.t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	if lifetime != "" {
		body = fmt.Sprintf(`{"name":%q,"limits":{"background":{"cpu_millis":{"default":500,"cap":4000},`+
			`"memory_mib":{"default":512,"cap":8192},"lifetime":%s},"barriers":["CC1","CC2","CC3"]}}`, name, lifetime)
	}
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	p := decodeJSON[types.RunnerPool](e.t, w)
	e.addExecutors(p.ID, "alpha")
	return p
}

// createOn launches a Background run on pool with extra fields and returns the stored run and the 201's warnings.
func (e *placementEnv) createOn(pool types.RunnerPool, extra string) (types.AgentRun, []string) {
	e.t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/runs", endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`+extra))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var resp struct {
		types.AgentRun
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatal(err)
	}
	run, err := e.pg.GetRun(context.Background(), resp.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return run, resp.Warnings
}

// patchEnd asks for a new end on run as the admin token and returns the answer.
func (e *placementEnv) patchEnd(run types.AgentRun, endsAt string) (int, runEndWaitResponse, string) {
	e.t.Helper()
	w := e.admin(http.MethodPatch, "/api/v1/runs/"+run.ID.String(), `{"ends_at":`+endsAt+`}`)
	var resp runEndWaitResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp, w.Body.String()
}

func atOffset(run types.AgentRun, got *time.Time, d time.Duration) bool {
	return got != nil && got.Sub(run.CreatedAt.Add(d)).Abs() < time.Second
}

// A pool that sets no lifetime bound leaves the end as today at every door: an ask is kept, No end is
// on offer, and a later PATCH is not capped. A deployment age shorter than a pool's maximum still binds.
func TestPG_RunEndUnderAPoolThatDoesNotBindIt(t *testing.T) {
	e := newPlacementEnv(t)
	for _, pool := range []types.RunnerPool{e.lifetimePool("No limits", ""), e.lifetimePool("Unlimited", `{"default_sec":0,"unlimited":true}`)} {
		noEnd := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`","no_end":true`)
		for _, door := range placementDoors[1:] {
			if w := e.admin(http.MethodPost, door, noEnd); w.Code != http.StatusOK {
				t.Errorf("%s: No end at %s = %d %s, want 200", pool.Name, door, w.Code, w.Body)
			}
		}
		run, _ := e.createOn(pool, fmt.Sprintf(`,"ends_after_sec":%d`, 30*secDay))
		if !atOffset(run, run.EndsAt, 30*24*time.Hour) || run.LifetimeCapAt != nil {
			t.Errorf("%s: ends %v, cap %v; want the 30 days asked and no cap", pool.Name, run.EndsAt, run.LifetimeCapAt)
		}
		code, resp, raw := e.patchEnd(run, `"`+run.CreatedAt.Add(60*24*time.Hour).UTC().Format(time.RFC3339)+`"`)
		if code != http.StatusOK || len(resp.Capped) != 0 || resp.LatestEnd != nil {
			t.Errorf("%s: PATCH = %d %s; want 200, not capped", pool.Name, code, raw)
		}
	}

	e.srv.cfg.RunMaxAge = time.Hour
	run, _ := e.createOn(e.lifetimePool("Two hours", `{"default_sec":1800,"max_sec":7200}`), fmt.Sprintf(`,"ends_after_sec":%d`, secDay))
	if !atOffset(run, run.EndsAt, time.Hour) || !atOffset(run, run.LifetimeCapAt, time.Hour) || run.LifetimeCapSource != types.LimitSourceDeployment {
		t.Errorf("ends %v, cap %v (%q); want both at an hour from the deployment", run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource)
	}
}

// R2-0154: a pool's lifetime maximum, shorter than the run's ask, bounds the run's end at every door.
// Create cuts the ask to it, stores it as the run's lifetime cap and names the pool; an untouched run
// takes the pool's default; preflight, the preview and create refuse No end naming the pool; and the
// end PATCH is held to the cap captured at create.
func TestPG_PoolLifetimeBoundsTheRunEnd(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.lifetimePool("Two hours", `{"default_sec":3600,"max_sec":7200}`)

	noEnd := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`","no_end":true`)
	for _, door := range placementDoors {
		w := e.admin(http.MethodPost, door, noEnd)
		if w.Code != http.StatusForbidden || wireReason(t, w) != reasonRunEndNoEndNotAllowed || !strings.Contains(w.Body.String(), pool.Name) {
			t.Errorf("No end at %s = %d %q, want 403 %s naming the pool", door, w.Code, wireReason(t, w), reasonRunEndNoEndNotAllowed)
		}
	}

	// An ask past the pool's maximum is cut, never refused: the dry doors admit it.
	overAsk := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`+fmt.Sprintf(`,"ends_after_sec":%d`, 7*secDay))
	for _, door := range placementDoors[1:] {
		if w := e.admin(http.MethodPost, door, overAsk); w.Code != http.StatusOK {
			t.Errorf("an over-ask at %s = %d %q, want 200", door, w.Code, wireReason(t, w))
		}
	}
	run, warnings := e.createOn(pool, fmt.Sprintf(`,"ends_after_sec":%d`, 7*secDay))
	if !atOffset(run, run.EndsAt, 2*time.Hour) || !atOffset(run, run.LifetimeCapAt, 2*time.Hour) || run.LifetimeCapSource != types.LimitSourcePool {
		t.Errorf("ends %v, cap %v (%q), created %v; want both at two hours from the pool", run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource, run.CreatedAt)
	}
	if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, pool.Name) }) {
		t.Errorf("warnings %q do not name the pool that cut the end", warnings)
	}
	if untouched, _ := e.createOn(pool, ""); !atOffset(untouched, untouched.EndsAt, time.Hour) {
		t.Errorf("an untouched run ends %v, want the pool's one-hour default", untouched.EndsAt)
	}

	code, resp, raw := e.patchEnd(run, `"`+run.CreatedAt.Add(30*24*time.Hour).UTC().Format(time.RFC3339)+`"`)
	if code != http.StatusOK || !atOffset(run, resp.EndsAt, 2*time.Hour) || !slices.Contains(resp.Capped, "ends_at") || resp.LatestEndSource != types.LimitSourcePool {
		t.Errorf("PATCH past the pool's cap = %d %s; want 200 capped at the captured pool cap", code, raw)
	}
	if code, _, raw := e.patchEnd(run, "null"); code != http.StatusForbidden {
		t.Errorf("PATCH to No end = %d %s, want 403", code, raw)
	}
}

// Governance tighter than the pool binds a member's end at every door, while the pool's maximum is
// still the run's lifetime cap: a seven-day ask under a 30-minute lease and a two-hour pool ends at 30
// minutes, says so in the 201, and is capped at two hours from the pool.
func TestPG_PoolEndUnderTighterGovernance(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.lifetimePool("Two hours", `{"default_sec":3600,"max_sec":7200}`)
	profile, err := e.pg.UpsertGovernanceProfile(context.Background(), types.GovernanceProfile{
		Name: "half hour", CreatedBy: "seed", Ceiling: govTestPolicy,
		Limits: types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 1800, AllowNoEnd: true, UserChangesLimits: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.seedAssignment(profile, e.personP)

	body := endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`+fmt.Sprintf(`,"ends_after_sec":%d`, 7*secDay))
	for _, door := range placementDoors[1:] {
		if w := e.call(e.person, http.MethodPost, door, body); w.Code != http.StatusOK {
			t.Errorf("%s = %d %q, want 200", door, w.Code, wireReason(t, w))
		}
	}
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var resp struct {
		types.AgentRun
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	run, err := e.pg.GetRun(context.Background(), resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !atOffset(run, run.EndsAt, 30*time.Minute) || !atOffset(run, run.LifetimeCapAt, 2*time.Hour) || run.LifetimeCapSource != types.LimitSourcePool {
		t.Errorf("ends %v, cap %v (%q); want 30 minutes from governance and a two-hour cap from the pool", run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource)
	}
	if !slices.ContainsFunc(resp.Warnings, func(w string) bool { return strings.Contains(w, "your admin") }) {
		t.Errorf("warnings %q do not name governance as the limit that cut the end", resp.Warnings)
	}
}

// The end PATCH is held to the cap the run captured at create, whatever happens to the pool after: a
// pool whose lifetime is raised, and then deleted, leaves the run's two-hour cap where it was.
func TestPG_EndPatchKeepsTheCapturedPoolCap(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.lifetimePool("Two hours", `{"default_sec":3600,"max_sec":7200}`)
	run, _ := e.createOn(pool, "")
	far := `"` + run.CreatedAt.Add(30*24*time.Hour).UTC().Format(time.RFC3339) + `"`
	heldToCap := func(when string) {
		t.Helper()
		code, resp, raw := e.patchEnd(run, far)
		if code != http.StatusOK || !atOffset(run, resp.EndsAt, 2*time.Hour) || !slices.Contains(resp.Capped, "ends_at") || resp.LatestEndSource != types.LimitSourcePool {
			t.Errorf("%s: PATCH = %d %s; want 200 capped at the two-hour cap captured at create", when, code, raw)
		}
	}

	raised := `{"background":{"cpu_millis":{"default":500,"cap":4000},"memory_mib":{"default":512,"cap":8192},` +
		`"lifetime":{"default_sec":3600,"max_sec":2592000}},"barriers":["CC1","CC2","CC3"]}`
	if w := e.admin(http.MethodPut, poolPath(pool.ID), fmt.Sprintf(`{"revision":%d,"limits":%s}`, e.revision(pool.ID), raised)); w.Code != http.StatusOK {
		t.Fatalf("raise the pool's lifetime = %d %s", w.Code, w.Body)
	}
	heldToCap("after the pool's lifetime was raised")

	if w := e.admin(http.MethodDelete, poolPath(pool.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d %s", w.Code, w.Body)
	}
	heldToCap("after the pool was deleted")
}

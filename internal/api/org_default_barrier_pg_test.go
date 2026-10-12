// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The organisation's default barrier (DS-5 §5.4) against a real Postgres: guarded by WARDYN_TEST_PG,
// skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setOrgBarrier writes the organisation's default barrier as the super admin.
func (e *placementEnv) setOrgBarrier(class string) {
	e.t.Helper()
	e.orgDefaults(`{"barrier":"` + class + `"}`)
}

// untouchedAnswer is what the three doors and the audit say of an untouched launch (no class named).
type untouchedAnswer struct {
	preflight, created types.ConfinementClass
	source             string
}

func (e *placementEnv) untouched(body string) untouchedAnswer {
	e.t.Helper()
	got := e.answers(e.person, body)
	for door, w := range got {
		if w.Code >= 300 {
			e.t.Fatalf("%s = %d %s", door, w.Code, w.Body)
		}
	}
	var out untouchedAnswer
	var pre struct {
		Enforced types.ConfinementClass `json:"enforced_confinement_class"`
	}
	_ = json.Unmarshal(got[placementDoors[1]].Body.Bytes(), &pre)
	out.preflight = pre.Enforced
	run := decodeJSON[types.AgentRun](e.t, got[placementDoors[0]])
	stored, err := e.pg.GetRun(context.Background(), run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	out.created = stored.ConfinementClass
	rows := e.audits("run.create")
	var data struct {
		Default string `json:"confinement_default"`
	}
	if err := json.Unmarshal(rows[len(rows)-1].Data, &data); err != nil {
		e.t.Fatal(err)
	}
	out.source = data.Default
	return out
}

// listedDefault is the default barrier the pool list shows for pool id, and who chose it.
func (e *placementEnv) listedDefault(pool uuid.UUID) (types.ConfinementClass, runnerpool.BarrierDefaultSource) {
	e.t.Helper()
	for _, o := range e.listed(e.person)[pool].BarrierOptions {
		if o.Default {
			return o.Class, o.DefaultSource
		}
	}
	e.t.Fatalf("pool %s lists no default barrier", pool)
	return "", ""
}

// T15: the organisation's CC2, with CC1-CC3 offered and the floor at CC1, is what an untouched launch
// gets at every door; the pool list and run.create say the organisation chose it; a launch that names a
// class keeps it. The rule has no pool-kind branch, so a Self-Hosted pool takes the same path
// (TestDefaultBarrierRule); a run on a person's own runner is not placeable in this tree.
func TestOrgDefaultBarrierApplies(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.addExecutors(remote.ID, "alpha", "beta")
	e.setOrgBarrier("CC2")

	got := e.untouched(limitsBody(`"runner_pool_id":"` + remote.ID.String() + `"`))
	if got != (untouchedAnswer{preflight: types.CC2, created: types.CC2, source: "organisation"}) {
		t.Errorf("a Remote Provided pool: untouched launch = %+v, want CC2 at both doors, chosen by the organisation", got)
	}
	if class, who := e.listedDefault(remote.ID); class != types.CC2 || who != runnerpool.BarrierDefaultOrganisation {
		t.Errorf("the list defaults to %s from %s, want CC2 from the organisation", class, who)
	}

	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", limitsBody(`"runner_pool_id":"`+remote.ID.String()+`","confinement_class":"CC3"`)); w.Code != http.StatusCreated {
		t.Fatalf("a named class = %d %s", w.Code, w.Body)
	}
	rows := e.audits("run.create")
	var named map[string]any
	_ = json.Unmarshal(rows[len(rows)-1].Data, &named)
	if named["confinement_class"] != "CC3" || named["confinement_source"] != "requested" || named["confinement_default"] != nil {
		t.Errorf("a named class = %v, want CC3 requested with no confinement_default", named)
	}
}

// T16: a default below the caller's floor, or one the pool does not allow, is not used: today's rule
// (the strongest offered at or above the floor) applies and the audit says the server chose.
func TestOrgDefaultBelowFloorFallsBack(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.addExecutors(remote.ID, "alpha", "beta")
	e.setOrgBarrier("CC1")
	floor := limitsBody(`"runner_pool_id":"` + remote.ID.String() + `","inline_policy":{"min_confinement_class":"CC2"}`)
	if got := e.untouched(floor); got != (untouchedAnswer{preflight: types.CC3, created: types.CC3, source: "server"}) {
		t.Errorf("org CC1 under a CC2 floor = %+v, want the strongest, CC3, chosen by the server", got)
	}

	// CC2 is at the floor but the pool allows only CC1 and CC3.
	e.setOrgBarrier("CC2")
	limited := e.limitedPool("No Wall", types.CC1, types.CC3)
	got := e.untouched(limitsBody(`"runner_pool_id":"` + limited.ID.String() + `"`))
	if got != (untouchedAnswer{preflight: types.CC3, created: types.CC3, source: "server"}) {
		t.Errorf("org CC2 on a pool that does not allow it = %+v, want CC3 chosen by the server", got)
	}
	if class, who := e.listedDefault(limited.ID); class != types.CC3 || who != runnerpool.BarrierDefaultServer {
		t.Errorf("the list defaults to %s from %s, want CC3 from the server", class, who)
	}
}

// T17: a person's own defaults refuse a barrier (400 runner_pool_invalid, nothing written); the
// organisation's accepts CC1-CC3 and none, and the column refuses anything else.
func TestPersonalDefaultsRefuseBarrier(t *testing.T) {
	e := newPlacementEnv(t)
	w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", `{"barrier":"CC2"}`)
	if w.Code != http.StatusBadRequest || wireReason(t, w) != string(runnerpool.ReasonInvalid) {
		t.Fatalf("a personal barrier = %d %s, want 400 runner_pool_invalid", w.Code, w.Body)
	}
	if got := decodeJSON[types.RunnerPoolDefaults](t, e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", "")); got.Barrier != "" {
		t.Errorf("a refused barrier was stored: %+v", got)
	}

	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"barrier":"CC9"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an organisation barrier CC9 = %d %s, want 400", w.Code, w.Body)
	}
	e.setOrgBarrier("CC3")
	if got := decodeJSON[types.RunnerPoolDefaults](t, e.admin(http.MethodGet, "/api/v1/runner-pool-defaults", "")); got.Barrier != types.CC3 {
		t.Errorf("the organisation's barrier reads %q, want CC3", got.Barrier)
	}
	rows := e.audits("runner_pool.default.set")
	var set struct {
		After types.RunnerPoolDefaults `json:"after"`
	}
	if err := json.Unmarshal(rows[len(rows)-1].Data, &set); err != nil || set.After.Barrier != types.CC3 {
		t.Errorf("runner_pool.default.set after = %+v (%v), want the barrier CC3", set.After, err)
	}
	e.orgDefaults(`{}`)
	if got := decodeJSON[types.RunnerPoolDefaults](t, e.admin(http.MethodGet, "/api/v1/runner-pool-defaults", "")); got.Barrier != "" {
		t.Errorf("a replaced document kept the barrier %q", got.Barrier)
	}

	// The column itself refuses a class that is not one.
	if _, err := e.pg.Pool.Exec(context.Background(), `UPDATE runner_pool_org_defaults SET barrier = 'CC9'`); err == nil {
		t.Error("the barrier column accepted CC9")
	}
}

// T18: the organisation's default is for a person's launch. A server-originated launch (here the source
// scan) keeps the strongest class the runner offers, whatever the organisation set.
func TestSourceScanKeepsStrongest(t *testing.T) {
	srv, _ := pgHarnessWithRunner(t, &fakeRunner{})
	if w := do(t, srv, http.MethodPut, "/api/v1/runner-pool-defaults", adminToken, `{"barrier":"CC2"}`); w.Code != http.StatusOK {
		t.Fatalf("set the organisation barrier = %d %s", w.Code, w.Body)
	}
	suffix := uuid.NewString()[:8]
	body := fmt.Sprintf(`{"name":"scan-%s","sources":[{"type":"repo","source":"acme/widgets-%s"}]}`, suffix, suffix)
	w := do(t, srv, http.MethodPost, "/api/v1/workspaces", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create workspace = %d %s", w.Code, w.Body)
	}
	ws := decodeJSON[types.Workspace](t, w)
	w = do(t, srv, http.MethodPost, "/api/v1/workspaces/"+ws.ID.String()+"/scan", adminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("scan = %d %s", w.Code, w.Body)
	}
	ids := decodeJSON[struct {
		ScanRunIDs []uuid.UUID `json:"scan_run_ids"`
	}](t, w).ScanRunIDs
	if len(ids) != 1 {
		t.Fatalf("scan runs = %v, want one", ids)
	}
	run := decodeJSON[types.AgentRun](t, do(t, srv, http.MethodGet, "/api/v1/runs/"+ids[0].String(), adminToken, ""))
	if run.ConfinementClass != types.CC3 {
		t.Errorf("the scan run enforces %s with the organisation default CC2, want the strongest offered, CC3", run.ConfinementClass)
	}
}

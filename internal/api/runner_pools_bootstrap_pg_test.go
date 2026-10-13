// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bootDoors posts body to create, preflight and the policy preview as c and fails unless each admits it.
func (e *poolEnv) bootDoors(name, body string) {
	e.t.Helper()
	want := map[string]int{"/api/v1/runs": http.StatusCreated, "/api/v1/runs/preflight": http.StatusOK, "/api/v1/runs/policy-preview": http.StatusOK}
	for _, door := range placementDoors {
		if w := e.call(e.person, http.MethodPost, door, body); w.Code != want[door] {
			e.t.Errorf("%s: %s = %d %s, want %d", name, door, w.Code, w.Body, want[door])
		}
	}
}

// After the real boot step on a database with no catalogue, a launch that names its placement is
// admitted at every door, and an administrator's own defaults survive a later boot as written.
func TestPG_BootstrapKeepsExplicitLaunchesAndAdministratorDefaults(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	e.bootDoors("placement named", runBody(`"placement":"remote"`))

	d, err := e.pg.GetRunnerPoolOrgDefaults(ctx)
	if err != nil || d.PoolID == nil {
		t.Fatalf("defaults after bootstrap = %+v, %v", d, err)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"pool_id":"`+d.PoolID.String()+`","barrier":"CC2"}`); w.Code != http.StatusOK {
		t.Fatalf("administrator defaults = %d %s", w.Code, w.Body)
	}
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.pg.GetRunnerPoolOrgDefaults(ctx); got.Barrier != types.CC2 || got.PoolID == nil || *got.PoolID != *d.PoolID {
		t.Errorf("a boot rewrote the administrator's defaults: %+v", got)
	}

	// A default barrier the organisation set before any pool existed is kept by the bootstrap.
	chosen := newPoolEnv(t, false)
	if w := chosen.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"barrier":"CC1"}`); w.Code != http.StatusOK {
		t.Fatalf("administrator barrier = %d %s", w.Code, w.Body)
	}
	if err := chosen.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := chosen.pg.GetRunnerPoolOrgDefaults(ctx); got.Barrier != types.CC1 || got.PoolID == nil {
		t.Errorf("bootstrap over an organisation's own barrier = %+v, want the barrier kept and the default pool set", got)
	}
}

// R2-0153: the real boot step on a database with no catalogue gives each configured remote-hosted runner
// its row, makes one organization pool named Organization holding them as the organisation's default
// pool, and audits all three, so a launch that names no pool and no placement is admitted at create,
// preflight and the preview. A second boot makes nothing and audits nothing.
func TestPG_BootstrapLetsALaunchNameNoPool(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DELETE FROM remote_runners`); err != nil { // a database from before 0173 ran a boot
		t.Fatal(err)
	}
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := e.pg.GetRunnerPoolOrgDefaults(ctx)
	if d.PoolID == nil {
		t.Fatalf("defaults after bootstrap = %+v, want the pool as the default", d)
	}
	p, err := e.pg.GetRunnerPool(ctx, *d.PoolID)
	if err != nil || p.Name != "Organization" || p.Scope != types.RunnerPoolOrganization {
		t.Errorf("bootstrap pool = %+v, %v", p, err)
	}
	if rr, err := e.pg.GetRemoteRunner(ctx, "fake"); err != nil || rr.Runner.PeoplePools != types.PeoplePoolsOff || len(rr.Pools) != 1 || rr.Pools[0].ID != p.ID {
		t.Errorf("the configured runner's row = %+v, %v; want it off for people's pools and in the pool", rr, err)
	}
	sets := e.audits(auditPoolDefaultSet)
	if len(sets) != 1 {
		t.Fatalf("bootstrap recorded %d default.set rows, want 1", len(sets))
	}
	if after, _ := auditData(t, sets[0])["after"].(map[string]any); after["pool_id"] != d.PoolID.String() {
		t.Errorf("bootstrap default.set after = %v, want pool_id %s", after, d.PoolID)
	}
	if made := e.audits(auditRemoteRunnerCreate); len(made) != 1 || made[0].Target != "fake" {
		t.Errorf("remote_runner.create rows = %+v, want one for the configured runner", made)
	}
	e.bootDoors("no pool or placement named", runBody(""))
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.audits(auditRemoteRunnerCreate)) != 1 || len(e.audits(auditPoolCreate)) != 1 {
		t.Error("a second boot audited a create")
	}
}

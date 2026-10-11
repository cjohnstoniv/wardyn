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
	if err != nil || d.RemoteProvided == nil {
		t.Fatalf("defaults after bootstrap = %+v, %v", d, err)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"remote_provided":"`+d.RemoteProvided.String()+`"}`); w.Code != http.StatusOK {
		t.Fatalf("administrator defaults = %d %s", w.Code, w.Body)
	}
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.pg.GetRunnerPoolOrgDefaults(ctx); got.PreferredHosting != "" || got.RemoteProvided == nil || *got.RemoteProvided != *d.RemoteProvided {
		t.Errorf("a boot rewrote the administrator's defaults: %+v", got)
	}

	// A preferred hosting type the organisation set before any pool existed is kept by the bootstrap.
	chosen := newPoolEnv(t, false)
	if _, err := chosen.pool.Exec(ctx, `INSERT INTO runner_pool_org_defaults (preferred_hosting, updated_by) VALUES ('self_hosted', 'admin')`); err != nil {
		t.Fatal(err)
	}
	if err := chosen.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := chosen.pg.GetRunnerPoolOrgDefaults(ctx); got.PreferredHosting != types.RunnerPoolSelfHosted || got.RemoteProvided == nil {
		t.Errorf("bootstrap over an organisation's own preference = %+v, want self_hosted kept and the remote default set", got)
	}
}

// R2-0153: the real boot step on a database with no catalogue makes the pool the organisation's
// preferred hosting type as well as its remote default, and audits both, so a launch that names no pool
// and no placement is admitted at create, preflight and the preview.
func TestPG_BootstrapLetsALaunchNameNoPool(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	d, _ := e.pg.GetRunnerPoolOrgDefaults(ctx)
	if d.PreferredHosting != types.RunnerPoolRemoteProvided || d.RemoteProvided == nil {
		t.Errorf("defaults after bootstrap = %+v, want the pool as remote default and remote_provided preferred", d)
	}
	sets := e.audits(auditPoolDefaultSet)
	if len(sets) != 1 {
		t.Fatalf("bootstrap recorded %d default.set rows, want 1", len(sets))
	}
	after, _ := auditData(t, sets[0])["after"].(map[string]any)
	if after["preferred_hosting"] != string(types.RunnerPoolRemoteProvided) {
		t.Errorf("bootstrap default.set after = %v, want preferred_hosting remote_provided", after)
	}
	e.bootDoors("no pool or placement named", runBody(""))
}

// DR-0035: defaults an administrator saved with no preferred hosting type before any pool existed keep
// it blank through the first bootstrap, which still names its pool as the remote default and audits the
// defaults as stored.
func TestPG_BootstrapKeepsABlankPreferenceSavedBeforeAnyPool(t *testing.T) {
	ctx := context.Background()
	blank := newPoolEnv(t, false)
	if w := blank.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{}`); w.Code != http.StatusOK {
		t.Fatalf("administrator blank defaults = %d %s", w.Code, w.Body)
	}
	if err := blank.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := blank.pg.GetRunnerPoolOrgDefaults(ctx)
	if got.PreferredHosting != "" || got.RemoteProvided == nil {
		t.Errorf("bootstrap over an administrator's blank preference = %+v, want it blank and the remote default set", got)
	}
	sets := blank.audits(auditPoolDefaultSet)
	if after, _ := auditData(t, sets[len(sets)-1])["after"].(map[string]any); after["preferred_hosting"] != nil || after["remote_provided"] != got.RemoteProvided.String() {
		t.Errorf("bootstrap default.set after = %v, want the stored defaults %+v", after, got)
	}
}

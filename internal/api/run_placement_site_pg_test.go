// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Sites against a real Postgres: a queued run keeps the pool, selection and site it was admitted with,
// whatever happens to the switch while it waits, and a site default with no runner for the run is refused
// through placement as that pool chosen as the person's default is. Guarded by WARDYN_TEST_PG: skipped
// cleanly when unset, must PASS when set. None of these runs in parallel: each registers the switch.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sitePool is an organization pool on executor that runs at most max runs at once.
func (e *placementEnv) sitePool(name, executor string, max int) types.RunnerPool {
	e.t.Helper()
	body := fmt.Sprintf(`{"name":%q,"limits":{"background":{"cpu_millis":{"default":500,"cap":4000},`+
		`"memory_mib":{"default":512,"cap":8192},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"],"max_concurrent_runs":%d}}`, name, max)
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create pool = %d %s", w.Code, w.Body)
	}
	p := decodeJSON[types.RunnerPool](e.t, w)
	e.addExecutors(p.ID, executor)
	return p
}

// orgSiteAt stores a live site whose one pool and default pool is pool, and an admin drive homed there,
// allocated to the person.
func (e *placementEnv) orgSiteAt(name string, pool types.RunnerPool) (types.OrgSite, types.UserDrive) {
	e.t.Helper()
	ctx := context.Background()
	drive, err := e.pg.UpsertUserDrive(ctx, *driveFixture(func(d *types.UserDrive) { d.Name, d.CreatedBy = "Office share", "admin@example.com" }), false)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pg.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = e.personP }), true); err != nil {
		e.t.Fatal(err)
	}
	site, err := e.pg.CreateOrgSite(ctx, types.OrgSite{Name: name, State: types.OrgSiteActive})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := e.pg.AssignPoolSite(ctx, pool.ID, site.ID); err != nil {
		e.t.Fatal(err)
	}
	if site, err = e.pg.UpdateOrgSite(ctx, site.ID, site.Revision, site.Name, "", "", &pool.ID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pg.SetDriveSite(ctx, drive.ID, site.ID); err != nil {
		e.t.Fatal(err)
	}
	return site, drive
}

// storeSites registers the switch and stores it on or off beside the runners switch, as an admin's write
// of the features block leaves it.
func (e *placementEnv) storeSites(on bool) {
	e.t.Helper()
	sc := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true},
		FeatureSwitches: &types.SiteFeatureSwitches{OrgSites: &types.OrgSitesFeature{Enabled: &on}}}
	if _, err := e.pg.PutSiteConfig(context.Background(), sc); err != nil {
		e.t.Fatal(err)
	}
}

// driveLaunch is the person's Background run that mounts drive and names no pool.
func (e *queueEnv) driveLaunch(drive types.UserDrive) queuedLaunchAnswer {
	e.t.Helper()
	body := strings.Replace(endBody("background", ""), `"no_repositories_or_drives":true`, fmt.Sprintf(`"drives":[{"drive_id":%q}]`, drive.ID), 1)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("launch with the homed drive = %d %s", w.Code, w.Body)
	}
	return decodeJSON[queuedLaunchAnswer](e.t, w)
}

// siteQueueSetup is a site whose default pool Office runs one run at a time and is full, the
// organisation's default Shared with room, and the person's drive homed at the site, with sites on: the
// drive's run waits in Office, chosen for its site. It answers the waiting run and the occupant to free.
func siteQueueSetup(t *testing.T, e *queueEnv) (types.OrgSite, types.RunnerPool, types.RunnerPool, types.UserDrive, queuedLaunchAnswer, types.AgentRun) {
	t.Helper()
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	e.srv.cfg.RunnerTarget = "docker"
	office, shared := e.sitePool("Office", "alpha", 1), e.sitePool("Shared", "beta", 5)
	e.orgDefaults(`{"pool_id":"` + shared.ID.String() + `"}`)
	site, drive := e.orgSiteAt("Leeds office", office)
	e.storeSites(true)
	occupant := e.occupy(office)
	waiting := e.driveLaunch(drive)
	if waiting.State != types.RunQueued {
		t.Fatalf("the drive's run is %s; want it QUEUED in its site's full default pool", waiting.State)
	}
	return site, e.reread(office), shared, drive, waiting, occupant
}

// assertAdmittedAtSite: the run is in Office, chosen as the site's default, at the site.
func assertAdmittedAtSite(t *testing.T, what string, r types.AgentRun, office types.RunnerPool, site types.OrgSite) {
	t.Helper()
	if r.RunnerPoolID == nil || *r.RunnerPoolID != office.ID || r.RunnerPoolSelection != types.RunnerPoolSelectedSite || r.SiteID == nil || *r.SiteID != site.ID {
		t.Errorf("%s: pool %v via %s at %v; want Office via site_default at %s", what, r.RunnerPoolID, r.RunnerPoolSelection, r.SiteID, site.ID)
	}
}

// A run admitted at its drive's site waits in that site's default pool and starts there with the pool,
// selection and site it was admitted with; run.create records the site and that the preference was met.
func TestPG_QueuedRunPromotedKeepsSiteDefault(t *testing.T) {
	e := newQueueEnv(t)
	site, office, _, _, waiting, occupant := siteQueueSetup(t, e)
	assertAdmittedAtSite(t, "queued", e.run(waiting.ID), office, site)
	rows := e.audits(waiting.ID, "run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create rows = %d, want 1", len(rows))
	}
	data := queueAuditData(t, rows[0])
	if data["site_id"] != site.ID.String() || data["site_name"] != site.Name || data["site_preference"] != string(runnerpool.SiteMet) {
		t.Errorf("run.create data = %v; want the site and site_preference met", data)
	}

	e.free(occupant)
	assertAdmittedAtSite(t, "started", e.waitStarted(waiting.ID), office, site)
}

// A queued run keeps what it was admitted with when sites are switched off while it waits: it starts in
// the same pool, still site_default, still at its site. A run launched after the switch-off gets no site
// step: the organisation's default, no site, and no site keys in run.create.
func TestPG_QueuedRunPromotedAfterSitesOff(t *testing.T) {
	e := newQueueEnv(t)
	site, office, shared, drive, waiting, occupant := siteQueueSetup(t, e)
	e.storeSites(false)

	e.free(occupant)
	assertAdmittedAtSite(t, "started after the switch-off", e.waitStarted(waiting.ID), office, site)

	later := e.run(e.driveLaunch(drive).ID)
	if later.RunnerPoolID == nil || *later.RunnerPoolID != shared.ID || later.RunnerPoolSelection != types.RunnerPoolSelectedOrg || later.SiteID != nil {
		t.Errorf("a launch after the switch-off: pool %v via %s at %v; want Shared via organisation_default at no site",
			later.RunnerPoolID, later.RunnerPoolSelection, later.SiteID)
	}
	rows := e.audits(later.ID, "run.create")
	if len(rows) != 1 {
		t.Fatalf("run.create rows = %d, want 1", len(rows))
	}
	for k := range queueAuditData(t, rows[0]) {
		if strings.HasPrefix(k, "site_") {
			t.Errorf("run.create of a run launched with sites off carries %q", k)
		}
	}
}

// offlineExecutors is the deployment's runner with the named executors configured but not reporting, as
// an executor that is down answers: unknown facts and no probe error.
type offlineExecutors struct {
	*poolRunner
	offline map[string]bool
}

func (o offlineExecutors) ExecutorFacts(ctx context.Context, name string) (runner.TargetFacts, bool) {
	if o.offline[name] {
		return runner.TargetFacts{}, false
	}
	return o.poolRunner.ExecutorFacts(ctx, name)
}

// A site default whose only runner is offline is refused through placement with the status and reason
// the same pool gets as the person's own default, and the sentence names the site and says how to run
// elsewhere. The run never falls through to the organisation's default, which has a runner online.
func TestPG_SiteDefaultOfflineTargetsRefusedThroughPlacement(t *testing.T) {
	e := newPlacementEnv(t)
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	e.srv.cfg.Runner = offlineExecutors{poolRunner: e.runner, offline: map[string]bool{"alpha": true}}
	office, shared := e.sitePool("Office", "alpha", 5), e.sitePool("Shared", "beta", 5)
	e.orgDefaults(`{"pool_id":"` + shared.ID.String() + `"}`)
	site, drive := e.orgSiteAt("Leeds office", office)

	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	place := func() (runPlacement, *runRefusal) {
		t.Helper()
		req := createRunRequest{Agent: "claude-code", Experience: "background"}
		return e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling, drives: true, driveIDs: []uuid.UUID{drive.ID}})
	}

	e.storeSites(true)
	plan, atSite := place()
	if atSite == nil {
		t.Fatalf("placed in %s via %s; want Office refused, never the organisation's default", plan.pool.Name, plan.pool.Selection)
	}

	e.storeSites(false)
	e.personDefaults(e.person, `{"pool_id":"`+office.ID.String()+`"}`)
	_, asPersonal := place()
	if asPersonal == nil {
		t.Fatal("Office as the person's default was placed; its only runner is offline")
	}
	if atSite.err != nil || atSite.status != asPersonal.status || atSite.body.Reason != asPersonal.body.Reason {
		t.Errorf("site default refused %d %s (%v), the person's default %d %s", atSite.status, atSite.body.Reason, atSite.err, asPersonal.status, asPersonal.body.Reason)
	}
	if !strings.HasPrefix(atSite.body.Error, asPersonal.body.Error+" ") || !strings.Contains(atSite.body.Error, site.Name) ||
		!strings.Contains(atSite.body.Error, "Name a pool to run elsewhere.") {
		t.Errorf("site default sentence %q; want the personal default's %q, then one naming %s", atSite.body.Error, asPersonal.body.Error, site.Name)
	}
	if strings.Contains(asPersonal.body.Error, "Name a pool to run elsewhere.") {
		t.Errorf("the person's default's refusal carries the site sentence: %q", asPersonal.body.Error)
	}
}

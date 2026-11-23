// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The site routes against a real Postgres: an admin defines a site, puts an organization pool at it, makes it
// the default and homes an admin drive there, and every change is audited without the storage link; a member
// reading the site sees only the pools they may use. Guarded by WARDYN_TEST_PG: skipped cleanly when unset,
// must PASS when set. Neither runs in parallel: each registers the switch.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sitesOnPG registers the switch and stores it on.
func (e *poolEnv) sitesOnPG() {
	e.t.Helper()
	withSiteFeatureHooks(e.t, availableSiteFeatures(featureOrgSites))
	on := true
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{
		FeatureSwitches: &types.SiteFeatureSwitches{OrgSites: &types.OrgSitesFeature{Enabled: &on}}}); err != nil {
		e.t.Fatal(err)
	}
}

func TestPG_OrgSiteRoutesWriteThroughTheStore(t *testing.T) {
	e := newPoolEnv(t, false)
	e.sitesOnPG()
	const link = "https://files.example.com/share"
	w := e.admin(http.MethodPost, "/api/v1/org-sites", fmt.Sprintf(`{"name":"Office","storage_label":"Office NAS","storage_url":%q}`, link))
	if w.Code != http.StatusCreated {
		t.Fatalf("create site = %d %s", w.Code, w.Body)
	}
	office := decodeJSON[types.OrgSite](t, w)
	wantReason(t, e.admin(http.MethodPost, "/api/v1/org-sites", `{"name":"Office"}`), http.StatusConflict, reasonOrgSiteNameTaken)

	pool := e.createPool("Office cluster", types.RunnerPoolOrganization)
	own := e.createPool("Mine", types.RunnerPoolUser)
	onOffice := fmt.Sprintf(`{"site_id":%q}`, office.ID)
	w = e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String()+"/site", onOffice)
	if w.Code != http.StatusOK {
		t.Fatalf("assign pool = %d %s", w.Code, w.Body)
	}
	if got := decodeJSON[poolSiteAnswer](t, w); got.Revision <= pool.Revision {
		t.Errorf("assign answered revision %d, want above the pool's %d: a move fences earlier admissions", got.Revision, pool.Revision)
	}
	wantReason(t, e.admin(http.MethodPut, "/api/v1/runner-pools/"+own.ID.String()+"/site", onOffice), http.StatusUnprocessableEntity, reasonOrgSitePoolScope)

	wantReason(t, e.admin(http.MethodPut, "/api/v1/org-sites/"+office.ID.String(), `{"revision":99,"name":"Office"}`), http.StatusConflict, reasonOrgSiteStale)
	wantReason(t, e.admin(http.MethodPut, "/api/v1/org-sites/"+office.ID.String(),
		fmt.Sprintf(`{"revision":%d,"name":"Office","default_pool_id":%q}`, office.Revision, own.ID)), http.StatusUnprocessableEntity, reasonOrgSiteDefaultPool)
	w = e.admin(http.MethodPut, "/api/v1/org-sites/"+office.ID.String(),
		fmt.Sprintf(`{"revision":%d,"name":"Office","storage_label":"Office NAS","storage_url":%q,"default_pool_id":%q}`, office.Revision, link, pool.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("set the default pool = %d %s", w.Code, w.Body)
	}
	if got := decodeJSON[types.OrgSite](t, w); got.DefaultPoolID == nil || *got.DefaultPoolID != pool.ID {
		t.Fatalf("default pool = %v, want %s", got.DefaultPoolID, pool.ID)
	}

	ctx := context.Background()
	drive, err := e.pg.UpsertUserDrive(ctx, *driveFixture(func(d *types.UserDrive) { d.Name, d.CreatedBy = "designs", "admin@example.com" }), false)
	if err != nil {
		t.Fatal(err)
	}
	if w := e.admin(http.MethodPut, "/api/v1/drives/"+drive.ID.String()+"/site", onOffice); w.Code != http.StatusOK {
		t.Fatalf("home the drive = %d %s", w.Code, w.Body)
	}
	listed := decodeJSON[[]types.OrgSiteDrive](t, e.admin(http.MethodGet, "/api/v1/org-sites/drives", ""))
	if !slices.ContainsFunc(listed, func(d types.OrgSiteDrive) bool { return d.DriveID == drive.ID && d.SiteID == office.ID }) {
		t.Errorf("GET /org-sites/drives = %+v, want designs at Office", listed)
	}
	if homes, err := e.pg.DriveHomeSites(ctx, []uuid.UUID{drive.ID}); err != nil || homes[drive.ID] != office.ID {
		t.Errorf("drive home = %v (%v), want Office", homes, err)
	}

	wantReason(t, e.admin(http.MethodDelete, "/api/v1/org-sites/"+office.ID.String(), ""), http.StatusConflict, reasonOrgSiteInUse)
	for _, path := range []string{"/api/v1/drives/" + drive.ID.String() + "/site", "/api/v1/runner-pools/" + pool.ID.String() + "/site"} {
		if w := e.admin(http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d %s", path, w.Code, w.Body)
		}
	}
	after := decodeJSON[types.OrgSite](t, e.admin(http.MethodGet, "/api/v1/org-sites/"+office.ID.String(), ""))
	if after.DefaultPoolID != nil || len(after.PoolIDs) != 0 {
		t.Errorf("after the pool left: default %v, pools %v, want none", after.DefaultPoolID, after.PoolIDs)
	}
	if w := e.admin(http.MethodDelete, "/api/v1/org-sites/"+office.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete the empty site = %d %s", w.Code, w.Body)
	}
	if live := decodeJSON[[]types.OrgSite](t, e.admin(http.MethodGet, "/api/v1/org-sites", "")); slices.ContainsFunc(live, func(s types.OrgSite) bool { return s.ID == office.ID }) {
		t.Errorf("a deleted site is still listed: %+v", live)
	}

	actions := map[string]int{}
	for _, action := range []string{auditOrgSiteCreate, auditOrgSiteUpdate, auditOrgSiteDelete, auditOrgSitePoolAssign,
		auditOrgSitePoolRemove, auditOrgSiteDriveSet, auditOrgSiteDriveRemove} {
		for _, d := range e.auditData(action, office.ID.String()) {
			actions[action]++
			for k, v := range d {
				if strings.Contains(k, "_url") || strings.Contains(fmt.Sprint(v), "files.example.com") {
					t.Errorf("%s carries the storage link in %q", action, k)
				}
			}
		}
	}
	for action, n := range actions {
		if n != 1 {
			t.Errorf("%s rows = %d, want 1", action, n)
		}
	}
	if len(actions) != 7 {
		t.Errorf("site audit actions seen = %v, want all 7 once", actions)
	}
}

func TestPG_OrgSiteMemberReadShowsOnlyUsablePools(t *testing.T) {
	e := newPoolEnv(t, false)
	e.sitesOnPG()
	open, narrowed := e.createPool("Open cluster", types.RunnerPoolOrganization), e.createPool("Narrowed cluster", types.RunnerPoolOrganization)
	w := e.admin(http.MethodPost, "/api/v1/org-sites", `{"name":"Office"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create site = %d %s", w.Code, w.Body)
	}
	office := decodeJSON[types.OrgSite](t, w)
	for _, p := range []types.RunnerPool{open, narrowed} {
		if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+p.ID.String()+"/site", fmt.Sprintf(`{"site_id":%q}`, office.ID)); w.Code != http.StatusOK {
			t.Fatalf("assign %s = %d %s", p.Name, w.Code, w.Body)
		}
	}
	w = e.admin(http.MethodPut, "/api/v1/org-sites/"+office.ID.String(), fmt.Sprintf(`{"revision":%d,"name":"Office","default_pool_id":%q}`, office.Revision, narrowed.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("set the default = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, "/api/v1/runner-pools/"+narrowed.ID.String()+"/use-policy",
		`{"subjects":[{"subject_type":"user","subject":"someone-else@corp.example"}]}`); w.Code != http.StatusOK {
		t.Fatalf("narrow the pool = %d %s", w.Code, w.Body)
	}
	member := decodeJSON[types.OrgSite](t, e.call(e.person, http.MethodGet, "/api/v1/org-sites/"+office.ID.String(), ""))
	if !slices.Equal(member.PoolIDs, []uuid.UUID{open.ID}) || member.DefaultPoolID != nil {
		t.Errorf("a member reads pools %v, default %v; want only %s and no default: a site never names a pool its reader may not use",
			member.PoolIDs, member.DefaultPoolID, open.ID)
	}
	listed := decodeJSON[[]types.OrgSite](t, e.call(e.person, http.MethodGet, "/api/v1/org-sites", ""))
	if len(listed) != 1 || slices.Contains(listed[0].PoolIDs, narrowed.ID) {
		t.Errorf("a member lists %+v, want Office without the narrowed pool", listed)
	}
	admin := decodeJSON[types.OrgSite](t, e.admin(http.MethodGet, "/api/v1/org-sites/"+office.ID.String(), ""))
	if len(admin.PoolIDs) != 2 || admin.DefaultPoolID == nil {
		t.Errorf("an admin reads pools %v, default %v; want both and the default", admin.PoolIDs, admin.DefaultPoolID)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The site routes against an in-memory store: the switch refuses every write that adds and lets removals
// through, every write that changes something writes one audit row, and no row carries the storage link.
// None of these runs in parallel: each swaps the registered switches.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// orgSiteRouteStore keeps sites, pool sites and drive homes in memory, with the store's rules: a user pool
// or a member drive is refused, a site in use is not deleted, a pool's move raises its revision, and an update
// that changes nothing keeps the revision. fail[method] makes that write answer the error instead; scErr makes
// the site config unreadable; an uncatalogued pool is live but outside the caller's catalogue.
type orgSiteRouteStore struct {
	store.Store
	store.RunnerPoolStore
	store.OrgSiteStore
	sc     types.SiteConfig
	sites  map[uuid.UUID]types.OrgSite
	pools  map[uuid.UUID]types.RunnerPool
	poolAt map[uuid.UUID]uuid.UUID
	drives map[uuid.UUID]types.UserDrive
	homes  map[uuid.UUID]uuid.UUID
	writes int

	fail         map[string]error
	scErr        error
	uncatalogued map[uuid.UUID]bool
}

var (
	_ store.RunnerPoolStore = (*orgSiteRouteStore)(nil)
	_ store.OrgSiteStore    = (*orgSiteRouteStore)(nil)
)

func (s *orgSiteRouteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, s.scErr
}

// ListRunnerPoolCatalogue is every live pool but the uncatalogued, as the oversight list an admin gets.
func (s *orgSiteRouteStore) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	var rows []store.RunnerPoolCatalogueRow
	for _, p := range s.pools {
		if p.State != types.RunnerPoolDeleted && !s.uncatalogued[p.ID] {
			rows = append(rows, store.RunnerPoolCatalogueRow{Pool: p})
		}
	}
	return rows, nil
}

func (s *orgSiteRouteStore) ListOrgSites(ctx context.Context) ([]types.OrgSite, error) {
	var out []types.OrgSite
	for id, site := range s.sites {
		if site.State == types.OrgSiteActive {
			site, _ = s.GetOrgSite(ctx, id)
			out = append(out, site)
		}
	}
	slices.SortFunc(out, func(a, b types.OrgSite) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *orgSiteRouteStore) ListOrgSiteDrives(context.Context) ([]types.OrgSiteDrive, error) {
	var out []types.OrgSiteDrive
	for d, at := range s.homes {
		if site := s.sites[at]; site.State == types.OrgSiteActive {
			out = append(out, types.OrgSiteDrive{DriveID: d, DriveName: s.drives[d].Name, SiteID: at, SiteName: site.Name})
		}
	}
	return out, nil
}

func (s *orgSiteRouteStore) GetRunnerPool(_ context.Context, id uuid.UUID) (types.RunnerPool, error) {
	p, ok := s.pools[id]
	if !ok {
		return types.RunnerPool{}, store.ErrNotFound
	}
	return p, nil
}

func (s *orgSiteRouteStore) GetUserDrive(_ context.Context, id uuid.UUID) (types.UserDrive, error) {
	d, ok := s.drives[id]
	if !ok {
		return types.UserDrive{}, store.ErrNotFound
	}
	return d, nil
}

func (s *orgSiteRouteStore) GetOrgSite(_ context.Context, id uuid.UUID) (types.OrgSite, error) {
	site, ok := s.sites[id]
	if !ok {
		return types.OrgSite{}, store.ErrNotFound
	}
	site.PoolIDs = []uuid.UUID{}
	for p, at := range s.poolAt {
		if at == id {
			site.PoolIDs = append(site.PoolIDs, p)
		}
	}
	return site, nil
}

func (s *orgSiteRouteStore) CreateOrgSite(_ context.Context, site types.OrgSite) (types.OrgSite, error) {
	if err := s.fail["CreateOrgSite"]; err != nil {
		return types.OrgSite{}, err
	}
	for _, o := range s.sites {
		if o.State == types.OrgSiteActive && o.Name == site.Name {
			return types.OrgSite{}, store.ErrConflict
		}
	}
	s.writes++
	site.State, site.Revision = types.OrgSiteActive, 1
	s.sites[site.ID] = site
	return s.GetOrgSite(context.Background(), site.ID)
}

func (s *orgSiteRouteStore) UpdateOrgSite(ctx context.Context, id uuid.UUID, revision int64, name, label, link string, def *uuid.UUID) (types.OrgSite, error) {
	if err := s.fail["UpdateOrgSite"]; err != nil {
		return types.OrgSite{}, err
	}
	cur, err := s.GetOrgSite(ctx, id)
	switch {
	case err != nil || cur.State != types.OrgSiteActive:
		return types.OrgSite{}, store.ErrNotFound
	case cur.Revision != revision:
		return types.OrgSite{}, store.ErrOrgSiteStale
	case def != nil && s.poolAt[*def] != id:
		return types.OrgSite{}, store.ErrOrgSiteDefaultPool
	case cur.Name == name && cur.StorageLabel == label && cur.StorageURL == link && uuidPtrEqual(cur.DefaultPoolID, def):
		return cur, nil
	}
	s.writes++
	cur.Name, cur.StorageLabel, cur.StorageURL, cur.DefaultPoolID, cur.Revision = name, label, link, def, cur.Revision+1
	s.sites[id] = cur
	return s.GetOrgSite(ctx, id)
}

func (s *orgSiteRouteStore) DeleteOrgSite(ctx context.Context, id uuid.UUID) (types.OrgSite, error) {
	if err := s.fail["DeleteOrgSite"]; err != nil {
		return types.OrgSite{}, err
	}
	cur, err := s.GetOrgSite(ctx, id)
	if err != nil || cur.State != types.OrgSiteActive {
		return types.OrgSite{}, store.ErrNotFound
	}
	for _, at := range s.homes {
		if at == id {
			return types.OrgSite{}, store.ErrOrgSiteInUse
		}
	}
	if len(cur.PoolIDs) > 0 {
		return types.OrgSite{}, store.ErrOrgSiteInUse
	}
	s.writes++
	cur.State, cur.DefaultPoolID, cur.Revision = types.OrgSiteDeleted, nil, cur.Revision+1
	s.sites[id] = cur
	return cur, nil
}

func (s *orgSiteRouteStore) AssignPoolSite(_ context.Context, poolID, siteID uuid.UUID) (*uuid.UUID, int64, error) {
	if err := s.fail["AssignPoolSite"]; err != nil {
		return nil, 0, err
	}
	p := s.pools[poolID]
	if p.Scope != types.RunnerPoolOrganization {
		return nil, 0, store.ErrRunnerPoolScope
	}
	from, at := s.poolAt[poolID]
	if at && from == siteID {
		return &from, p.Revision, nil
	}
	s.writes++
	if at {
		s.leave(poolID, from)
	}
	s.poolAt[poolID] = siteID
	p.Revision++
	s.pools[poolID] = p
	if at {
		return &from, p.Revision, nil
	}
	return nil, p.Revision, nil
}

func (s *orgSiteRouteStore) leave(poolID, siteID uuid.UUID) {
	delete(s.poolAt, poolID)
	if site := s.sites[siteID]; site.DefaultPoolID != nil && *site.DefaultPoolID == poolID {
		site.DefaultPoolID, site.Revision = nil, site.Revision+1
		s.sites[siteID] = site
	}
}

func (s *orgSiteRouteStore) ClearPoolSite(_ context.Context, poolID uuid.UUID) (*uuid.UUID, int64, error) {
	if err := s.fail["ClearPoolSite"]; err != nil {
		return nil, 0, err
	}
	p := s.pools[poolID]
	from, at := s.poolAt[poolID]
	if !at {
		return nil, p.Revision, nil
	}
	s.writes++
	s.leave(poolID, from)
	p.Revision++
	s.pools[poolID] = p
	return &from, p.Revision, nil
}

func (s *orgSiteRouteStore) SetDriveSite(_ context.Context, driveID, siteID uuid.UUID) (*uuid.UUID, error) {
	if err := s.fail["SetDriveSite"]; err != nil {
		return nil, err
	}
	if s.drives[driveID].OwnerSubject != "" {
		return nil, store.ErrOrgSiteMemberDrive
	}
	from, at := s.homes[driveID]
	s.writes++
	s.homes[driveID] = siteID
	if at {
		return &from, nil
	}
	return nil, nil
}

func (s *orgSiteRouteStore) ClearDriveSite(_ context.Context, driveID uuid.UUID) (*uuid.UUID, error) {
	if err := s.fail["ClearDriveSite"]; err != nil {
		return nil, err
	}
	from, at := s.homes[driveID]
	if !at {
		return nil, nil
	}
	s.writes++
	delete(s.homes, driveID)
	return &from, nil
}

func (s *orgSiteRouteStore) PoolSite(ctx context.Context, poolID uuid.UUID) (types.OrgSite, bool, error) {
	at, ok := s.poolAt[poolID]
	if !ok {
		return types.OrgSite{}, false, nil
	}
	site, err := s.GetOrgSite(ctx, at)
	return site, err == nil && site.State == types.OrgSiteActive, err
}

// orgSiteRoutes is a server over an orgSiteRouteStore holding the site Office with the organization pool
// Office cluster and the admin drive designs at it, an empty site Spare, a deleted site Closed, a second
// organization pool, a person's own pool, a second admin drive and a member drive.
type orgSiteRoutes struct {
	srv                       *Server
	audit                     *recRecorder
	st                        *orgSiteRouteStore
	office, spare, closed     types.OrgSite
	officePool, spool, mine   types.RunnerPool
	designs, notes, memberOne types.UserDrive
}

func newOrgSiteRoutes(t *testing.T) *orgSiteRoutes {
	t.Helper()
	pool := func(name string, scope types.RunnerPoolScope) types.RunnerPool {
		return types.RunnerPool{ID: uuid.New(), Name: name, Scope: scope, State: types.RunnerPoolActive, Revision: 3}
	}
	f := &orgSiteRoutes{
		office:     types.OrgSite{ID: uuid.New(), Name: "Office", State: types.OrgSiteActive, Revision: 2},
		spare:      types.OrgSite{ID: uuid.New(), Name: "Spare", State: types.OrgSiteActive, Revision: 1},
		closed:     types.OrgSite{ID: uuid.New(), Name: "Closed", State: types.OrgSiteDeleted, Revision: 4},
		officePool: pool("Office cluster", types.RunnerPoolOrganization),
		spool:      pool("Second cluster", types.RunnerPoolOrganization),
		mine:       pool("Mine", types.RunnerPoolUser),
		designs:    types.UserDrive{ID: uuid.New(), Name: "designs"},
		notes:      types.UserDrive{ID: uuid.New(), Name: "notes"},
		memberOne:  types.UserDrive{ID: uuid.New(), Name: "scratch", OwnerSubject: "sub-person"},
	}
	f.st = &orgSiteRouteStore{
		sites:  map[uuid.UUID]types.OrgSite{f.office.ID: f.office, f.spare.ID: f.spare, f.closed.ID: f.closed},
		pools:  map[uuid.UUID]types.RunnerPool{f.officePool.ID: f.officePool, f.spool.ID: f.spool, f.mine.ID: f.mine},
		poolAt: map[uuid.UUID]uuid.UUID{f.officePool.ID: f.office.ID},
		drives: map[uuid.UUID]types.UserDrive{f.designs.ID: f.designs, f.notes.ID: f.notes, f.memberOne.ID: f.memberOne},
		homes:  map[uuid.UUID]uuid.UUID{f.designs.ID: f.office.ID},
	}
	h := newHarness(t)
	f.srv, f.audit = New(baseTestConfig(h, f.st)), h.audit
	return f
}

// storeSites stores features.org_sites.enabled as on.
func (f *orgSiteRoutes) storeSites(on bool) {
	f.st.sc.FeatureSwitches = &types.SiteFeatureSwitches{OrgSites: &types.OrgSitesFeature{Enabled: &on}}
}

func (f *orgSiteRoutes) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, method, "/api/v1"+path, adminToken, body)
}

func responseReason(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Reason
}

// orgSiteRows is every org_site.* audit row, in order.
func (f *orgSiteRoutes) orgSiteRows() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "org_site.") {
			out = append(out, ev)
		}
	}
	return out
}

func TestOrgSitesOffRefusesAddsAllowsRemovals(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, f *orgSiteRoutes){
		"nothing stored": func(t *testing.T, _ *orgSiteRoutes) { withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites)) },
		"stored off": func(t *testing.T, f *orgSiteRoutes) {
			withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
			f.storeSites(false)
		},
		"stored on, no hook": func(t *testing.T, f *orgSiteRoutes) { withSiteFeatureHooks(t, nil); f.storeSites(true) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newOrgSiteRoutes(t)
			setup(t, f)
			adds := []struct{ method, path, body string }{
				{http.MethodPost, "/org-sites", `{"name":"Branch"}`},
				{http.MethodPut, "/org-sites/" + f.office.ID.String(), fmt.Sprintf(`{"revision":2,"name":"Office","default_pool_id":%q}`, f.officePool.ID)},
				{http.MethodPut, "/runner-pools/" + f.spool.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.office.ID)},
				{http.MethodPut, "/drives/" + f.notes.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.office.ID)},
			}
			for _, a := range adds {
				w := f.do(t, a.method, a.path, a.body)
				if w.Code != http.StatusForbidden || responseReason(t, w) != reasonOrgSitesOff {
					t.Errorf("%s %s with sites off = %d %s, want 403 %s", a.method, a.path, w.Code, w.Body, reasonOrgSitesOff)
				}
			}
			if f.st.writes != 0 || len(f.orgSiteRows()) != 0 {
				t.Fatalf("a refused add reached the store (%d writes) or the audit (%d rows)", f.st.writes, len(f.orgSiteRows()))
			}
			if w := f.do(t, http.MethodGet, "/org-sites", ""); w.Code != http.StatusOK {
				t.Errorf("GET /org-sites with sites off = %d %s, want 200", w.Code, w.Body)
			}
			removals := []struct{ method, path string }{
				{http.MethodDelete, "/drives/" + f.designs.ID.String() + "/site"},
				{http.MethodDelete, "/runner-pools/" + f.officePool.ID.String() + "/site"},
				{http.MethodDelete, "/org-sites/" + f.spare.ID.String()},
				{http.MethodDelete, "/org-sites/" + f.office.ID.String()},
			}
			for _, rm := range removals {
				if w := f.do(t, rm.method, rm.path, ""); w.Code != http.StatusNoContent {
					t.Errorf("%s %s with sites off = %d %s, want 204", rm.method, rm.path, w.Code, w.Body)
				}
			}
			if len(f.st.homes) != 0 || len(f.st.poolAt) != 0 || f.st.sites[f.office.ID].State != types.OrgSiteDeleted {
				t.Errorf("removals with sites off left homes %v, pool sites %v, office %s", f.st.homes, f.st.poolAt, f.st.sites[f.office.ID].State)
			}
		})
	}
	// The same add goes through once the switch is registered and stored on: the refusal above is the switch's.
	f := newOrgSiteRoutes(t)
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	f.storeSites(true)
	if w := f.do(t, http.MethodPost, "/org-sites", `{"name":"Branch"}`); w.Code != http.StatusCreated {
		t.Errorf("POST /org-sites with sites on = %d %s, want 201", w.Code, w.Body)
	}
}

func TestOrgSiteWritesAudited(t *testing.T) {
	f := newOrgSiteRoutes(t)
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	f.storeSites(true)
	w := f.do(t, http.MethodPost, "/org-sites", `{"name":"Branch","storage_label":"Branch NAS"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	branch := decodeJSON[types.OrgSite](t, w)
	sitePath := "/org-sites/" + branch.ID.String()
	onBranch := fmt.Sprintf(`{"site_id":%q}`, branch.ID)
	steps := []struct {
		method, path, body string
		code               int
	}{
		{http.MethodPut, "/runner-pools/" + f.officePool.ID.String() + "/site", onBranch, http.StatusOK},
		{http.MethodPut, "/runner-pools/" + f.officePool.ID.String() + "/site", onBranch, http.StatusOK}, // already there: no row
		{http.MethodPut, sitePath, fmt.Sprintf(`{"revision":1,"name":"Branch office","storage_label":"Branch NAS","default_pool_id":%q}`, f.officePool.ID), http.StatusOK},
		{http.MethodPut, "/drives/" + f.designs.ID.String() + "/site", onBranch, http.StatusOK},
		{http.MethodDelete, "/drives/" + f.designs.ID.String() + "/site", "", http.StatusNoContent},
		{http.MethodDelete, "/drives/" + f.designs.ID.String() + "/site", "", http.StatusNoContent}, // no home: no row
		{http.MethodDelete, "/runner-pools/" + f.officePool.ID.String() + "/site", "", http.StatusNoContent},
		{http.MethodDelete, sitePath, "", http.StatusNoContent},
	}
	for _, s := range steps {
		if w := f.do(t, s.method, s.path, s.body); w.Code != s.code {
			t.Fatalf("%s %s = %d %s, want %d", s.method, s.path, w.Code, w.Body, s.code)
		}
	}
	want := []string{auditOrgSiteCreate, auditOrgSitePoolAssign, auditOrgSiteUpdate, auditOrgSiteDriveSet,
		auditOrgSiteDriveRemove, auditOrgSitePoolRemove, auditOrgSiteDelete}
	rows := f.orgSiteRows()
	var got []string
	for _, ev := range rows {
		got = append(got, ev.Action)
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if ev.Target != branch.ID.String() || d["site_id"] != branch.ID.String() || d["site_name"] == nil {
			t.Errorf("%s: target %s, site_id %v, site_name %v; want the site %s and its name", ev.Action, ev.Target, d["site_id"], d["site_name"], branch.ID)
		}
		if ev.Outcome != "success" {
			t.Errorf("%s: outcome %q, want success", ev.Action, ev.Outcome)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("site audit rows = %v, want %v (one per change, none for a write that changes nothing)", got, want)
	}
	// A pool moved from Office to Branch names the site it left; the pool's revision rises.
	var moved map[string]any
	_ = json.Unmarshal(rows[1].Data, &moved)
	if moved["site_before"] != f.office.ID.String() || moved["revision"] != float64(4) {
		t.Errorf("pool assign row = %v, want site_before %s and revision 4", moved, f.office.ID)
	}
}

func TestOrgSiteAuditCarriesNoStorageURL(t *testing.T) {
	f := newOrgSiteRoutes(t)
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	f.storeSites(true)
	const link, next = "https://files.example.com/share", "https://nas.example.org/designs"
	// A link that could carry a credential is refused before the store is reached.
	for _, bad := range []string{"https://user:secret@files.example.com/share", "https://files.example.com/share?token=x", "ftp://files.example.com"} {
		w := f.do(t, http.MethodPost, "/org-sites", fmt.Sprintf(`{"name":"Branch","storage_url":%q}`, bad))
		if w.Code != http.StatusBadRequest || responseReason(t, w) != reasonOrgSiteInvalid {
			t.Errorf("create with storage_url %q = %d %s, want 400 %s", bad, w.Code, w.Body, reasonOrgSiteInvalid)
		}
	}
	if f.st.writes != 0 {
		t.Fatalf("a refused storage link reached the store (%d writes)", f.st.writes)
	}
	w := f.do(t, http.MethodPost, "/org-sites", fmt.Sprintf(`{"name":"Branch","storage_label":"Branch NAS","storage_url":%q}`, link))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	branch := decodeJSON[types.OrgSite](t, w)
	if branch.StorageURL != link {
		t.Errorf("the site answers storage_url %q, want %q: the note is shown, only the audit leaves it out", branch.StorageURL, link)
	}
	body := fmt.Sprintf(`{"revision":1,"name":"Branch","storage_label":"Branch NAS","storage_url":%q}`, next)
	if w := f.do(t, http.MethodPut, "/org-sites/"+branch.ID.String(), body); w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodPut, "/runner-pools/"+f.spool.ID.String()+"/site", fmt.Sprintf(`{"site_id":%q}`, branch.ID)); w.Code != http.StatusOK {
		t.Fatalf("assign = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodDelete, "/runner-pools/"+f.spool.ID.String()+"/site", ""); w.Code != http.StatusNoContent {
		t.Fatalf("unassign = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodDelete, "/org-sites/"+branch.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	rows := f.orgSiteRows()
	if len(rows) != 5 {
		t.Fatalf("site audit rows = %d, want 5", len(rows))
	}
	for _, ev := range rows {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		for k := range d {
			if strings.Contains(k, "_url") {
				t.Errorf("%s carries the key %q: a storage link never enters an audit row", ev.Action, k)
			}
		}
		if raw := string(ev.Data); strings.Contains(raw, "files.example.com") || strings.Contains(raw, "nas.example.org") {
			t.Errorf("%s data %s carries the storage link", ev.Action, raw)
		}
	}
	var created, updated map[string]any
	_ = json.Unmarshal(rows[0].Data, &created)
	_ = json.Unmarshal(rows[1].Data, &updated)
	if created["storage_link_set"] != true || created["storage_label"] != "Branch NAS" {
		t.Errorf("create row = %v, want storage_link_set true and the label", created)
	}
	if updated["storage_link_set"] != true || updated["storage_link_changed"] != true {
		t.Errorf("update row = %v, want storage_link_set and storage_link_changed true", updated)
	}
}

// sitesOnRoutes registers the switch and stores it on.
func sitesOnRoutes(t *testing.T, f *orgSiteRoutes) {
	t.Helper()
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	f.storeSites(true)
}

// wantSiteRefusal fails unless w answered code with reason.
func wantSiteRefusal(t *testing.T, what string, w *httptest.ResponseRecorder, code int, reason string) {
	t.Helper()
	if got := responseReason(t, w); w.Code != code || got != reason {
		t.Errorf("%s = %d %q (%s), want %d %q", what, w.Code, got, w.Body, code, reason)
	}
}

func TestOrgSiteUnknownOrDeletedSiteIsNotFound(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	unknown := uuid.New()
	for name, c := range map[string]struct{ method, path, body string }{
		"update an unknown site":       {http.MethodPut, "/org-sites/" + unknown.String(), `{"revision":1,"name":"X"}`},
		"update a deleted site":        {http.MethodPut, "/org-sites/" + f.closed.ID.String(), `{"revision":4,"name":"Closed"}`},
		"delete a deleted site":        {http.MethodDelete, "/org-sites/" + f.closed.ID.String(), ""},
		"read an unknown site":         {http.MethodGet, "/org-sites/" + unknown.String(), ""},
		"a pool to an unknown site":    {http.MethodPut, "/runner-pools/" + f.spool.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, unknown)},
		"a pool to a deleted site":     {http.MethodPut, "/runner-pools/" + f.spool.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.closed.ID)},
		"a drive's home a deleted one": {http.MethodPut, "/drives/" + f.notes.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.closed.ID)},
	} {
		wantSiteRefusal(t, name, f.do(t, c.method, c.path, c.body), http.StatusNotFound, reasonOrgSiteNotFound)
	}
	if f.st.writes != 0 || len(f.orgSiteRows()) != 0 {
		t.Errorf("a refusal reached the store (%d writes) or the audit (%d rows)", f.st.writes, len(f.orgSiteRows()))
	}
	// A read of a deleted site answers it, with its state, so a run placed there can still name it.
	w := f.do(t, http.MethodGet, "/org-sites/"+f.closed.ID.String(), "")
	if got := decodeJSON[types.OrgSite](t, w); w.Code != http.StatusOK || got.State != types.OrgSiteDeleted {
		t.Errorf("GET a deleted site = %d, state %q; want 200 and deleted", w.Code, got.State)
	}
}

func TestOrgSiteRefusesAMemberDriveAndAPersonsPool(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	onOffice := fmt.Sprintf(`{"site_id":%q}`, f.office.ID)
	wantSiteRefusal(t, "a member drive's home", f.do(t, http.MethodPut, "/drives/"+f.memberOne.ID.String()+"/site", onOffice),
		http.StatusUnprocessableEntity, reasonOrgSitePoolScope)
	wantSiteRefusal(t, "a person's own pool at a site", f.do(t, http.MethodPut, "/runner-pools/"+f.mine.ID.String()+"/site", onOffice),
		http.StatusUnprocessableEntity, reasonOrgSitePoolScope)
	if f.st.writes != 0 || len(f.orgSiteRows()) != 0 {
		t.Errorf("a refusal reached the store (%d writes) or the audit (%d rows)", f.st.writes, len(f.orgSiteRows()))
	}
}

func TestOrgSiteReadKeepsADeletedPoolAndHidesAnUncataloguedOne(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	gone := types.RunnerPool{ID: uuid.New(), Name: "Gone", Scope: types.RunnerPoolOrganization, State: types.RunnerPoolDeleted}
	veiled := types.RunnerPool{ID: uuid.New(), Name: "Veiled", Scope: types.RunnerPoolOrganization, State: types.RunnerPoolActive}
	f.st.pools[gone.ID], f.st.pools[veiled.ID] = gone, veiled
	f.st.poolAt[gone.ID], f.st.poolAt[veiled.ID] = f.office.ID, f.office.ID
	f.st.uncatalogued = map[uuid.UUID]bool{veiled.ID: true}
	office := f.st.sites[f.office.ID]
	office.DefaultPoolID = &veiled.ID
	f.st.sites[f.office.ID] = office
	check := func(what string, site types.OrgSite) {
		t.Helper()
		if !slices.Contains(site.PoolIDs, f.officePool.ID) || !slices.Contains(site.PoolIDs, gone.ID) || slices.Contains(site.PoolIDs, veiled.ID) {
			t.Errorf("%s pools = %v, want the live catalogued pool and the deleted one, not the uncatalogued one", what, site.PoolIDs)
		}
		if site.DefaultPoolID != nil {
			t.Errorf("%s default = %v, want none: the default is a pool the reader's catalogue does not show", what, site.DefaultPoolID)
		}
	}
	check("GET /org-sites/{id}", decodeJSON[types.OrgSite](t, f.do(t, http.MethodGet, "/org-sites/"+f.office.ID.String(), "")))
	var listed types.OrgSite
	for _, s := range decodeJSON[[]types.OrgSite](t, f.do(t, http.MethodGet, "/org-sites", "")) {
		if s.ID == f.office.ID {
			listed = s
		}
	}
	check("GET /org-sites", listed)
	drives := decodeJSON[[]types.OrgSiteDrive](t, f.do(t, http.MethodGet, "/org-sites/drives", ""))
	if len(drives) != 1 || drives[0].DriveID != f.designs.ID || drives[0].SiteName != "Office" {
		t.Errorf("GET /org-sites/drives = %+v, want designs at Office", drives)
	}
}

func TestOrgSiteUnreadableSiteConfigRefusesAdds(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	f.st.scErr = errors.New("site config unreadable")
	for _, a := range []struct{ method, path, body string }{
		{http.MethodPost, "/org-sites", `{"name":"Branch"}`},
		{http.MethodPut, "/org-sites/" + f.office.ID.String(), `{"revision":2,"name":"Office 2"}`},
		{http.MethodPut, "/runner-pools/" + f.spool.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.office.ID)},
		{http.MethodPut, "/drives/" + f.notes.ID.String() + "/site", fmt.Sprintf(`{"site_id":%q}`, f.office.ID)},
	} {
		if w := f.do(t, a.method, a.path, a.body); w.Code != http.StatusInternalServerError {
			t.Errorf("%s %s with the site config unreadable = %d %s, want 500", a.method, a.path, w.Code, w.Body)
		}
	}
	if f.st.writes != 0 {
		t.Errorf("an add reached the store (%d writes) with the switch unreadable", f.st.writes)
	}
}

func TestOrgSiteUpdateValidatesBeforeTheStore(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	for _, body := range []string{
		`{"revision":2,"name":""}`,
		`{"revision":2,"name":" Office"}`,
		`{"revision":2,"name":"Office","storage_url":"https://user:secret@files.example.com/share"}`,
		`{"revision":2,"name":"Office","storage_url":"https://files.example.com/share?token=x"}`,
	} {
		wantSiteRefusal(t, "update "+body, f.do(t, http.MethodPut, "/org-sites/"+f.office.ID.String(), body), http.StatusBadRequest, reasonOrgSiteInvalid)
	}
	if f.st.writes != 0 || len(f.orgSiteRows()) != 0 {
		t.Errorf("an invalid update reached the store (%d writes) or the audit (%d rows)", f.st.writes, len(f.orgSiteRows()))
	}
}

func TestOrgSiteUpdateThatChangesNothingWritesNoRow(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	w := f.do(t, http.MethodPut, "/org-sites/"+f.office.ID.String(), `{"revision":2,"name":"Office"}`)
	if got := decodeJSON[types.OrgSite](t, w); w.Code != http.StatusOK || got.Revision != 2 {
		t.Fatalf("an update that changes nothing = %d, revision %d; want 200 and revision 2", w.Code, got.Revision)
	}
	if rows := f.orgSiteRows(); len(rows) != 0 {
		t.Errorf("an update that changes nothing wrote %d site rows, want 0", len(rows))
	}
}

func TestOrgSiteDefaultPoolClearedIsAudited(t *testing.T) {
	f := newOrgSiteRoutes(t)
	sitesOnRoutes(t, f)
	office := f.st.sites[f.office.ID]
	office.DefaultPoolID = &f.officePool.ID
	f.st.sites[f.office.ID] = office
	rowData := func(i int) map[string]any {
		t.Helper()
		rows := f.orgSiteRows()
		if len(rows) <= i {
			t.Fatalf("site audit rows = %d, want more than %d", len(rows), i)
		}
		var d map[string]any
		if err := json.Unmarshal(rows[i].Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	onSpare := fmt.Sprintf(`{"site_id":%q}`, f.spare.ID)
	// Office's default moves to Spare: Office loses its default.
	if w := f.do(t, http.MethodPut, "/runner-pools/"+f.officePool.ID.String()+"/site", onSpare); w.Code != http.StatusOK {
		t.Fatalf("move the default pool = %d %s", w.Code, w.Body)
	}
	if d := rowData(0); d["default_pool_cleared"] != true || d["site_before"] != f.office.ID.String() {
		t.Errorf("moving a site's default pool: row %v, want default_pool_cleared true and site_before Office", d)
	}
	if f.st.sites[f.office.ID].DefaultPoolID != nil {
		t.Errorf("Office still names its moved pool as its default")
	}
	// A pool that was not its site's default moves: false.
	f.st.poolAt[f.spool.ID] = f.office.ID
	if w := f.do(t, http.MethodPut, "/runner-pools/"+f.spool.ID.String()+"/site", onSpare); w.Code != http.StatusOK {
		t.Fatalf("move a pool = %d %s", w.Code, w.Body)
	}
	if d := rowData(1); d["default_pool_cleared"] != false {
		t.Errorf("moving a pool that was not the default: row %v, want default_pool_cleared false", d)
	}
	// Spare makes the pool its default, then the pool leaves: cleared again.
	spare := f.st.sites[f.spare.ID]
	body := fmt.Sprintf(`{"revision":%d,"name":"Spare","default_pool_id":%q}`, spare.Revision, f.officePool.ID)
	if w := f.do(t, http.MethodPut, "/org-sites/"+f.spare.ID.String(), body); w.Code != http.StatusOK {
		t.Fatalf("set Spare's default = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodDelete, "/runner-pools/"+f.officePool.ID.String()+"/site", ""); w.Code != http.StatusNoContent {
		t.Fatalf("take the default pool off = %d %s", w.Code, w.Body)
	}
	if d := rowData(3); d["default_pool_cleared"] != true || d["site_id"] != f.spare.ID.String() {
		t.Errorf("taking a site's default pool off: row %v, want default_pool_cleared true at Spare", d)
	}
}

func TestOrgSiteStoreRefusalsKeepTheirReasons(t *testing.T) {
	onOffice := `{"site_id":"%s"}`
	cases := []struct {
		name, method, path, body string
		storeErr                 map[string]error
		code                     int
		reason                   string
	}{
		{"update stale in the handler", http.MethodPut, "/org-sites/{office}", `{"revision":1,"name":"Office"}`, nil, http.StatusConflict, reasonOrgSiteStale},
		{"update stale in the store", http.MethodPut, "/org-sites/{office}", `{"revision":2,"name":"Office 2"}`, map[string]error{"UpdateOrgSite": store.ErrOrgSiteStale}, http.StatusConflict, reasonOrgSiteStale},
		{"update name taken", http.MethodPut, "/org-sites/{office}", `{"revision":2,"name":"Spare"}`, map[string]error{"UpdateOrgSite": store.ErrConflict}, http.StatusConflict, reasonOrgSiteNameTaken},
		{"update default not at the site, in the handler", http.MethodPut, "/org-sites/{office}", `{"revision":2,"name":"Office","default_pool_id":"{spool}"}`, nil, http.StatusUnprocessableEntity, reasonOrgSiteDefaultPool},
		{"update default not at the site, in the store", http.MethodPut, "/org-sites/{office}", `{"revision":2,"name":"Office","default_pool_id":"{officePool}"}`, map[string]error{"UpdateOrgSite": store.ErrOrgSiteDefaultPool}, http.StatusUnprocessableEntity, reasonOrgSiteDefaultPool},
		{"update deleted meanwhile", http.MethodPut, "/org-sites/{office}", `{"revision":2,"name":"Office 2"}`, map[string]error{"UpdateOrgSite": store.ErrNotFound}, http.StatusNotFound, reasonOrgSiteNotFound},
		{"create name taken", http.MethodPost, "/org-sites", `{"name":"Office"}`, nil, http.StatusConflict, reasonOrgSiteNameTaken},
		{"delete in use", http.MethodDelete, "/org-sites/{office}", "", nil, http.StatusConflict, reasonOrgSiteInUse},
		{"delete unknown in the store", http.MethodDelete, "/org-sites/{spare}", "", map[string]error{"DeleteOrgSite": store.ErrNotFound}, http.StatusNotFound, reasonOrgSiteNotFound},
		{"assign a user pool in the store", http.MethodPut, "/runner-pools/{spool}/site", onOffice, map[string]error{"AssignPoolSite": store.ErrRunnerPoolScope}, http.StatusUnprocessableEntity, reasonOrgSitePoolScope},
		{"assign, site or pool gone meanwhile", http.MethodPut, "/runner-pools/{spool}/site", onOffice, map[string]error{"AssignPoolSite": store.ErrNotFound}, http.StatusNotFound, reasonOrgSiteNotFound},
		{"unassign a user pool in the store", http.MethodDelete, "/runner-pools/{officePool}/site", "", map[string]error{"ClearPoolSite": store.ErrRunnerPoolScope}, http.StatusUnprocessableEntity, reasonOrgSitePoolScope},
		{"unassign, pool gone meanwhile", http.MethodDelete, "/runner-pools/{officePool}/site", "", map[string]error{"ClearPoolSite": store.ErrNotFound}, http.StatusNotFound, "runner_pool_not_found"},
		{"home a member drive in the store", http.MethodPut, "/drives/{notes}/site", onOffice, map[string]error{"SetDriveSite": store.ErrOrgSiteMemberDrive}, http.StatusUnprocessableEntity, reasonOrgSitePoolScope},
		{"home, site gone meanwhile", http.MethodPut, "/drives/{notes}/site", onOffice, map[string]error{"SetDriveSite": store.ErrNotFound}, http.StatusNotFound, reasonOrgSiteNotFound},
		{"an unknown drive", http.MethodPut, "/drives/" + uuid.NewString() + "/site", onOffice, nil, http.StatusNotFound, reasonUserDriveNotFound},
		{"clear a home, store error", http.MethodDelete, "/drives/{designs}/site", "", map[string]error{"ClearDriveSite": errors.New("boom")}, http.StatusInternalServerError, reasonInternalError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOrgSiteRoutes(t)
			sitesOnRoutes(t, f)
			f.st.fail = c.storeErr
			fill := strings.NewReplacer("{office}", f.office.ID.String(), "{spare}", f.spare.ID.String(), "{spool}", f.spool.ID.String(),
				"{officePool}", f.officePool.ID.String(), "{notes}", f.notes.ID.String(), "{designs}", f.designs.ID.String())
			body := fill.Replace(c.body)
			if strings.Contains(body, "%s") {
				body = fmt.Sprintf(body, f.office.ID)
			}
			wantSiteRefusal(t, c.name, f.do(t, c.method, fill.Replace(c.path), body), c.code, c.reason)
			if f.st.writes != 0 || len(f.orgSiteRows()) != 0 {
				t.Errorf("a refusal wrote %d store changes and %d site rows, want none", f.st.writes, len(f.orgSiteRows()))
			}
		})
	}
}

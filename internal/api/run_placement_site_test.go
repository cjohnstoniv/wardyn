// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteStore serves a pool catalogue, the person's and the organisation's defaults, the site config and
// the site reads placement makes, beside the governance fake. Each read can be made to fail, and the site
// reads are counted.
type siteStore struct {
	*capStore
	store.RunnerPoolStore
	store.OrgSiteStore
	rows      []store.RunnerPoolCatalogueRow
	personal  types.RunnerPoolDefaults
	org       types.RunnerPoolDefaults
	homes     map[uuid.UUID]uuid.UUID
	sites     map[uuid.UUID]types.OrgSite
	poolSites map[uuid.UUID]uuid.UUID
	sc        types.SiteConfig

	configErr, homesErr, poolSiteErr error
	siteReads                        int
}

var (
	_ store.RunnerPoolStore    = (*siteStore)(nil)
	_ store.OrgSiteStore       = (*siteStore)(nil)
	_ store.PrincipalPrefStore = (*siteStore)(nil)
)

func (s *siteStore) ListRunnerPoolCatalogue(context.Context, string, bool) ([]store.RunnerPoolCatalogueRow, error) {
	return slices.Clone(s.rows), nil
}

func (s *siteStore) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	return s.org, nil
}

func (s *siteStore) GetPrincipalPref(context.Context, string, string) (json.RawMessage, error) {
	if s.personal.PoolID == nil {
		return nil, store.ErrNotFound
	}
	return json.Marshal(s.personal)
}

// PutPrincipalPref makes siteStore a PrincipalPrefStore, without which placement reads no personal default.
func (s *siteStore) PutPrincipalPref(context.Context, string, string, json.RawMessage) error {
	return nil
}

func (s *siteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, s.configErr
}

func (s *siteStore) DriveHomeSites(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.siteReads++
	if s.homesErr != nil {
		return nil, s.homesErr
	}
	out := map[uuid.UUID]uuid.UUID{}
	for _, id := range ids {
		if site, ok := s.homes[id]; ok && s.sites[site].State == types.OrgSiteActive {
			out[id] = site
		}
	}
	return out, nil
}

func (s *siteStore) GetOrgSite(_ context.Context, id uuid.UUID) (types.OrgSite, error) {
	s.siteReads++
	site, ok := s.sites[id]
	if !ok {
		return types.OrgSite{}, store.ErrNotFound
	}
	return site, nil
}

func (s *siteStore) PoolSite(_ context.Context, poolID uuid.UUID) (types.OrgSite, bool, error) {
	s.siteReads++
	if s.poolSiteErr != nil {
		return types.OrgSite{}, false, s.poolSiteErr
	}
	site, ok := s.sites[s.poolSites[poolID]]
	if !ok || site.State != types.OrgSiteActive {
		return types.OrgSite{}, false, nil
	}
	return site, true, nil
}

// siteFixture is an office site whose default pool is Office, a person whose own default is Mine, the
// organisation's default Shared, and an admin drive homed at the office. Mine is at a second site, Branch,
// so a run placed in it with sites on records Branch.
type siteFixture struct {
	srv                   *Server
	st                    *siteStore
	office, branch        types.OrgSite
	officePool, mine, org types.RunnerPool
	drive                 uuid.UUID
}

func newSiteFixture(t *testing.T) *siteFixture {
	t.Helper()
	f := &siteFixture{drive: uuid.New()}
	pool := func(name string) types.RunnerPool {
		return types.RunnerPool{ID: uuid.New(), Name: name, Scope: types.RunnerPoolOrganization, State: types.RunnerPoolActive, Revision: 2}
	}
	f.officePool, f.mine, f.org = pool("Office"), pool("Mine"), pool("Shared")
	f.office = types.OrgSite{ID: uuid.New(), Name: "Leeds office", State: types.OrgSiteActive, DefaultPoolID: &f.officePool.ID}
	f.branch = types.OrgSite{ID: uuid.New(), Name: "Branch", State: types.OrgSiteActive}
	f.st = &siteStore{capStore: &capStore{govTier: types.CapabilitySubjectUser},
		rows:      []store.RunnerPoolCatalogueRow{{Pool: f.officePool}, {Pool: f.mine}, {Pool: f.org}},
		personal:  types.RunnerPoolDefaults{PoolID: &f.mine.ID},
		org:       types.RunnerPoolDefaults{PoolID: &f.org.ID},
		homes:     map[uuid.UUID]uuid.UUID{f.drive: f.office.ID},
		sites:     map[uuid.UUID]types.OrgSite{f.office.ID: f.office, f.branch.ID: f.branch},
		poolSites: map[uuid.UUID]uuid.UUID{f.officePool.ID: f.office.ID, f.mine.ID: f.branch.ID},
	}
	f.srv = &Server{cfg: Config{DefaultPolicy: govDeployment(), Store: f.st}}
	return f
}

// sitesOn registers the switch and stores it on; sitesStored stores enabled without registering it.
func sitesOn(t *testing.T, f *siteFixture) {
	t.Helper()
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	sitesStored(f, true)
}

func sitesStored(f *siteFixture, on bool) {
	f.st.sc.FeatureSwitches = &types.SiteFeatureSwitches{OrgSites: &types.OrgSitesFeature{Enabled: &on}}
}

// driveRun is a create that mounts the office drive and names no pool.
func (f *siteFixture) driveRun() placementInput {
	return placementInput{req: &createRunRequest{}, drives: true, driveIDs: []uuid.UUID{f.drive}}
}

// place resolves in's pool as create does and stamps the run row and the run.create fields from it, with
// the run bound to one runner of the pool.
func (f *siteFixture) place(t *testing.T, in placementInput) (types.AgentRun, map[string]any, *runRefusal) {
	t.Helper()
	_, _, pool, pref, refusal := f.srv.resolveRunPool(govMemberCtx([]string{"eng"}, false), f.st, in)
	if refusal != nil {
		return types.AgentRun{}, nil, refusal
	}
	rid := uuid.New()
	p := runPlacement{pooled: true, pool: pool, sitePref: pref,
		eligible: []poolTarget{{id: rid.String(), runnerID: &rid, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1}}}}}
	run := types.AgentRun{ID: uuid.New()}
	if r := p.apply(&run, types.CC1); r != nil {
		t.Fatalf("apply: %+v", r)
	}
	return run, p.auditFields(run, ""), nil
}

func mustPlace(t *testing.T, f *siteFixture, in placementInput) (types.AgentRun, map[string]any) {
	t.Helper()
	run, audit, refusal := f.place(t, in)
	if refusal != nil {
		t.Fatalf("placement refused: %d %+v %v", refusal.status, refusal.body, refusal.err)
	}
	return run, audit
}

func siteKeys(audit map[string]any) []string {
	var out []string
	for k := range maps.Keys(audit) {
		if strings.HasPrefix(k, "site_") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// With the switch off, or stored on by a release that cannot turn it on, placement, the run row and the
// run.create fields are today's: the person's default is chosen though the drive is homed at a site with a
// default pool, nothing about sites is read, and a run placed in a pool that is at a site records none.
func TestOrgSitesOffIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *siteFixture)
	}{
		{"switch off", func(t *testing.T, f *siteFixture) {
			withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
			sitesStored(f, false)
		}},
		{"stored on, not registered", func(t *testing.T, f *siteFixture) {
			withSiteFeatureHooks(t, nil)
			sitesStored(f, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiteFixture(t)
			tc.setup(t, f)
			permitted := []types.RunnerPool{f.officePool, f.mine, f.org}
			golden, ref := runnerpool.Resolve(permitted, runnerpool.Choice{}, f.st.personal, f.st.org)
			if ref != nil {
				t.Fatal(ref)
			}
			run, audit := mustPlace(t, f, f.driveRun())
			if *run.RunnerPoolID != golden.ID || run.RunnerPoolSelection != golden.Selection || golden.ID != f.mine.ID {
				t.Errorf("pool %s via %s, want today's %s via %s", *run.RunnerPoolID, run.RunnerPoolSelection, golden.Name, golden.Selection)
			}
			if run.SiteID != nil {
				t.Errorf("a run placed in a pool at a site records site %s with sites off", run.SiteID)
			}
			want := runPlacement{pooled: true, pool: golden}.auditFields(run, "")
			if got := slices.Sorted(maps.Keys(audit)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
				t.Errorf("run.create keys = %v, want today's %v", got, slices.Sorted(maps.Keys(want)))
			}
			if f.st.siteReads != 0 {
				t.Errorf("%d site reads with sites off", f.st.siteReads)
			}
		})
	}
}

// A site grants nothing: a site default the caller's use policy keeps them out of is passed over, as if
// the site had none, and the run goes to the person's own default.
func TestSiteDefaultNeverWidensUsePolicy(t *testing.T) {
	f := newSiteFixture(t)
	sitesOn(t, f)
	f.st.rows[0].Policy = &types.RunnerPoolUsePolicy{PoolID: f.officePool.ID,
		Subjects: []types.RunnerPoolSubject{{SubjectType: types.CapabilitySubjectGroup, Subject: "ops"}}}
	run, audit := mustPlace(t, f, f.driveRun())
	if *run.RunnerPoolID != f.mine.ID || run.RunnerPoolSelection != types.RunnerPoolSelectedPersonal {
		t.Fatalf("pool %s via %s, want the person's default", *run.RunnerPoolID, run.RunnerPoolSelection)
	}
	if audit["site_preference"] != runnerpool.SiteUnusable {
		t.Errorf("site_preference = %v, want unusable", audit["site_preference"])
	}
	// The same caller in the policy's group gets the site's pool.
	f.st.rows[0].Policy.Subjects[0].Subject = "eng"
	if run, _ := mustPlace(t, f, f.driveRun()); *run.RunnerPoolID != f.officePool.ID || run.RunnerPoolSelection != types.RunnerPoolSelectedSite {
		t.Errorf("a caller the policy admits: pool %s via %s, want the site's default", *run.RunnerPoolID, run.RunnerPoolSelection)
	}
}

// Every run placed at a site records it on the run row and in run.create: chosen as the drives' site's
// default, and named explicitly, which records the site but no preference.
func TestRunCreateAuditCarriesSite(t *testing.T) {
	f := newSiteFixture(t)
	sitesOn(t, f)
	run, audit := mustPlace(t, f, f.driveRun())
	if run.SiteID == nil || *run.SiteID != f.office.ID || run.RunnerPoolSelection != types.RunnerPoolSelectedSite || *run.RunnerPoolID != f.officePool.ID {
		t.Fatalf("run = pool %v via %s at %v, want Office via site_default at the office", run.RunnerPoolID, run.RunnerPoolSelection, run.SiteID)
	}
	if audit["site_id"] != run.SiteID || audit["site_name"] != f.office.Name || audit["site_preference"] != runnerpool.SiteMet ||
		audit["runner_pool_selection"] != types.RunnerPoolSelectedSite {
		t.Errorf("run.create = %v", audit)
	}

	named := f.driveRun()
	named.req.RunnerPoolID = f.mine.ID.String()
	run, audit = mustPlace(t, f, named)
	if run.SiteID == nil || *run.SiteID != f.branch.ID || audit["site_name"] != f.branch.Name {
		t.Errorf("a named pool at a site: run site %v, run.create %v", run.SiteID, audit)
	}
	if _, has := audit["site_preference"]; has || run.RunnerPoolSelection != types.RunnerPoolSelectedExplicit {
		t.Errorf("a named pool: selection %s, run.create %v; the site step never ran", run.RunnerPoolSelection, audit)
	}

	// Drives homed at two sites prefer neither.
	other := uuid.New()
	f.st.homes[other] = f.branch.ID
	two := f.driveRun()
	two.driveIDs = append(two.driveIDs, other)
	if run, audit := mustPlace(t, f, two); *run.RunnerPoolID != f.mine.ID || audit["site_preference"] != runnerpool.SiteAmbiguous {
		t.Errorf("two sites: pool %s, run.create %v; want the person's default, ambiguous", *run.RunnerPoolID, audit)
	}
	// A drive with no home is passed over.
	unhomed := f.driveRun()
	unhomed.driveIDs = append(unhomed.driveIDs, uuid.New())
	if run, _ := mustPlace(t, f, unhomed); *run.RunnerPoolID != f.officePool.ID {
		t.Errorf("an unhomed drive beside a homed one: pool %s, want the site's default", *run.RunnerPoolID)
	}
}

// A run held to one runner skips the site step and is placed as today, with site_preference pinned: a
// drive bound to the person's runner, a kept machine resumed with no drives or with drives bound to its
// runner, workspace folders bound to a runner, and a request runner_id. A kept machine resumed with
// unbound drives keeps no pin, so the site step runs for it.
func TestSiteStepSkippedWhenRunPinned(t *testing.T) {
	machine := uuid.New()
	pin := func(drives bool, driveRunner *uuid.UUID) *uuid.UUID {
		_, r := keptPlacementPin(drives, driveRunner, &machine)
		return r
	}
	for _, tc := range []struct {
		name   string
		shape  func(*placementInput)
		pinned bool
	}{
		{"a drive bound to the person's runner", func(in *placementInput) { r := uuid.New(); in.driveRunner = &r }, true},
		{"a kept machine with no drives", func(in *placementInput) { in.driveIDs, in.driveRunner = nil, pin(false, nil) }, true},
		{"a kept machine with drives bound to its runner", func(in *placementInput) { in.driveRunner = pin(true, &machine) }, true},
		{"bound workspace folders", func(in *placementInput) { r := uuid.New(); in.mountRunner = &r }, true},
		{"a request runner_id", func(in *placementInput) { in.req.RunnerID = uuid.NewString() }, true},
		{"a kept machine with unbound drives", func(in *placementInput) { in.driveRunner = pin(true, nil) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiteFixture(t)
			sitesOn(t, f)
			in := f.driveRun()
			tc.shape(&in)
			run, audit := mustPlace(t, f, in)
			if !tc.pinned {
				if *run.RunnerPoolID != f.officePool.ID || audit["site_preference"] != runnerpool.SiteMet {
					t.Errorf("pool %s, run.create %v; want the site's default, met", *run.RunnerPoolID, audit)
				}
				return
			}
			if *run.RunnerPoolID != f.mine.ID || run.RunnerPoolSelection != types.RunnerPoolSelectedPersonal || audit["site_preference"] != runnerpool.SitePinned {
				t.Errorf("pool %s via %s, run.create %v; want the person's default, pinned", *run.RunnerPoolID, run.RunnerPoolSelection, audit)
			}
			// A pinned run still records its pool's site; only the drives' homes go unread.
			if run.SiteID == nil || *run.SiteID != f.branch.ID {
				t.Errorf("a pinned run in a pool at a site records %v, want the branch", run.SiteID)
			}
			if f.st.siteReads != 1 {
				t.Errorf("%d site reads for a pinned run, want only the pool's site", f.st.siteReads)
			}
		})
	}
}

// A usable site default is a choice: when it has no runner for the run, the run is refused with the
// status and reason the same pool chosen as the person's default gets, the sentence names the site and
// says how to run elsewhere, and the run is never put in the person's default instead.
func TestSiteDefaultNoEligibleTargetAnswersLikeAChosenPool(t *testing.T) {
	f := newSiteFixture(t)
	sitesOn(t, f)
	_, _, pool, _, refusal := f.srv.resolveRunPool(govMemberCtx([]string{"eng"}, false), f.st, f.driveRun())
	if refusal != nil || pool.ID != f.officePool.ID || pool.Selection != types.RunnerPoolSelectedSite {
		t.Fatalf("pool %+v %+v, want the site's default before any runner is looked at", pool, refusal)
	}
	asPersonal := pool
	asPersonal.Selection = types.RunnerPoolSelectedPersonal
	for _, r := range []*runRefusal{
		targetRefusal(placement.ReasonRunnerOffline, pool.Name, pool.Scope),
		targetRefusal(placement.ReasonRunnerAtCapacity, pool.Name, pool.Scope),
		targetRefusal(placement.ReasonPlacementCapability, pool.Name, pool.Scope),
		poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: pool.Name, Scope: pool.Scope}),
	} {
		personal, site := siteDefaultRefusal(asPersonal, r), siteDefaultRefusal(pool, r)
		if personal != r {
			t.Errorf("%s: a personal default's refusal was changed", r.body.Reason)
		}
		if site.status != personal.status || site.body.Reason != personal.body.Reason {
			t.Errorf("%s: site default answers %d %s, the personal default %d %s", r.body.Reason, site.status, site.body.Reason, personal.status, personal.body.Reason)
		}
		if !strings.HasPrefix(site.body.Error, personal.body.Error) || !strings.Contains(site.body.Error, f.office.Name) ||
			!strings.Contains(site.body.Error, "Name a pool to run elsewhere.") {
			t.Errorf("%s: sentence %q", r.body.Reason, site.body.Error)
		}
	}
	boom := runServerError("list runner pool members", errors.New("connection reset"))
	if siteDefaultRefusal(pool, boom) != boom {
		t.Error("a server error was reworded")
	}
}

// An unreadable home-site map, pool site or site config refuses the create with a server error; a run is
// never placed as if its drives had no site.
func TestSiteStepStoreErrorRefuses(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		name string
		fail func(*siteStore)
	}{
		{"drive homes", func(s *siteStore) { s.homesErr = boom }},
		{"pool site", func(s *siteStore) { s.poolSiteErr = boom }},
		{"site config", func(s *siteStore) { s.configErr = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiteFixture(t)
			sitesOn(t, f)
			tc.fail(f.st)
			_, _, refusal := f.place(t, f.driveRun())
			if refusal == nil || !errors.Is(refusal.err, boom) || refusal.status != 0 {
				t.Fatalf("refusal = %+v, want a server error carrying the read's", refusal)
			}
		})
	}
}

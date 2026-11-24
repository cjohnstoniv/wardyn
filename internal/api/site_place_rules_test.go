// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sitePlaceJudgement is a pooled local create-time judgement of a request of kind in pool, with the organisation's
// machine-kind denials and the given classes among the pool's runners.
func sitePlaceJudgement(t *testing.T, kind string, denials []types.MachineKindDenial, pool types.ResolvedRunnerPool, trusts ...types.RunnerTrustClass) (localJudgement, *runRefusal) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	f.st.siteConfig = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: denials}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	return f.srv.localPlacementRefusal(r, createRunRequest{Placement: placement.Local, MachineKind: kind}, true, pool, trusts, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
}

// dispatchSiteStore is the dispatch fixture's store with one site's read: the name a refusal at that site says.
type dispatchSiteStore struct {
	localPersonRunnerStore
	store.OrgSiteStore
	site  types.OrgSite
	reads *int
}

func (d dispatchSiteStore) GetOrgSite(_ context.Context, id uuid.UUID) (types.OrgSite, error) {
	*d.reads++
	if id != d.site.ID {
		return types.OrgSite{}, store.ErrNotFound
	}
	return d.site, nil
}

// dispatchAtSite classifies a local Linux VM run placed at site (nil: none) and bound to a personal runner, with
// sites on and the given denials, and answers whether dispatch went on, the failure hint and the site reads.
func dispatchAtSite(t *testing.T, office types.OrgSite, site *uuid.UUID, denials ...types.MachineKindDenial) (bool, string, int) {
	t.Helper()
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	id := uuid.New()
	run.RunnerID, run.MachineKind, run.SiteID = &id, types.MachineLinuxVM, site
	ls.run = run
	reads := 0
	st := dispatchSiteStore{localPersonRunnerStore: localPersonRunnerStore{localDispatchStore: ls, row: types.Runner{ID: id, TrustClass: types.RunnerTrustPersonal, OwnerKind: types.RunnerOwnerPerson}}, site: office, reads: &reads}
	srv.cfg.Store = st
	on := true
	sc := types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: denials},
		FeatureSwitches: &types.SiteFeatureSwitches{OrgSites: &types.OrgSitesFeature{Enabled: &on}}}
	spec := localOrgNetworkSpec(run.ID)
	ok := srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), sc, &spec, nil, llmTransport{}, false)
	return ok, st.failureHint, reads
}

// An admin denies Linux VMs on personal runners at one site and leaves them at another. At create, a pool at the
// denied site with only personal runners is refused 403 placement_denied with a sentence that names the site, and
// a pool there that also holds a company-managed runner places the run on it; a pool at the other site, and a pool
// at no site, place the run on personal runners. At dispatch the run's recorded site is judged the same way.
func TestSitePlaceRuleDeniesKindAtOneSite(t *testing.T) {
	office := types.OrgSite{ID: uuid.New(), Name: "Leeds office", State: types.OrgSiteActive}
	branchID := uuid.New()
	deny := []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM, SiteID: office.ID.String()}}
	officePool := types.ResolvedRunnerPool{ID: uuid.New(), Name: "Office cluster", SiteID: &office.ID, SiteName: office.Name}
	branchPool := types.ResolvedRunnerPool{ID: uuid.New(), Name: "Branch cluster", SiteID: &branchID, SiteName: "Branch"}

	const sentence = "Your organisation does not allow Linux VM sandboxes on personal runners at Leeds office."
	_, ref := sitePlaceJudgement(t, "linux_vm", deny, officePool, types.RunnerTrustPersonal)
	if ref == nil || ref.status != http.StatusForbidden || ref.body.Reason != string(placement.ReasonPlacementDenied) || ref.body.Error != sentence {
		t.Fatalf("personal only at the denied site: %+v, want 403 placement_denied saying %q", ref, sentence)
	}
	if j, ref := sitePlaceJudgement(t, "linux_vm", deny, officePool, types.RunnerTrustPersonal, types.RunnerTrustManagedDevice); ref != nil || j.trust != types.RunnerTrustManagedDevice {
		t.Errorf("personal and managed_device at the denied site: %+v trust %q, want admitted at managed_device", ref, j.trust)
	}
	for name, pool := range map[string]types.ResolvedRunnerPool{"the other site": branchPool, "no site": {ID: uuid.New(), Name: "Shared"}} {
		if j, ref := sitePlaceJudgement(t, "linux_vm", deny, pool, types.RunnerTrustPersonal); ref != nil || j.trust != types.RunnerTrustPersonal {
			t.Errorf("personal only at %s: %+v trust %q, want admitted at personal", name, ref, j.trust)
		}
	}
	if j, ref := sitePlaceJudgement(t, "", deny, officePool, types.RunnerTrustPersonal); ref != nil || j.trust != types.RunnerTrustPersonal {
		t.Errorf("a container at the denied site: %+v trust %q, want admitted", ref, j.trust)
	}

	ok, hint, reads := dispatchAtSite(t, office, &office.ID, deny...)
	if ok || !strings.Contains(hint, sentence) || reads != 1 {
		t.Fatalf("dispatch at the denied site: ok %v hint %q reads %d, want refused saying %q after one site read", ok, hint, reads, sentence)
	}
	if ok, hint, _ := dispatchAtSite(t, office, &branchID, deny...); !ok {
		t.Errorf("dispatch at the other site was refused: %q", hint)
	}
	if ok, hint, _ := dispatchAtSite(t, office, nil, deny...); !ok {
		t.Errorf("dispatch at no site was refused: %q", hint)
	}
	// A list with no entry keyed to the run's site reads no site.
	if ok, _, reads := dispatchAtSite(t, office, &branchID); !ok || reads != 0 {
		t.Errorf("dispatch with no keyed entry: ok %v, %d site reads, want dispatched with none", ok, reads)
	}
}

// A denial keyed to a site needs a live site when it is written: an unknown or deleted one is refused, a store with
// no sites refuses any, and a read that fails is returned.
func TestMachineKindDenialSiteRefusal(t *testing.T) {
	f := newSiteFixture(t)
	deleted := types.OrgSite{ID: uuid.New(), Name: "Old office", State: types.OrgSiteDeleted}
	f.st.sites[deleted.ID] = deleted
	keyed := func(site string) []types.MachineKindDenial {
		return []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineContainer}, {Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM, SiteID: site}}
	}
	ctx := context.Background()
	if msg, err := f.srv.machineKindDenialSiteRefusal(ctx, keyed(f.office.ID.String())); msg != "" || err != nil {
		t.Errorf("a live site: %q %v, want accepted", msg, err)
	}
	for name, site := range map[string]string{"unknown": uuid.NewString(), "deleted": deleted.ID.String()} {
		if msg, err := f.srv.machineKindDenialSiteRefusal(ctx, keyed(site)); err != nil || !strings.Contains(msg, "machine_kind_denials[1]: site_id "+site+" is not a live site") {
			t.Errorf("%s site: %q %v, want refused naming entry 1", name, msg, err)
		}
	}
	f.st.siteErr = errors.New("read failed")
	if _, err := f.srv.machineKindDenialSiteRefusal(ctx, keyed(f.office.ID.String())); err == nil {
		t.Error("a failed site read was not returned")
	}
	if msg, err := f.srv.machineKindDenialSiteRefusal(ctx, keyed("")); msg != "" || err != nil {
		t.Errorf("no keyed entry read a site: %q %v", msg, err)
	}
	noSites := &Server{cfg: Config{Store: localPlanTestStore{}}}
	if msg, _ := noSites.machineKindDenialSiteRefusal(ctx, keyed(f.office.ID.String())); msg == "" {
		t.Error("a store with no sites accepted a keyed entry")
	}
}

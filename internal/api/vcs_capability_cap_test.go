// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The source-control capability caps: the boundary's (and a restriction's) allowlists of Azure DevOps and
// GitHub capabilities, met with each provider row's ceiling and enforced at dispatch, at every ask and
// decision, at every credential resolve and for git pushes.

// vcsLaneStore is the dispatch fixture's store holding a boundary.
type vcsLaneStore struct {
	*adoTestStore
	b    types.Boundary
	bErr error
}

func (s *vcsLaneStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, s.bErr }

func (s *vcsLaneStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// vcsCapStore, vcsPATStore and vcsOwnPATStore are the resolve fixtures' stores holding a boundary.
type vcsCapStore struct {
	*adoCapStore
	b types.Boundary
}

func (s *vcsCapStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *vcsCapStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

type vcsPATStore struct {
	*adoPATStore
	b types.Boundary
}

func (s *vcsPATStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *vcsPATStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

type vcsOwnPATStore struct {
	*adoSignInStore
	b types.Boundary
}

func (s *vcsOwnPATStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *vcsOwnPATStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

type vcsGHStore struct {
	*ghWallsStore
	b types.Boundary
}

func (s *vcsGHStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *vcsGHStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// capBoundary is a hard_limits boundary whose Azure DevOps cap is caps.
func capBoundary(caps ...adoscope.Capability) types.Boundary {
	return types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AzureDevOpsCapabilities: &caps}}
}

func ghCapBoundary(caps ...ghscope.Capability) types.Boundary {
	return types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{GitHubCapabilities: &caps}}
}

var (
	adoWrite, adoBuild = adoscope.CapCodeWrite, adoscope.CapBuildExecute
	adoPRParts         = adoCaps(adoscope.CapPRCreate, adoscope.CapPRVote, adoscope.CapPRComplete)
)

// A cap reads as written: an empty list or an ungrantable name is refused, pr is accepted and stays pr.
func TestBoundaryCapValidation(t *testing.T) {
	empty, emptyGH := []adoscope.Capability{}, []ghscope.Capability{}
	bad := []adoscope.Capability{"nonsense"}
	for name, tc := range map[string]struct {
		spec types.BoundarySpec
		want string
	}{
		"empty azure devops":     {types.BoundarySpec{AzureDevOpsCapabilities: &empty}, "azure_devops_capabilities: an empty cap allows no capability; omit the field for no cap"},
		"empty github":           {types.BoundarySpec{GitHubCapabilities: &emptyGH}, "github_capabilities: an empty cap allows no capability; omit the field for no cap"},
		"ungrantable azure":      {types.BoundarySpec{AzureDevOpsCapabilities: &bad}, `azure_devops_capabilities[0]: "nonsense" is not a grantable capability`},
		"ungrantable github":     {types.BoundarySpec{GitHubCapabilities: &[]ghscope.Capability{"nonsense"}}, `github_capabilities[0]: "nonsense" is not a grantable capability`},
		"second entry ungranted": {types.BoundarySpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead, "nonsense"}}, `azure_devops_capabilities[1]: "nonsense"`},
	} {
		err := validateBoundary(&types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: tc.spec})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, tc.want)
		}
	}
	pr := []adoscope.Capability{adoscope.CapPR}
	w := types.BoundaryWrite{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AzureDevOpsCapabilities: &pr, GitHubCapabilities: &[]ghscope.Capability{ghscope.CapPR}}}
	if err := validateBoundary(&w); err != nil {
		t.Fatalf("a cap naming pr: %v", err)
	}
	if got := *w.Spec.AzureDevOpsCapabilities; !slices.Equal(got, []adoscope.Capability{adoscope.CapPR}) {
		t.Errorf("stored cap = %v, want pr as written", got)
	}
}

// The helpers: a cap is the intersection of the layers that name one, pr reads as its parts, and a layer
// that names none leaves the rest.
func TestVCSCapHelpers(t *testing.T) {
	layer := func(caps ...adoscope.Capability) sealLayer { return sealLayer{adoCap: &caps} }
	if adoCapOf(nil) != nil || adoCapOf([]sealLayer{{}, {}}) != nil {
		t.Error("layers naming no cap gave a cap")
	}
	if got := adoCapOf([]sealLayer{layer(adoRead, adoscope.CapPR)}); got == nil || !slices.Equal(*got, append(adoCaps(adoRead), adoPRParts...)) {
		t.Errorf("one layer = %v, want pr expanded", got)
	}
	if got := adoCapOf([]sealLayer{layer(adoRead, adoWrite), {}, layer(adoWrite, adoBuild)}); got == nil || !slices.Equal(*got, adoCaps(adoWrite)) {
		t.Errorf("two layers = %v, want the intersection [code_write]", got)
	}
	if got := adoCapOf([]sealLayer{layer(adoRead), layer(adoBuild)}); got == nil || len(*got) != 0 {
		t.Errorf("disjoint layers = %v, want an empty non-nil cap", got)
	}
	ghLayer := sealLayer{githubCap: &[]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPR}}
	if got := githubCapOf([]sealLayer{ghLayer}); got == nil || !slices.Equal(*got, ghscope.ExpandPR([]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPR})) {
		t.Errorf("github cap = %v, want pr expanded", got)
	}

	row := &types.ADOEntraConfig{CapabilityCeiling: adoCaps(adoRead, adoscope.CapPR)}
	if got := effectiveADOCeiling(row, nil); !slices.Equal(got, row.Ceiling()) {
		t.Errorf("no cap: ceiling = %v, want the row's %v", got, row.Ceiling())
	}
	capped := adoCapOf([]sealLayer{layer(adoscope.CapPRCreate)})
	if got := effectiveADOCeiling(row, capped); !slices.Equal(got, adoCaps(adoscope.CapPRCreate)) {
		t.Errorf("cap [pr_create] on [code_read pr]: ceiling = %v, want [pr_create]", got)
	}
	whole := adoCaps(adoRead, adoWrite)
	if got := withinADOCap(whole, nil); !slices.Equal(got, whole) {
		t.Errorf("withinADOCap with no cap = %v", got)
	}
	if got := withinADOCap(whole, &[]adoscope.Capability{adoWrite}); !slices.Equal(got, adoCaps(adoWrite)) {
		t.Errorf("withinADOCap = %v, want [code_write]", got)
	}
	if got := withinGitHubCap([]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}, &[]ghscope.Capability{ghscope.CapCodeRead}); !slices.Equal(got, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Errorf("withinGitHubCap = %v", got)
	}
	if capLacks[adoscope.Capability](nil, adoWrite) || !capLacks(&[]adoscope.Capability{adoRead}, adoWrite) || capLacks(&[]adoscope.Capability{adoWrite}, adoWrite) {
		t.Error("capLacks: nil is no cap, a list without the capability lacks it, a list with it does not")
	}
}

// vcsLaneResult is one dispatch of the Azure DevOps lane.
type vcsLaneResult struct {
	lane   adoEntraLane
	ok     bool
	policy types.RunPolicySpec
	f      *adoFixture
}

func runVCSLane(t *testing.T, b types.Boundary, bErr error, row types.GitProvider, policy types.RunPolicySpec,
	standing adoStandingBound, mutRun func(*types.AgentRun),
) vcsLaneResult {
	t.Helper()
	f := newADOFixture(t)
	st := &vcsLaneStore{adoTestStore: &adoTestStore{site: adoSite(row)}, b: b, bErr: bErr}
	f.srv.cfg.Store = st
	ado, on := resolveADOEntraRun(st.site, nil, adoTestRepo, adoTestOwner)
	if !on {
		t.Fatal("the fixture row resolved no lane")
	}
	run := types.AgentRun{ID: uuid.New()}
	if mutRun != nil {
		mutRun(&run)
	}
	res := vcsLaneResult{f: f, policy: policy}
	res.lane, res.ok = f.srv.authorADOEntraLane(context.Background(), run, ado, on, adoEntraUngraded(), standing,
		dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &res.policy, map[string]string{}, nil)
	return res
}

// refusal is the reason on the dispatch's refusal row.
func (r vcsLaneResult) refusal(t *testing.T) string {
	t.Helper()
	rows := r.f.audit.find("run.create")
	if len(rows) == 0 {
		t.Fatal("no run.create refusal row")
	}
	var d map[string]any
	_ = json.Unmarshal(rows[len(rows)-1].Data, &d)
	s, _ := d["reason"].(string)
	return s
}

func vcsRow(ceiling, profile []adoscope.Capability) types.GitProvider {
	row := adoEntraTestRow()
	row.Entra.CapabilityCeiling, row.Entra.DefaultProfile = ceiling, profile
	return row
}

func TestBoundaryCapNarrowsDefaultProfileRun(t *testing.T) {
	all := adoCaps(adoRead, adoWrite, adoscope.CapPRCreate, adoBuild)
	res := runVCSLane(t, capBoundary(adoRead, adoWrite, adoscope.CapPR), nil, vcsRow(all, all), types.RunPolicySpec{}, adoStandingBound{}, nil)
	want := adoCaps(adoRead, adoWrite, adoscope.CapPRCreate)
	if !res.ok || res.lane.gate == nil || !slices.Equal(res.lane.gate.Capabilities, want) {
		t.Fatalf("ok %v gate %+v, want the run to hold %v", res.ok, res.lane.gate, want)
	}
	if !slices.Equal(res.policy.AzureDevOpsCapabilities, want) {
		t.Errorf("the run policy records %v, want the bounded list %v", res.policy.AzureDevOpsCapabilities, want)
	}
}

func TestBoundaryCapNoLimitsBindsNothing(t *testing.T) {
	b := capBoundary(adoRead)
	b.Mode = types.BoundaryNoLimits
	all := adoCaps(adoRead, adoWrite)
	res := runVCSLane(t, b, nil, vcsRow(all, all), types.RunPolicySpec{}, adoStandingBound{}, nil)
	if !res.ok || res.lane.gate == nil || !slices.Equal(res.lane.gate.Capabilities, all) {
		t.Fatalf("ok %v gate %+v, want the profile untouched under no_limits", res.ok, res.lane.gate)
	}
}

func TestBoundaryCapLeavesNothingRefused(t *testing.T) {
	all := adoCaps(adoRead, adoWrite)
	res := runVCSLane(t, capBoundary(adoBuild), nil, vcsRow(adoCaps(adoRead, adoWrite, adoBuild), all), types.RunPolicySpec{}, adoStandingBound{}, nil)
	if res.ok || res.refusal(t) != reasonADOCapabilitiesCapped {
		t.Fatalf("ok %v reason %q, want a refusal with %s", res.ok, res.refusal(t), reasonADOCapabilitiesCapped)
	}
}

// A policy's list is judged before it is narrowed: one past the cap refuses, as one past the ceiling does.
func TestBoundaryCapPolicyListOutsideRefused(t *testing.T) {
	row := vcsRow(adoCaps(adoRead, adoWrite, adoBuild), adoCaps(adoRead, adoWrite))
	b := capBoundary(adoRead, adoWrite)
	res := runVCSLane(t, b, nil, row, types.RunPolicySpec{AzureDevOpsCapabilities: adoCaps(adoRead, adoBuild)}, adoStandingBound{operator: true}, nil)
	if res.ok || res.refusal(t) != reasonADOCapabilitiesCapped {
		t.Fatalf("ok %v reason %q, want %s", res.ok, res.refusal(t), reasonADOCapabilitiesCapped)
	}
	inside := runVCSLane(t, b, nil, row, types.RunPolicySpec{AzureDevOpsCapabilities: adoCaps(adoRead)}, adoStandingBound{operator: true}, nil)
	if !inside.ok {
		t.Fatalf("a list inside the cap was refused: %q", inside.refusal(t))
	}
}

// An operator's own pick and a member's governance standing are not bounded by the row's profile, so only
// a cap applied after them binds either.
func TestBoundaryCapBindsOperatorPick(t *testing.T) {
	row := vcsRow(adoCaps(adoRead, adoWrite, adoBuild), adoCaps(adoRead, adoWrite))
	pick := types.RunPolicySpec{AzureDevOpsCapabilities: adoCaps(adoRead, adoBuild)}
	open := runVCSLane(t, types.Boundary{Mode: types.BoundaryNoLimits}, nil, row, pick, adoStandingBound{operator: true}, nil)
	if !open.ok || !slices.Equal(open.lane.gate.Capabilities, adoCaps(adoRead, adoBuild)) {
		t.Fatalf("without a cap: ok %v gate %+v, want the pick", open.ok, open.lane.gate)
	}
	capped := runVCSLane(t, capBoundary(adoRead, adoWrite), nil, row, pick, adoStandingBound{operator: true}, nil)
	if capped.ok || capped.refusal(t) != reasonADOCapabilitiesCapped {
		t.Fatalf("with the cap: ok %v reason %q, want %s", capped.ok, capped.refusal(t), reasonADOCapabilitiesCapped)
	}
}

func TestBoundaryCapBindsGovernanceStanding(t *testing.T) {
	row := vcsRow(adoCaps(adoRead, adoWrite, adoBuild), adoCaps(adoRead, adoWrite))
	pick := types.RunPolicySpec{AzureDevOpsCapabilities: adoCaps(adoRead, adoBuild)}
	member := adoStandingBound{governance: adoCaps(adoBuild)}
	open := runVCSLane(t, types.Boundary{Mode: types.BoundaryNoLimits}, nil, row, pick, member, nil)
	if !open.ok || !slices.Equal(open.lane.gate.Capabilities, adoCaps(adoRead, adoBuild)) {
		t.Fatalf("without a cap: ok %v gate %+v, want the standing grant held", open.ok, open.lane.gate)
	}
	capped := runVCSLane(t, capBoundary(adoRead, adoWrite), nil, row, pick, member, nil)
	if capped.ok || capped.refusal(t) != reasonADOCapabilitiesCapped {
		t.Fatalf("with the cap: ok %v reason %q, want %s", capped.ok, capped.refusal(t), reasonADOCapabilitiesCapped)
	}
}

// An unreadable boundary is no capability, as it is no egress.
func TestBoundaryCapUnreadBoundaryRefusesDispatch(t *testing.T) {
	all := adoCaps(adoRead, adoWrite)
	res := runVCSLane(t, types.Boundary{}, errors.New("boundary unreadable"), vcsRow(all, all), types.RunPolicySpec{}, adoStandingBound{}, nil)
	if res.ok || res.refusal(t) != reasonADOCapabilitiesCapped {
		t.Fatalf("ok %v reason %q, want %s", res.ok, res.refusal(t), reasonADOCapabilitiesCapped)
	}
}

// The admin token's run (an operator's, with no restriction) is bound by the boundary all the same.
func TestBoundaryCapBreakGlassBound(t *testing.T) {
	all := adoCaps(adoRead, adoWrite)
	res := runVCSLane(t, capBoundary(adoRead), nil, vcsRow(all, all), types.RunPolicySpec{}, adoStandingBound{operator: true},
		func(r *types.AgentRun) { r.OperatorOwned = true })
	if !res.ok || !slices.Equal(res.lane.gate.Capabilities, adoCaps(adoRead)) {
		t.Fatalf("ok %v gate %+v, want the operator's run narrowed to [code_read]", res.ok, res.lane.gate)
	}
}

// swapCapStore puts the boundary in front of an ask fixture's store.
func swapCapStore(f *adoCapFixture, b types.Boundary) {
	f.srv.cfg.Store = &vcsCapStore{adoCapStore: f.srv.cfg.Store.(*adoCapStore), b: b}
}

// Inside the row's ceiling and outside the cap, an ask is refused and raises no approval.
func TestBoundaryCapAskRefused(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoCreate, adoVote), adoCaps(adoRead, adoCreate), nil)
	swapCapStore(f, capBoundary(adoRead, adoCreate))
	w := f.ask(t, adoVote, types.FirstUseWaitForReview, uuid.Nil, prPath+"/7/reviewers/a1")
	if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != reasonCapabilityCapped {
		t.Fatalf("status %d body %s, want 403 %s", w.Code, w.Body.String(), reasonCapabilityCapped)
	}
	if len(f.approvals.requested) != 0 {
		t.Errorf("rows raised = %d, want none", len(f.approvals.requested))
	}
	if got := granted(t, f.ask(t, adoCreate, types.FirstUseWaitForReview, uuid.Nil, prPath)); !adoscope.HasName(got, adoCreate) {
		t.Errorf("pr_create inside the cap answered %v, want it standing", got)
	}
}

// An escalation raised before the cap was saved cannot be approved once the cap leaves its capability out.
func TestBoundaryCapDecideRefused(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoCreate, adoVote), adoCaps(adoRead, adoCreate), nil)
	id := pendingID(t, f.ask(t, adoVote, types.FirstUseWaitForReview, uuid.Nil, prPath+"/7/reviewers/a1"), adoCapabilityPendingState)
	ap := f.row(id)
	decide := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		_, isADO, ok := f.srv.adoDecisionRule(w, httptest.NewRequest(http.MethodPost, "/", nil), ap, types.ScopeOnce, true)
		if !isADO {
			t.Fatal("the escalation was not read as an Azure DevOps one")
		}
		if ok != (w.Code == http.StatusOK) {
			t.Fatalf("ok %v with status %d", ok, w.Code)
		}
		return w
	}
	if w := decide(); w.Code != http.StatusOK {
		t.Fatalf("before the cap: status %d %s, want the approval admitted", w.Code, w.Body.String())
	}
	swapCapStore(f, capBoundary(adoRead, adoCreate))
	if w := decide(); w.Code != http.StatusForbidden || wireReason(t, w) != reasonADOAccessAboveCeiling {
		t.Fatalf("after the cap: status %d %s, want 403 %s", w.Code, w.Body.String(), reasonADOAccessAboveCeiling)
	}
	if f.srv.adoEscalationWithinCeiling(context.Background(), f.runID, adoCapabilityScope{ProviderID: f.st.site.WorkspaceProviders.Git[0].ID, Capability: string(adoVote)}) {
		t.Error("the decide door admits a capability the cap leaves out")
	}
}

// A cap saved after dispatch that leaves out a capability the run holds stops its Azure DevOps access at
// the next resolve.
func TestBoundaryCapResolveStopsRunningRun(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoWrite, adoCreate), adoCaps(adoRead, adoWrite), nil)
	if w := f.resolve(t, f.subject, "dev.azure.com"); w.Code != http.StatusOK {
		t.Fatalf("before the cap: status %d %s", w.Code, w.Body.String())
	}
	f.audit.rows = nil
	swapCapStore(f, capBoundary(adoRead))
	w := f.resolve(t, f.subject, "dev.azure.com")
	if w.Code != http.StatusForbidden || f.failureReason(t)["reason"] != reasonCapabilityCapped {
		t.Fatalf("after the cap: status %d %s, want 403 %s", w.Code, w.Body.String(), reasonCapabilityCapped)
	}
}

// A minted_pat run's tokens are revoked at the resolve that refuses it, as on drift.
func TestBoundaryCapResolveRevokesMintedPATs(t *testing.T) {
	fx := newADOPATFixture(t)
	first := fx.ok(t, "dev.azure.com", nil)
	fx.srv.cfg.Store = &vcsPATStore{adoPATStore: fx.st, b: capBoundary(adoRead)}
	w, _ := fx.resolve(t, "dev.azure.com", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonCapabilityCapped) {
		t.Fatalf("status %d body %s, want 403 %s", w.Code, w.Body.String(), reasonCapabilityCapped)
	}
	if got := fx.pats.revokedIDs(); !slices.Equal(got, []string{first.JTI}) {
		t.Errorf("revoked %v, want the run's token", got)
	}
	if live := fx.st.unrevoked(t); len(live) != 0 {
		t.Errorf("live rows = %+v, want none", live)
	}
}

// The own-token lane has the same resolve door.
func TestBoundaryCapOwnPATResolveRefused(t *testing.T) {
	f := newOwnPATRun(t)
	f.srv.cfg.Store = &vcsOwnPATStore{adoSignInStore: f.st, b: capBoundary(adoscope.CapWikiRead)}
	wantRefused(t, f.resolve(t, capSub, "dev.azure.com", ""), http.StatusForbidden, reasonCapabilityCapped)
}

// A resolve whose run or layers cannot be read is not answered from a guess.
func TestBoundaryCapResolveUnreadableRun(t *testing.T) {
	f := newADOCapFixtureRow(t, adoCaps(adoRead, adoWrite), adoCaps(adoRead, adoWrite), nil)
	swapCapStore(f, capBoundary(adoRead, adoWrite))
	f.srv.cfg.Store = &vcsNoRunStore{f.srv.cfg.Store.(*vcsCapStore)}
	w := f.resolve(t, f.subject, "dev.azure.com")
	if w.Code != http.StatusServiceUnavailable || f.failureReason(t)["reason"] != reasonRunUnreadable {
		t.Fatalf("status %d %s, want 503 %s", w.Code, w.Body.String(), reasonRunUnreadable)
	}
}

type vcsNoRunStore struct{ *vcsCapStore }

func (*vcsNoRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, errors.New("run unreadable")
}

// githubCLICapFixture is githubCLIWallsFixture with a boundary in front of the store.
func githubCLICapFixture(t *testing.T, b types.Boundary) (*componentFixture, map[string]any) {
	t.Helper()
	cs := &capStore{}
	h := newHarness(t)
	st := &componentTestStore{govEscapeStore: newGovEscapeStore(cs), snapshots: map[uuid.UUID][]types.RunComponent{}}
	vst := &vcsGHStore{ghWallsStore: &ghWallsStore{componentTestStore: st, prefs: map[string]json.RawMessage{}}, b: b}
	rec := &recRecorder{}
	cfg := baseTestConfig(h, vst)
	cfg.Audit, cfg.Broker, cfg.Runner = rec, h.broker, &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)}}
	f := &componentFixture{srv: New(cfg), st: st, cs: cs, rec: rec}
	ref := f.org(ghCLIRowID, types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub,
		GitHub: &types.CLIGitHub{Capabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}}}})
	return f, ref
}

func TestBoundaryCapGitHubCLINarrowed(t *testing.T) {
	f, ref := githubCLICapFixture(t, ghCapBoundary(ghscope.CapCodeRead))
	fr := f.srv.cfg.Runner.(*fakeRunner)
	body := componentBody(ref)
	body["repo"] = "acme/app"
	if w := f.ask(t, componentDoors[0], body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	door := fr.lastSpec.ProxyConfig.GitHubAPI
	fr.mu.Unlock()
	if door == nil || !slices.Equal(door.Capabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Fatalf("GitHubAPI = %+v, want the door with code_read alone", door)
	}
}

func TestBoundaryCapGitHubCLINothingRefused(t *testing.T) {
	f, ref := githubCLICapFixture(t, ghCapBoundary(ghscope.CapIssuesWrite))
	body := componentBody(ref)
	body["repo"] = "acme/app"
	w := f.ask(t, componentDoors[0], body)
	if got := decodeErrorBody(t, w); w.Code != http.StatusForbidden || got.Reason != reasonGitHubCapabilitiesCapped {
		t.Fatalf("create = %d %+v, want 403 %s", w.Code, got, reasonGitHubCapabilitiesCapped)
	}
}

// A cap that leaves out code_write withholds git pushes on the forge it names; a cap that keeps it, or no
// cap, withholds nothing.
func TestBoundaryCapWithoutCodeWriteWithholdsPush(t *testing.T) {
	ghRead, ghWrite := []ghscope.Capability{ghscope.CapCodeRead}, []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}
	for name, tc := range map[string]struct {
		b    types.Boundary
		want gitPushWithheld
	}{
		"both without code_write": {types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{
			AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead}, GitHubCapabilities: &ghRead}}, gitPushWithheld{github: true, ado: true}},
		"github only":       {ghCapBoundary(ghRead...), gitPushWithheld{github: true}},
		"azure devops only": {capBoundary(adoRead), gitPushWithheld{ado: true}},
		"code_write kept": {types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{
			AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead, adoWrite}, GitHubCapabilities: &ghWrite}}, gitPushWithheld{}},
		"no_limits": {types.Boundary{Mode: types.BoundaryNoLimits, Spec: types.BoundarySpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead}}}, gitPushWithheld{}},
		"no cap":    {types.Boundary{Mode: types.BoundaryHardLimits}, gitPushWithheld{}},
	} {
		srv := &Server{cfg: Config{Store: &vcsLaneStore{adoTestStore: &adoTestStore{}, b: tc.b}}}
		got, refusal := srv.gitPushWithheldFor(context.Background(), types.AgentRun{ID: uuid.New()}, types.RunPolicySpec{})
		if refusal != nil || got != tc.want {
			t.Errorf("%s: withheld %+v (refused %v), want %+v", name, got, refusal, tc.want)
		}
	}
}

// The dry run lists the ceiling and default profile entries the drafted cap would remove, per row.
func TestBoundaryDryRunProviderRows(t *testing.T) {
	narrowed := vcsRow(adoCaps(adoRead, adoWrite, adoscope.CapPR), adoCaps(adoRead, adoWrite, adoscope.CapPRCreate))
	untouched := vcsRow(adoCaps(adoRead), adoCaps(adoRead))
	untouched.ID, untouched.BaseURLs = "ado-row-2", []string{"https://dev.azure.com/fabrikam"}
	srv := &Server{cfg: Config{Store: &adoTestStore{site: adoSite(narrowed, untouched)}}}
	rows, err := srv.boundaryProviderRows(context.Background(), capBoundary(adoRead, adoscope.CapPRCreate))
	if err != nil {
		t.Fatal(err)
	}
	want := []boundaryProviderCap{{ID: narrowed.ID,
		CeilingRemoved:        adoCaps(adoWrite, adoscope.CapPRVote, adoscope.CapPRComplete),
		DefaultProfileRemoved: adoCaps(adoWrite)}}
	if !slices.EqualFunc(rows, want, func(a, b boundaryProviderCap) bool {
		return a.ID == b.ID && slices.Equal(a.CeilingRemoved, b.CeilingRemoved) && slices.Equal(a.DefaultProfileRemoved, b.DefaultProfileRemoved)
	}) {
		t.Errorf("provider rows = %+v, want %+v", rows, want)
	}
	if rows, err := srv.boundaryProviderRows(context.Background(), types.Boundary{Mode: types.BoundaryHardLimits}); err != nil || len(rows) != 0 {
		t.Errorf("a boundary with no cap lists %+v (err %v), want none", rows, err)
	}
}

// A restriction carries the same caps, read from the restrictions the run captured.

// A restriction's cap is the cap of its people's runs only; a layer for an unreadable restriction allows
// nothing.
func TestRestrictionCapNarrowsItsPeople(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := e.seedProfile("vc-u", "a.example")
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-vc-bound", 1, false)
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-vc-plain", 1, false)
	seedRestriction(t, e, "vc-cap", types.RestrictionSpec{
		AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead, adoscope.CapPR},
		GitHubCapabilities:      &[]ghscope.Capability{ghscope.CapCodeRead},
	}, types.RestrictionLimits{}, userMember("sub-vc-bound"))
	bound := treeCeiling(t, e, treeCtx("sub-vc-bound", nil, ""))
	layers := e.srv.restrictionSealLayers(context.Background(), types.AgentRun{ID: uuid.New(), RestrictionIDs: bound.RestrictionIDs})
	if got := adoCapOf(layers); got == nil || !slices.Equal(*got, append(adoCaps(adoRead), adoPRParts...)) {
		t.Errorf("restriction's Azure DevOps cap = %v, want [code_read pr_create pr_vote pr_complete]", got)
	}
	if got := githubCapOf(layers); got == nil || !slices.Equal(*got, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Errorf("restriction's GitHub cap = %v, want [code_read]", got)
	}
	plain := treeCeiling(t, e, treeCtx("sub-vc-plain", nil, ""))
	if len(plain.RestrictionIDs) != 0 || adoCapOf(e.srv.restrictionSealLayers(context.Background(), types.AgentRun{RestrictionIDs: plain.RestrictionIDs})) != nil {
		t.Error("a person outside the restriction is capped")
	}
	// A restriction the run captured but the store cannot read allows no capability.
	lone := &Server{cfg: Config{Store: &adoTestStore{}}}
	unread := lone.restrictionSealLayers(context.Background(), types.AgentRun{ID: uuid.New(), RestrictionIDs: []uuid.UUID{uuid.New()}})
	if got := adoCapOf(unread); got == nil || len(*got) != 0 {
		t.Errorf("unread restriction's cap = %v, want an empty non-nil cap", got)
	}
}

// Break-glass: the admin token's run captures no restriction, so its cap is lifted, and the boundary's is not.
func TestRestrictionCapLiftedByBreakGlass(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	seedRestriction(t, e, "vc-all", types.RestrictionSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead}},
		types.RestrictionLimits{}, types.RestrictionMember{SubjectType: types.CapabilitySubjectAll})
	b := testBoundary(types.BoundaryHardLimits)
	b.Spec.AzureDevOpsCapabilities = &[]adoscope.Capability{adoRead, adoWrite}
	putBoundary(t, e, b)
	c := treeCeiling(t, e, context.Background())
	if len(c.RestrictionIDs) != 0 {
		t.Fatalf("break-glass captured restrictions %v", c.RestrictionIDs)
	}
	layers := e.srv.outerLayers(context.Background(), types.AgentRun{ID: uuid.New(), RestrictionIDs: c.RestrictionIDs})
	if got := adoCapOf(layers); got == nil || !slices.Equal(*got, adoCaps(adoRead, adoWrite)) {
		t.Errorf("break-glass run's cap = %v, want the boundary's [code_read code_write] alone", got)
	}
}

func TestRestrictionCapValidation(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	for name, body := range map[string]string{
		"empty azure devops": `{"name":"x","spec":{"azure_devops_capabilities":[]},"limits":{},"members":[]}`,
		"empty github":       `{"name":"x","spec":{"github_capabilities":[]},"limits":{},"members":[]}`,
		"ungrantable":        `{"name":"x","spec":{"azure_devops_capabilities":["nonsense"]},"limits":{},"members":[]}`,
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonRestrictionInvalid {
			t.Errorf("%s: %d %s, want 422 %s", name, w.Code, w.Body, reasonRestrictionInvalid)
		}
	}
	good := `{"name":"ok","spec":{"azure_devops_capabilities":["code_read","pr"],"github_capabilities":["code_read"]},"limits":{},"members":[]}`
	if w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", good); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Errorf("a valid cap: %d %s, want it stored", w.Code, w.Body)
	}
}

// Replacing a restriction by one with a wider cap, or none, loosens it, so it is never exempt from a second human.
func TestRestrictionCapLoosenedIsNotAtLeastAsStrict(t *testing.T) {
	old := types.Restriction{Spec: types.RestrictionSpec{AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead}, GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead}}}
	same := types.RestrictionWrite{Spec: old.Spec}
	if !restrictionAtLeastAsStrict(old, same) {
		t.Error("the same caps are not at least as strict")
	}
	for name, spec := range map[string]types.RestrictionSpec{
		"no cap":           {},
		"wider azure cap":  {AzureDevOpsCapabilities: &[]adoscope.Capability{adoRead, adoWrite}, GitHubCapabilities: old.Spec.GitHubCapabilities},
		"wider github cap": {AzureDevOpsCapabilities: old.Spec.AzureDevOpsCapabilities, GitHubCapabilities: &[]ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite}},
		"dropped github":   {AzureDevOpsCapabilities: old.Spec.AzureDevOpsCapabilities},
	} {
		if restrictionAtLeastAsStrict(old, types.RestrictionWrite{Spec: spec}) {
			t.Errorf("%s: judged at least as strict", name)
		}
	}
	if !restrictionAtLeastAsStrict(types.Restriction{}, types.RestrictionWrite{Spec: old.Spec}) {
		t.Error("adding a cap to a restriction with none is stricter")
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Standing Azure DevOps capabilities come only from what an admin configured:
// a member's policy list stands where it meets the row's default_profile or
// their governance ADO list, whatever the policy source; an operator's stands
// inside the row's ceiling. These compose the create path's own resolution
// (denyUserRequest's ceiling, resolveRunPolicy's spec) with dispatch and the
// capability resolve.

type adoStandingCase struct {
	source     string // inline, saved, saved-unassigned or none
	operator   bool
	picked     []adoscope.Capability
	governance []adoscope.Capability
}

// adoStandingLaunch resolves a create and dispatches its lane on a row with the
// given default_profile and ceiling.
func adoStandingLaunch(t *testing.T, c adoStandingCase, defaults, ceiling []adoscope.Capability) (*adoCapFixture, adoEntraLane, bool) {
	t.Helper()
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, FirstUseApproval: types.FirstUseAlwaysDeny, AzureDevOpsCapabilities: c.picked}
	st := &memberBoundStore{policy: types.RunPolicy{ID: uuid.New(), Spec: spec}}
	if c.source != "saved-unassigned" {
		st.profile = &types.GovernanceProfile{ID: uuid.New(), Name: "ado", Ceiling: types.RunPolicySpec{
			MinConfinementClass: types.CC2, FirstUseApproval: types.FirstUseAlwaysDeny, AzureDevOpsCapabilities: c.governance}}
	}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	if st.profile == nil {
		cfg.DefaultPolicy.AzureDevOpsCapabilities = c.governance
	}
	srv := New(cfg)
	role := oidc.RoleUser
	if c.operator {
		role = oidc.RoleAdmin
	}
	ctx := operatorCtx("ado-member", "member@example.test", role)
	req := &createRunRequest{Agent: "claude-code"}
	switch c.source {
	case "inline":
		req.InlinePolicy = &spec
	case "saved", "saved-unassigned":
		req.PolicyID = &st.policy.ID
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	gov, denied := srv.denyUserRequest(w, r, *req)
	if denied {
		t.Fatalf("create denied: %d %s", w.Code, w.Body.String())
	}
	resolved, _, _, _, ok := srv.resolveRunPolicy(ctx, w, r, req, false)
	if !ok {
		t.Fatalf("resolve refused: %d %s", w.Code, w.Body.String())
	}

	f := newADOCapFixture(t)
	row := f.st.site.WorkspaceProviders.Git[0].Entra
	row.DefaultProfile, row.CapabilityCeiling = slices.Clone(defaults), slices.Clone(ceiling)
	f.st.grants = nil
	a, on := resolveADOEntraRun(f.st.site, []string{adoTestRepo}, f.subject)
	if !on {
		t.Fatal("provider did not resolve")
	}
	dc := ceilingForDispatch(gov, adoEntraUngraded(), bedrockCredUngraded())
	lane, ok := f.srv.authorADOEntraLane(context.Background(), types.AgentRun{ID: f.runID}, a, on, dc.adoEntra, dc.adoStanding,
		dispatchLLMPlan{mitmCACertPEM: "CERT", mitmCAKeyPEM: "KEY"}, &resolved, map[string]string{}, nil)
	if ok && len(f.st.grants) > 0 {
		f.grantID = f.st.grants[0].ID
	}
	// run.policy.resolve audits this policy as what the run may do: it must
	// name the standing set the gate enforces, not the list the member asked for.
	if ok && lane.gate != nil && len(c.picked) > 0 && !slices.Equal(resolved.AzureDevOpsCapabilities, lane.gate.Capabilities) {
		t.Errorf("audited policy capabilities = %v, gate = %v", resolved.AzureDevOpsCapabilities, lane.gate.Capabilities)
	}
	return f, lane, ok
}

// assertStanding checks the REST gate and every grant snapshot hold exactly want.
func assertStanding(t *testing.T, f *adoCapFixture, lane adoEntraLane, ok bool, want []adoscope.Capability) {
	t.Helper()
	if !ok || lane.gate == nil {
		t.Fatalf("dispatch refused (ok=%v)", ok)
	}
	if !slices.Equal(lane.gate.Capabilities, want) {
		t.Errorf("gate capabilities = %v, want %v", lane.gate.Capabilities, want)
	}
	for i, got := range grantSnapshotCaps(t, f.st) {
		if !slices.Equal(got, want) {
			t.Errorf("grant[%d] snapshot = %v, want %v", i, got, want)
		}
	}
}

var adoStandingSources = []string{"inline", "saved", "saved-unassigned"}

// P-1: a member's [read pr] under default [read] and no governance list stands
// only [read]; PR is refused under always_deny and raises a request under
// deny_with_review. With a governance list of [read pr] the admin granted PR,
// so it stands.
func TestADOStanding_MemberStandsOnlyWhatAnAdminGranted(t *testing.T) {
	r, pr := adoscope.CapRead, adoscope.CapPR
	both := []adoscope.Capability{r, pr}
	for _, source := range adoStandingSources {
		t.Run(source+"/no governance list, always_deny", func(t *testing.T) {
			f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: source, picked: both}, []adoscope.Capability{r}, both)
			assertStanding(t, f, lane, ok, []adoscope.Capability{r})
			if w := f.ask(t, pr, types.FirstUseAlwaysDeny, uuid.Nil, prPath); w.Code != http.StatusForbidden || len(f.approvals.requested) != 0 {
				t.Fatalf("PR status=%d raised=%d, want 403 with nothing raised", w.Code, len(f.approvals.requested))
			}
		})
		t.Run(source+"/no governance list, deny_with_review", func(t *testing.T) {
			f, _, _ := adoStandingLaunch(t, adoStandingCase{source: source, picked: both}, []adoscope.Capability{r}, both)
			w := f.ask(t, pr, types.FirstUseDenyWithReview, uuid.Nil, prPath)
			if w.Code != http.StatusForbidden || f.failureReasonOf(t)["reason"] != "capability_review" || len(f.approvals.requested) != 1 {
				t.Fatalf("PR status=%d body=%s raised=%d, want 403 capability_review with one request", w.Code, w.Body.String(), len(f.approvals.requested))
			}
		})
		t.Run(source+"/governance list grants PR", func(t *testing.T) {
			f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: source, picked: both, governance: both}, []adoscope.Capability{r}, both)
			assertStanding(t, f, lane, ok, both)
			if got := granted(t, f.ask(t, pr, types.FirstUseAlwaysDeny, uuid.Nil, prPath)); !slices.Contains(got, string(pr)) {
				t.Errorf("granted %v, want PR standing", got)
			}
		})
	}
}

// P-2: an explicit narrower choice is honoured whatever the governance list,
// and an explicit list that meets nothing permitted refuses the launch rather
// than falling back to the default.
func TestADOStanding_ExplicitChoiceIsNeverWidened(t *testing.T) {
	r, cw, pr := adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR
	for name, gov := range map[string][]adoscope.Capability{"unset": nil, "disjoint": {cw}} {
		for _, source := range adoStandingSources {
			t.Run(source+"/governance "+name, func(t *testing.T) {
				f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: source, picked: []adoscope.Capability{r}, governance: gov},
					[]adoscope.Capability{r, cw}, []adoscope.Capability{r, cw})
				assertStanding(t, f, lane, ok, []adoscope.Capability{r})
			})
		}
	}
	for _, source := range adoStandingSources {
		t.Run(source+"/nothing permitted", func(t *testing.T) {
			f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: source, picked: []adoscope.Capability{pr}},
				[]adoscope.Capability{r}, []adoscope.Capability{r, pr})
			if ok || lane.gate != nil || len(f.st.grants) != 0 {
				t.Fatalf("ok=%v gate=%+v grants=%d, want the launch refused with nothing authored", ok, lane.gate, len(f.st.grants))
			}
			rows := f.audit.find("run.create")
			if len(rows) != 1 {
				t.Fatalf("run.create audit rows = %d, want one refusal", len(rows))
			}
			for _, want := range []string{`"reason":"ado_capabilities_none_permitted"`, "Can't launch with this policy. It asks for “Open pull requests”"} {
				if !strings.Contains(string(rows[0].Data), want) {
					t.Errorf("refusal %s does not say %q", rows[0].Data, want)
				}
			}
		})
	}
}

// Unchanged: an absent choice keeps the row's default; an operator's list
// stands inside the ceiling, clamped by nothing else; the row ceiling refuses
// at dispatch; a tightened live ceiling refuses at resolve.
func TestADOStanding_Controls(t *testing.T) {
	r, pr := adoscope.CapRead, adoscope.CapPR
	both := []adoscope.Capability{r, pr}
	t.Run("member, no policy keeps the default", func(t *testing.T) {
		f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: "none", governance: both}, []adoscope.Capability{r}, both)
		assertStanding(t, f, lane, ok, []adoscope.Capability{r})
		if w := f.ask(t, pr, types.FirstUseAlwaysDeny, uuid.Nil, prPath); w.Code != http.StatusForbidden {
			t.Fatalf("unselected PR status=%d, want 403", w.Code)
		}
	})
	for _, source := range adoStandingSources {
		t.Run(source+"/operator stands the policy's list", func(t *testing.T) {
			f, lane, ok := adoStandingLaunch(t, adoStandingCase{source: source, operator: true, picked: both}, []adoscope.Capability{r}, both)
			assertStanding(t, f, lane, ok, both)
		})
	}
	t.Run("operator outside the row ceiling is refused", func(t *testing.T) {
		f, _, ok := adoStandingLaunch(t, adoStandingCase{source: "inline", operator: true, picked: both}, []adoscope.Capability{r}, []adoscope.Capability{r})
		if ok || len(f.st.grants) != 0 {
			t.Fatalf("outside ceiling accepted: ok=%v grants=%d", ok, len(f.st.grants))
		}
	})
	t.Run("member granted past the row ceiling is refused", func(t *testing.T) {
		f, _, ok := adoStandingLaunch(t, adoStandingCase{source: "inline", picked: both, governance: both}, []adoscope.Capability{r}, []adoscope.Capability{r})
		if ok || len(f.st.grants) != 0 {
			t.Fatalf("outside ceiling accepted: ok=%v grants=%d", ok, len(f.st.grants))
		}
	})
	t.Run("live ceiling tightened refuses the snapshot", func(t *testing.T) {
		f, _, ok := adoStandingLaunch(t, adoStandingCase{source: "inline", picked: both, governance: both}, []adoscope.Capability{r}, both)
		if !ok {
			t.Fatal("setup refused")
		}
		f.st.site.WorkspaceProviders.Git[0].Entra.CapabilityCeiling = []adoscope.Capability{r}
		if w := f.ask(t, pr, types.FirstUseAlwaysDeny, uuid.Nil, prPath); w.Code != http.StatusForbidden {
			t.Fatalf("tightened live ceiling status=%d, want 403", w.Code)
		}
	})
	t.Run("lanes that resolve their own ceiling carry the same bound", func(t *testing.T) {
		cfg := baseTestConfig(newHarness(t), &memberBoundStore{})
		cfg.OIDC = &oidc.Authenticator{}
		srv := New(cfg)
		for role, want := range map[string]bool{oidc.RoleAdmin: true, oidc.RoleUser: false} {
			dc, _, err := srv.resolveDispatchCeiling(operatorCtx("sub", "sub@example.test", role))
			if err != nil || dc.adoStanding.operator != want {
				t.Errorf("%s: operator=%v err=%v, want %v", role, dc.adoStanding.operator, err, want)
			}
		}
	})
}

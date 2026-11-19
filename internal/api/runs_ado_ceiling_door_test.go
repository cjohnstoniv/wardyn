// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The wire string the dry doors answer with when a run policy's Azure DevOps
// list reaches past the provider row's capability_ceiling. Spelled out so a
// rename of the constant cannot rename the contract.
const adoAboveCeilingReason = "ado_capabilities_above_ceiling"

// adoCeilingDoor posts a draft whose inline policy names caps to path as who:
// "member" (no governance list), "governed" (a governance list granting
// governance), or "operator" (an admin's own wdn_ token, which is bound by the
// ceiling alone; the admin token has no person and is off the lane, and an
// admin's session starts no run in the Admin view and is a member's in the User
// view). The row is
// adoEntraTestRow:
// ceiling [code_read code_write pr], default profile [code_read code_write].
func adoCeilingDoor(t *testing.T, who, path string, governance []adoscope.Capability, caps string) *httptest.ResponseRecorder {
	t.Helper()
	cs := &capStore{userTypes: utKnown}
	if who == "governed" {
		cs.govProfile = &types.GovernanceUnit{ID: uuid.New(), Name: "ado", Ceiling: types.RunPolicySpec{
			MinConfinementClass: types.CC2, AzureDevOpsCapabilities: governance}}
		cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
	}
	srv, st, _ := govEscapeFixture(t, cs)
	st.siteConfig = adoSite(adoEntraTestRow())
	sub := govMemberSub
	if who == "operator" {
		sub = uvAdminSub
		giveAdminToken(st, sub)
	}
	st.workspaces = []types.Workspace{{
		ID: uuid.New(), Name: "app", OwnedBy: sub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}},
	}}
	body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
		`"workspace_repos":[{"repo":` + quote(adoTestRepo) + `,"target":"/work/repo"}],"azure_devops_capabilities":` + caps + `}}`
	if path == policyPreviewPath {
		forbidPreviewSideEffects(t, srv)
	}
	if who == "operator" {
		return do(t, srv, http.MethodPost, path, st.tokenRaw, body)
	}
	return doSSO(t, srv, http.MethodPost, path, govSession(t, govMemberSub, []string{"eng"}, false), body)
}

// adoDispatchRefusal is what dispatch answers for the same inputs
// (authorADOEntraLane's withPolicyCapabilities, then authorADOEntraInjection's
// ceiling check on the bounded list): "" when the list passes.
func adoDispatchRefusal(picked, ceiling, profile, governance []adoscope.Capability, operator bool) string {
	lane := adoEntraRun{rowID: "ado-row-1", caps: profile, ceiling: ceiling}
	bounded, permitted := lane.withPolicyCapabilities(picked, adoStandingBound{operator: operator, governance: governance})
	if !permitted {
		return "ado_capabilities_none_permitted"
	}
	if bounded.capsFromPolicy && len(adoCapsOutside(bounded.caps, ceiling)) > 0 {
		return adoAboveCeilingReason
	}
	return ""
}

// Review and the policy preview refuse a list past the row's ceiling with the
// sentence dispatch gives, instead of accepting it and failing after the click.
func TestADOCeilingRefusedAtTheDryDoors(t *testing.T) {
	past := adoPolicyPastCeiling([]adoscope.Capability{adoscope.CapPolicyAdmin})
	dryDoors := []string{"/api/v1/runs/preflight", policyPreviewPath}
	wide := []adoscope.Capability{adoscope.CapPolicyAdmin}
	for _, tc := range []struct {
		name, who, caps string
		governance      []adoscope.Capability
		wantCode        int
		wantReason      string // "" = the list passes
	}{
		{"member whose governance list is wider than the ceiling picks outside it", "governed", `["code_read","policy_admin"]`, wide, 422, adoAboveCeilingReason},
		{"member picks only what the governance list grants past the ceiling", "governed", `["policy_admin"]`, wide, 422, adoAboveCeilingReason},
		{"member picks narrower than the ceiling", "governed", `["code_read"]`, wide, 200, ""},
		{"member picks inside the ceiling with a wide governance list", "governed", `["code_read","pr"]`, append(slices.Clone(wide), adoscope.CapPR), 200, ""},
		{"operator outside the ceiling", "operator", `["code_read","policy_admin"]`, nil, 422, adoAboveCeilingReason},
		{"operator inside the ceiling", "operator", `["code_read","pr"]`, nil, 200, ""},
		{"empty list is unchanged", "member", `[]`, nil, 200, ""},
		{"member with nothing standing keeps none_permitted first", "member", `["policy_admin"]`, nil, 422, "ado_capabilities_none_permitted"},
		{"member's outside pick is dropped before the ceiling is asked", "member", `["code_read","policy_admin"]`, nil, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range dryDoors {
				w := adoCeilingDoor(t, tc.who, path, tc.governance, tc.caps)
				if w.Code != tc.wantCode {
					t.Fatalf("%s = %d, want %d: %s", path, w.Code, tc.wantCode, w.Body.String())
				}
				if tc.wantReason == "" {
					if strings.Contains(w.Body.String(), adoAboveCeilingReason) || strings.Contains(w.Body.String(), "ado_capabilities_none_permitted") {
						t.Errorf("%s refused a list that passes: %s", path, w.Body.String())
					}
					continue
				}
				if !strings.Contains(w.Body.String(), `"reason":"`+tc.wantReason+`"`) {
					t.Errorf("%s body %s lacks reason %s", path, w.Body.String(), tc.wantReason)
				}
				if tc.wantReason == adoAboveCeilingReason && !strings.Contains(w.Body.String(), past) {
					t.Errorf("%s body %s lacks dispatch's sentence %q", path, w.Body.String(), past)
				}
			}
		})
	}
}

// Create is unchanged: it does not refuse on the ceiling (dispatch re-reads the
// live row and does), so no run row changes.
func TestADOCeilingNotRefusedAtCreate(t *testing.T) {
	w := adoCeilingDoor(t, "operator", "/api/v1/runs", nil, `["code_read","policy_admin"]`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// For every (picked, who) the dry door's answer equals dispatch's.
func TestADOCeilingDryDoorMatchesDispatch(t *testing.T) {
	row := adoEntraTestRow().Entra
	wide := []adoscope.Capability{adoscope.CapPolicyAdmin, adoscope.CapPR}
	picks := map[string][]adoscope.Capability{
		`["code_read"]`:                {adoscope.CapCodeRead},
		`["pr"]`:                       {adoscope.CapPR},
		`["policy_admin"]`:             {adoscope.CapPolicyAdmin},
		`["code_read","policy_admin"]`: {adoscope.CapCodeRead, adoscope.CapPolicyAdmin},
		`["code_read","pr"]`:           {adoscope.CapCodeRead, adoscope.CapPR},
	}
	for _, who := range []string{"member", "governed", "operator"} {
		var governance []adoscope.Capability
		if who == "governed" {
			governance = wide
		}
		for caps, picked := range picks {
			want := adoDispatchRefusal(picked, row.CapabilityCeiling, row.Profile(), governance, who == "operator")
			w := adoCeilingDoor(t, who, "/api/v1/runs/preflight", governance, caps)
			got := ""
			if w.Code == http.StatusUnprocessableEntity {
				for _, r := range []string{adoAboveCeilingReason, "ado_capabilities_none_permitted"} {
					if strings.Contains(w.Body.String(), `"reason":"`+r+`"`) {
						got = r
					}
				}
			} else if w.Code != http.StatusOK {
				t.Fatalf("%s %s = %d: %s", who, caps, w.Code, w.Body.String())
			}
			if got != want {
				t.Errorf("%s %s: preflight reason %q, dispatch %q", who, caps, got, want)
			}
		}
	}
}

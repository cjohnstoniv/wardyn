// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRestrictionKeepsDefaultMemberOutOfHoldLane: a restriction puts a Default-context person in the
// Default unit, which is no assigned profile, so a deployment default that holds a tool does not
// flip their runs into the supervised hold lane or refuse codex-cli.
func TestRestrictionKeepsDefaultMemberOutOfHoldLane(t *testing.T) {
	hold := func(c *Config) {
		p := c.DefaultPolicy.Clone()
		p.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}
		c.DefaultPolicy = p
	}
	e := newGovEnvOn(t, throwawayPGPool(t), hold)
	seedRestriction(t, e, "hl-all", types.RestrictionSpec{DeniedDomains: strs("a.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectAll})
	ctx := memberCtxFor("sub-hl")
	c := treeCeiling(t, e, ctx)
	if !restrictionDefaultUnit(c.Unit) || len(c.Spec.ToolRules) == 0 {
		t.Fatalf("setup: unit %v tool rules %v, want the restriction's Default unit over a holding default", c.Unit, c.Spec.ToolRules)
	}
	req := createRunRequest{Agent: "codex-cli", Task: "t"}
	if governanceHoldRules(c) || effectiveToolApprovals(req, c) == "hold" {
		t.Errorf("a person only a restriction binds is in the hold lane")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
	if ref := e.srv.runGovernanceSupervision(r, req, c); ref != nil {
		t.Errorf("codex-cli refused for a person only a restriction binds: %+v", ref.body)
	}
}

// TestRestrictionClampsStoredRow: a stored policy row selected by a person a restriction covers is
// clamped as a whole to the ceiling (the restriction's posture knobs hold on it), not only its additions.
func TestRestrictionClampsStoredRow(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	cc3 := types.CC3
	seedRestriction(t, e, "sr-all", types.RestrictionSpec{MinConfinementClass: &cc3}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectAll})
	row, err := e.pg.CreatePolicy(t.Context(), types.RunPolicy{Name: "sr-row", Spec: types.RunPolicySpec{MinConfinementClass: types.CC1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := memberCtxFor("sub-sr")
	req := createRunRequest{Agent: "claude-code", Repo: "acme/widgets", PolicyID: &row.ID}
	spec, _, _, _, ok := e.srv.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx), &req, true)
	if !ok {
		t.Fatal("resolve refused the request")
	}
	if spec.MinConfinementClass != types.CC3 {
		t.Errorf("stored row resolved to %s, want the restriction's CC3", spec.MinConfinementClass)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The fixed corpus the boundary tests resolve (boundary_pg_test.go). This file uses only APIs
// that predate the boundary, so the same corpus run on a tree without it wrote
// testdata/boundary_no_limits_golden.json: in no_limits, every path below must still resolve to it
// byte for byte.

const (
	boundaryMemberSub   = "sub-walled-member"
	boundaryUnassigned  = "sub-unassigned"
	boundarySuperSub    = "sub-boundary-super"
	boundaryOutsideHost = "outside.example"
)

// boundaryCorpus is the seeded rows: a profile wider than the boundary assigned to one member, and a
// stored policy that allows all egress.
type boundaryCorpus struct {
	profile types.GovernanceUnit
	open    types.RunPolicy
}

func seedBoundaryCorpus(t *testing.T, e *govEnv) boundaryCorpus {
	t.Helper()
	ctx := context.Background()
	p := e.seedProfile("wide-team", "api.anthropic.com", "pypi.org", boundaryOutsideHost, "evil.example")
	e.seedAssignment(p, boundaryMemberSub)
	open, err := e.pg.CreatePolicy(ctx, types.RunPolicy{Name: "open-egress", Spec: types.RunPolicySpec{
		AllowAllEgress: true, MinConfinementClass: types.CC1,
	}})
	if err != nil {
		t.Fatalf("seed stored policy: %v", err)
	}
	return boundaryCorpus{profile: p, open: open}
}

// boundaryCase is one principal and request: a policy path the boundary must bound.
type boundaryCase struct {
	name string
	ctx  context.Context
	req  func(c boundaryCorpus) createRunRequest
}

func boundaryCases() []boundaryCase {
	wide := func() *types.RunPolicySpec {
		return &types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com", boundaryOutsideHost}}
	}
	open := func() *types.RunPolicySpec {
		return &types.RunPolicySpec{MinConfinementClass: types.CC1, AllowAllEgress: true}
	}
	base := func() createRunRequest { return createRunRequest{Agent: "claude-code", Repo: "acme/widgets"} }
	member, unassigned := memberCtxFor(boundaryMemberSub), memberCtxFor(boundaryUnassigned)
	super := operatorCtx(boundarySuperSub, "boundary-super@corp.example", oidc.RoleAdmin)
	return []boundaryCase{
		{"profile_chain_default", member, func(boundaryCorpus) createRunRequest { return base() }},
		{"member_pipeline_inline", member, func(boundaryCorpus) createRunRequest { r := base(); r.InlinePolicy = wide(); return r }},
		{"stored_policy_unassigned", unassigned, func(c boundaryCorpus) createRunRequest { r := base(); r.PolicyID = &c.open.ID; return r }},
		{"deployment_default_unassigned", unassigned, func(boundaryCorpus) createRunRequest { return base() }},
		{"admin_run_inline", super, func(boundaryCorpus) createRunRequest { r := base(); r.InlinePolicy = open(); return r }},
		{"admin_run_stored", super, func(c boundaryCorpus) createRunRequest { r := base(); r.PolicyID = &c.open.ID; return r }},
		{"admin_token_inline", context.Background(), func(boundaryCorpus) createRunRequest { r := base(); r.InlinePolicy = open(); return r }},
	}
}

// boundaryResolved is what one case resolves to: its ceiling and its run's policy.
type boundaryResolved struct {
	Case            string                 `json:"case"`
	CeilingSpec     types.RunPolicySpec    `json:"ceiling_spec"`
	CeilingLimits   types.GovernanceLimits `json:"ceiling_limits"`
	CeilingWarnings []string               `json:"ceiling_warnings"`
	Spec            types.RunPolicySpec    `json:"spec"`
	Warnings        []string               `json:"warnings"`
	PolicyID        bool                   `json:"policy_id"`
	OK              bool                   `json:"ok"`
}

func resolveBoundaryCase(t *testing.T, srv *Server, c boundaryCorpus, wc boundaryCase) boundaryResolved {
	t.Helper()
	ceiling, err := srv.effectiveCeiling(wc.ctx)
	if err != nil {
		t.Fatalf("%s: effective ceiling: %v", wc.name, err)
	}
	req := wc.req(c)
	spec, id, warns, _, ok := srv.resolveRunPolicy(wc.ctx, httptest.NewRecorder(), governReq(wc.ctx), &req, true)
	return boundaryResolved{Case: wc.name, CeilingSpec: ceiling.Spec, CeilingLimits: ceiling.Limits, CeilingWarnings: ceiling.Warnings,
		Spec: spec, Warnings: warns, PolicyID: id != nil && *id != uuid.Nil, OK: ok}
}

func resolveBoundaryCorpus(t *testing.T, srv *Server, c boundaryCorpus) []boundaryResolved {
	t.Helper()
	var out []boundaryResolved
	for _, wc := range boundaryCases() {
		out = append(out, resolveBoundaryCase(t, srv, c, wc))
	}
	return out
}

// TestBoundaryNoLimitsGolden: with no boundary row the corpus resolves to the recorded behaviour.
// It uses only what predates the boundary, so it also runs on the tree that records the golden.
func TestBoundaryNoLimitsGolden(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedBoundaryCorpus(t, e)
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolveBoundaryCorpus(t, e.srv, c))
}

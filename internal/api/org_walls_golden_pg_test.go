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

// The fixed corpus the org walls tests resolve (org_walls_pg_test.go). This file uses only APIs
// that predate the walls, so the same corpus run on a tree without them wrote
// testdata/org_walls_off_golden.json: with the walls off, every path below must still resolve to it
// byte for byte.

const (
	wallsMemberSub   = "sub-walled-member"
	wallsUnassigned  = "sub-unassigned"
	wallsSuperSub    = "sub-walls-super"
	wallsOutsideHost = "outside.example"
)

// wallsCorpus is the seeded rows: a profile wider than the walls assigned to one member, and a
// stored policy that allows all egress.
type wallsCorpus struct {
	profile types.GovernanceProfile
	open    types.RunPolicy
}

func seedWallsCorpus(t *testing.T, e *govEnv) wallsCorpus {
	t.Helper()
	ctx := context.Background()
	p := e.seedProfile("wide-team", "api.anthropic.com", "pypi.org", wallsOutsideHost, "evil.example")
	e.seedAssignment(p, wallsMemberSub)
	open, err := e.pg.CreatePolicy(ctx, types.RunPolicy{Name: "open-egress", Spec: types.RunPolicySpec{
		AllowAllEgress: true, MinConfinementClass: types.CC1,
	}})
	if err != nil {
		t.Fatalf("seed stored policy: %v", err)
	}
	return wallsCorpus{profile: p, open: open}
}

// wallsCase is one principal and request: a policy path the walls must bound.
type wallsCase struct {
	name string
	ctx  context.Context
	req  func(c wallsCorpus) createRunRequest
}

func wallsCases() []wallsCase {
	wide := func() *types.RunPolicySpec {
		return &types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com", wallsOutsideHost}}
	}
	open := func() *types.RunPolicySpec {
		return &types.RunPolicySpec{MinConfinementClass: types.CC1, AllowAllEgress: true}
	}
	base := func() createRunRequest { return createRunRequest{Agent: "claude-code", Repo: "acme/widgets"} }
	member, unassigned := memberCtxFor(wallsMemberSub), memberCtxFor(wallsUnassigned)
	super := operatorCtx(wallsSuperSub, "walls-super@corp.example", oidc.RoleAdmin)
	return []wallsCase{
		{"profile_chain_default", member, func(wallsCorpus) createRunRequest { return base() }},
		{"member_pipeline_inline", member, func(wallsCorpus) createRunRequest { r := base(); r.InlinePolicy = wide(); return r }},
		{"stored_policy_unassigned", unassigned, func(c wallsCorpus) createRunRequest { r := base(); r.PolicyID = &c.open.ID; return r }},
		{"deployment_default_unassigned", unassigned, func(wallsCorpus) createRunRequest { return base() }},
		{"admin_run_inline", super, func(wallsCorpus) createRunRequest { r := base(); r.InlinePolicy = open(); return r }},
		{"admin_run_stored", super, func(c wallsCorpus) createRunRequest { r := base(); r.PolicyID = &c.open.ID; return r }},
		{"admin_token_inline", context.Background(), func(wallsCorpus) createRunRequest { r := base(); r.InlinePolicy = open(); return r }},
	}
}

// wallsResolved is what one case resolves to: its ceiling and its run's policy.
type wallsResolved struct {
	Case            string                 `json:"case"`
	CeilingSpec     types.RunPolicySpec    `json:"ceiling_spec"`
	CeilingLimits   types.GovernanceLimits `json:"ceiling_limits"`
	CeilingWarnings []string               `json:"ceiling_warnings"`
	Spec            types.RunPolicySpec    `json:"spec"`
	Warnings        []string               `json:"warnings"`
	PolicyID        bool                   `json:"policy_id"`
	OK              bool                   `json:"ok"`
}

func resolveWallsCase(t *testing.T, srv *Server, c wallsCorpus, wc wallsCase) wallsResolved {
	t.Helper()
	ceiling, err := srv.effectiveCeiling(wc.ctx)
	if err != nil {
		t.Fatalf("%s: effective ceiling: %v", wc.name, err)
	}
	req := wc.req(c)
	spec, id, warns, _, ok := srv.resolveRunPolicy(wc.ctx, httptest.NewRecorder(), governReq(wc.ctx), &req, true)
	return wallsResolved{Case: wc.name, CeilingSpec: ceiling.Spec, CeilingLimits: ceiling.Limits, CeilingWarnings: ceiling.Warnings,
		Spec: spec, Warnings: warns, PolicyID: id != nil && *id != uuid.Nil, OK: ok}
}

func resolveWallsCorpus(t *testing.T, srv *Server, c wallsCorpus) []wallsResolved {
	t.Helper()
	var out []wallsResolved
	for _, wc := range wallsCases() {
		out = append(out, resolveWallsCase(t, srv, c, wc))
	}
	return out
}

// TestOrgWallsOffGolden: with no walls row the corpus resolves to the recorded behaviour.
func TestOrgWallsOffGolden(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	c := seedWallsCorpus(t, e)
	compareOrUpdateGolden(t, "testdata/org_walls_off_golden.json", resolveWallsCorpus(t, e.srv, c))
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member is told the leaf profile's name and nothing about the profiles it is built on. These tests
// create a run as a member after a BASE edit has dropped something the overlay named, through the real
// create handler, and read back every member-facing surface the composition could leak through.

const (
	secretBaseName = "company-baseline-secret-name"
	secretBaseHost = "base-only.example"
)

// disclosureGraph is a base that now allows only api.anthropic.com, and a leaf whose overlay still
// names a host the base used to allow.
func disclosureGraph() (base, leaf types.GovernanceProfile) {
	spec := govProfileSpec()
	spec.AllowedDomains = []string{"api.anthropic.com"}
	base = types.GovernanceProfile{ID: uuid.New(), Name: secretBaseName, Ceiling: spec}
	leaf = types.GovernanceProfile{ID: uuid.New(), Name: "team", BaseProfileID: &base.ID, Overlay: &types.CeilingOverlay{
		AllowedDomains: &[]string{"api.anthropic.com", secretBaseHost}}}
	return base, leaf
}

func assertNoBase(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{secretBaseName, secretBaseHost} {
		if strings.Contains(text, secret) {
			t.Errorf("%s names the base (%q): %s", where, secret, text)
		}
	}
}

func TestComposedProfileDisclosesNothingOfItsBaseToAMember(t *testing.T) {
	base, leaf := disclosureGraph()
	cs := assignedStore(&leaf)
	cs.govGraph = []types.GovernanceProfile{base, leaf}
	srv, st := pvFixture(t, cs)
	st.profiles = cs.govGraph
	const owner = "sub-walled"
	member := govSession(t, owner, []string{"eng"}, false)

	// 201: the clamp warnings the create returns.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	assertNoBase(t, "the 201", w.Body.String())
	var created struct {
		ID       uuid.UUID `json:"id"`
		Warnings []string  `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	narrowed := false
	for _, warn := range created.Warnings {
		if strings.Contains(warn, `"team"`) && strings.Contains(warn, "narrowed") {
			narrowed = true
		}
	}
	if !narrowed {
		t.Fatalf("warnings = %q, want the leaf's own narrowing warning: the scenario did not fire, so the check above proves nothing", created.Warnings)
	}

	// GET /runs/{id}/policy, as the run's owner.
	waitForRecAudit(t, st.audit, created.ID, "run.policy.resolve", "success")
	pw := pvMember(t, owner)
	rec, _ := pw.get(t, srv, created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET policy = %d %s", rec.Code, rec.Body.String())
	}
	assertNoBase(t, "GET /runs/{id}/policy", rec.Body.String())

	// The member's own policy default.
	dw := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default", member, "")
	if dw.Code != http.StatusOK {
		t.Fatalf("GET /policies/default = %d %s", dw.Code, dw.Body.String())
	}
	assertNoBase(t, "GET /policies/default", dw.Body.String())
	if !strings.Contains(dw.Body.String(), `"team"`) {
		t.Errorf("GET /policies/default = %s, want the leaf's name", dw.Body.String())
	}

	// Every audit row the launch wrote is an admin surface, but a clamp warning copied into one is
	// the same text the member read.
	clamped := false
	for _, ev := range st.audit.snapshot() {
		if ev.Action == "run.create" {
			assertNoBase(t, "the run.create audit row", string(ev.Data))
			clamped = clamped || strings.Contains(string(ev.Data), `"clamp_warnings"`)
		}
	}
	if !clamped {
		t.Error("the run.create row carries no clamp_warnings, so the check above proves nothing")
	}
}

func TestComposedProfileLaunchRefusalNamesOnlyTheLeaf(t *testing.T) {
	baseSpec := govProfileSpec()
	baseSpec.AllowedMethods = []string{"GET"}
	base := types.GovernanceProfile{ID: uuid.New(), Name: secretBaseName, Ceiling: baseSpec}
	post := []string{"POST"}
	leaf := types.GovernanceProfile{ID: uuid.New(), Name: "team", BaseProfileID: &base.ID,
		Overlay: &types.CeilingOverlay{AllowedMethods: &post}}
	cs := assignedStore(&leaf)
	cs.govGraph = []types.GovernanceProfile{base, leaf}
	srv, _, audit := govEscapeFixture(t, cs)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code","task":"t"}`)
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Reason != string(authz.ReasonGovernanceOverlayUnsatisfiable) {
		t.Fatalf("create = %d %s, want 403 governance_overlay_unsatisfiable", w.Code, w.Body.String())
	}
	assertNoBase(t, "the launch refusal", w.Body.String())
	if !strings.Contains(body.Error, `"team"`) {
		t.Errorf("refusal = %s, want the member's own profile named", w.Body.String())
	}
	for _, ev := range audit.snapshot() {
		if ev.Action == authz.AuditAction {
			assertNoBase(t, "the authz.denied row", string(ev.Data))
		}
	}
	if !recHasRefusal(audit, authz.ReasonGovernanceOverlayUnsatisfiable, "governance.ceiling") {
		t.Error("the refusal was not audited")
	}
}

// A chain that cannot be read or composed fails the launch closed with the generic 500 and creates no
// run: an unreadable base, a base the read left out, a loop and a chain four deep.
func TestComposedProfileSeededBadChainsFailTheLaunchClosed(t *testing.T) {
	for _, sc := range seededBadChains() {
		t.Run(sc.name, func(t *testing.T) {
			base, leaf := composedPair(types.RunPolicySpec{}, types.GovernanceLimits{})
			graph := sc.profiles(base, leaf)
			// The leaf the resolver picks is the one in the graph that names the base the scenario seeded.
			i := slices.IndexFunc(graph, func(p types.GovernanceProfile) bool { return p.ID == leaf.ID })
			cs := assignedStore(&graph[i])
			cs.govGraph, cs.govChainErr = graph, sc.listErr
			srv, st, _ := govEscapeFixture(t, cs)
			for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
				w := doSSO(t, srv, http.MethodPost, path, govSession(t, "sub-walled", []string{"eng"}, false), `{"agent":"claude-code","task":"t"}`)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("POST %s with an unresolvable chain = %d %s, want a closed 500", path, w.Code, w.Body.String())
				}
				assertNoBase(t, path, w.Body.String())
			}
			st.mu.Lock()
			n := len(st.runs)
			st.mu.Unlock()
			if n != 0 {
				t.Errorf("%d run(s) were created under an unresolvable chain", n)
			}
		})
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The read sites of a mount's host path, against a real Postgres: R1-R3, R6 and R10 one by one, and
// the route census over every read surface (§1 of the design, and POST /runs/preflight). Guarded by
// WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

var pinNeedles = []string{pinMountSrc, pinMountSrcB, pinBaseSrc}

// seedStoredPolicy stores a policy carrying the seeded mount, as a super admin writes it.
func (e *govEnv) seedStoredPolicy(name string) uuid.UUID {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "spec": types.RunPolicySpec{MinConfinementClass: types.CC2,
		AllowedDomains: []string{"api.anthropic.com"}, WorkspaceMounts: pinMounts()}})
	w := e.admin(http.MethodPost, "/api/v1/policies", string(b))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("seed policy = %d %s", w.Code, w.Body)
	}
	var p types.RunPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func leaksAny(body string) bool {
	return slices.ContainsFunc(pinNeedles, func(n string) bool { return strings.Contains(body, n) })
}

// sentinelJSON is the sentinel as a JSON body spells it (encoding/json escapes < and >).
var sentinelJSON = strings.Trim(string(mustJSON(mountSourceWithheld)), `"`)

// pinRead asserts one read: the security admin reads the sentinel and no seeded host path, the super
// admin reads the host path.
func (e *govEnv) pinRead(method, path, body, want string) {
	e.t.Helper()
	sec := e.call(e.alice, method, path, body)
	if sec.Code != http.StatusOK || leaksAny(sec.Body.String()) || !strings.Contains(sec.Body.String(), sentinelJSON) {
		e.t.Errorf("security admin %s %s = %d, want 200 with the sentinel and no host path: %s", method, path, sec.Code, sec.Body)
	}
	if sup := e.call(e.super, method, path, body); sup.Code != http.StatusOK || !strings.Contains(sup.Body.String(), want) {
		e.t.Errorf("super admin %s %s = %d, want 200 naming %s: %s", method, path, sup.Code, want, sup.Body)
	}
}

func TestPolMount_R1_ListPoliciesWithholdsMountSources(t *testing.T) {
	e := newMountEnv(t, false)
	e.seedStoredPolicy("pm-r1")
	e.pinRead(http.MethodGet, "/api/v1/policies", "", pinMountSrc)
}

func TestPolMount_R2_GetPolicyWithholdsMountSources(t *testing.T) {
	e := newMountEnv(t, false)
	id := e.seedStoredPolicy("pm-r2")
	e.pinRead(http.MethodGet, "/api/v1/policies/"+id.String(), "", pinMountSrc)
}

func TestPolMount_R3_DefaultPolicyWithholdsMountSources(t *testing.T) {
	e := newMountEnv(t, false)
	e.pinRead(http.MethodGet, "/api/v1/policies/default", "", pinBaseSrc)
}

// R6: the policy preview is shown to the run's author whatever their tier, so it is redacted for a
// member even when a super admin asks.
func TestPolMount_R6_PolicyPreviewIsMemberTierForEveryone(t *testing.T) {
	e := newMountEnv(t, false)
	w := e.admin(http.MethodPost, "/api/v1/runs/policy-preview", `{"agent":"claude-code","task":"t"}`)
	var body policyPreviewResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("policy-preview = %d %s", w.Code, w.Body)
	}
	if i := slices.IndexFunc(body.Spec.WorkspaceMounts, func(m types.WorkspaceMount) bool { return m.Target == pinTgtA }); i < 0 ||
		body.Spec.WorkspaceMounts[i].Source != mountSourceWithheld {
		t.Errorf("the preview's mounts = %+v, want %s withheld", body.Spec.WorkspaceMounts, pinTgtA)
	}
	if leaksAny(w.Body.String()) {
		t.Errorf("the preview names a host path: %s", w.Body)
	}
}

// R10: the boundary dry run serves no mount, at any tier; the census (TestPolMount_CensusAST) is what
// holds its redaction to the reader's tier.
func TestPolMount_R10_BoundaryDryRunNamesNoHostPath(t *testing.T) {
	e := newMountEnv(t, false)
	e.seedMountProfile("pm-r10", pinMounts())
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/dry-run", boundaryJSON(testBoundary(types.BoundaryHardLimits)))
	if w.Code != http.StatusOK || leaksAny(w.Body.String()) {
		t.Errorf("dry run = %d %s, want 200 with no host path", w.Code, w.Body)
	}
}

// censusRoute is one read the census makes. who is nil for the admin token; field, when set, is the
// one top-level JSON member checked (a super admin's preflight may show its own policy elsewhere).
type censusRoute struct {
	name, method, path, body string
	security                 bool
	field                    string
}

// censusLeaks is every route whose answer names a needle, as fetch answers it.
func censusLeaks(routes []censusRoute, needles []string, fetch func(censusRoute) (int, string)) []string {
	var bad []string
	for _, r := range routes {
		code, body := fetch(r)
		if code != http.StatusOK {
			bad = append(bad, r.name+": status "+http.StatusText(code))
			continue
		}
		if r.field != "" {
			var doc map[string]json.RawMessage
			if json.Unmarshal([]byte(body), &doc) != nil {
				bad = append(bad, r.name+": not a JSON object")
				continue
			}
			body = string(doc[r.field])
		}
		if slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(body, n) }) {
			bad = append(bad, r.name)
		}
	}
	return bad
}

// TestPolMount_CensusRoutes seeds the host path into every place one lives (the deployment default, a
// stored policy, a preset, a standalone and a composed profile, a pending change) and reads every
// surface that serves them: none names it to a security admin, and the risk assessment names it to
// nobody.
func TestPolMount_CensusRoutes(t *testing.T) {
	e := newMountEnv(t, true)
	pol := e.seedStoredPolicy("pm-census")
	base := e.seedMountProfile("pm-census-base", pinMounts())
	e.seedComposed("pm-census-child", base, types.CeilingOverlay{WorkspaceMounts: &[]types.WorkspaceMount{{Source: pinMountSrc, Target: pinTgtA}}})
	preset, _ := json.Marshal(client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets",
		PolicyOverlay: json.RawMessage(`{"workspace_mounts":[{"source":"` + pinMountSrc + `","target":"` + pinTgtA + `"}]}`)}})
	if w := e.admin(http.MethodPut, "/api/v1/presets/pm-census", string(preset)); w.Code != http.StatusCreated {
		t.Fatalf("seed preset = %d %s", w.Code, w.Body)
	}
	diff := newProfileDiffView(base, nil, specReadFull) // a pending change as an older binary rendered it
	held, _, err := e.pg.ProposeGovernanceChange(context.Background(), types.GovernanceChange{
		TargetKind: govKindProfile, Op: "delete", TargetKey: base.ID.String(), Payload: mustJSON(map[string]any{"id": base.ID}),
		Diff:     renderGovernanceDiff(diff, nil, changedPaths(diff.profileDiffStored, nil), nil),
		BaseHash: "x", DeploymentHash: "x", ProposedBy: "sub-old", ProposedByEmail: "old@corp.example",
	}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	routes := []censusRoute{
		{"R1 GET /policies", http.MethodGet, "/api/v1/policies", "", true, ""},
		{"R2 GET /policies/{id}", http.MethodGet, "/api/v1/policies/" + pol.String(), "", true, ""},
		{"R3 GET /policies/default", http.MethodGet, "/api/v1/policies/default", "", true, ""},
		{"R5 GET /presets", http.MethodGet, "/api/v1/presets", "", true, ""},
		{"R5 GET /presets/{name}", http.MethodGet, "/api/v1/presets/pm-census", "", true, ""},
		{"R7 GET /governance", http.MethodGet, "/api/v1/governance", "", true, ""},
		{"R8 GET /governance/changes", http.MethodGet, "/api/v1/governance/changes", "", true, ""},
		{"R8 GET /governance/changes/{id}", http.MethodGet, "/api/v1/governance/changes/" + held.ID.String(), "", true, ""},
		{"R10 POST /governance/dry-run", http.MethodPost, "/api/v1/governance/dry-run", boundaryJSON(testBoundary(types.BoundaryHardLimits)), true, ""},
		{"R6 POST /runs/policy-preview", http.MethodPost, "/api/v1/runs/policy-preview", `{"agent":"claude-code","task":"t"}`, false, ""},
		{"R12 POST /runs/preflight risk_assessment", http.MethodPost, "/api/v1/runs/preflight", `{"agent":"claude-code","task":"t"}`, false, "risk_assessment"},
	}
	fetch := func(r censusRoute) (int, string) {
		if r.security {
			w := e.call(e.alice, r.method, r.path, r.body)
			return w.Code, w.Body.String()
		}
		w := e.admin(r.method, r.path, r.body)
		return w.Code, w.Body.String()
	}
	if bad := censusLeaks(routes, pinNeedles, fetch); len(bad) > 0 {
		t.Errorf("CENSUS: these reads name a seeded host path (or failed): %v", bad)
	}
	// The census is not vacuous: the super admin reads the host path on the security admin's routes.
	for _, r := range routes[:8] {
		if w := e.call(e.super, r.method, r.path, r.body); !leaksAny(w.Body.String()) {
			t.Errorf("CENSUS: the super admin's %s names no seeded host path, so the census proves nothing there: %s", r.name, w.Body)
		}
	}
}

// TestPolMount_CensusRoutes_PlantedViolation is the census's twin: a route that answers the needle is
// reported, by name, and the others are not.
func TestPolMount_CensusRoutes_PlantedViolation(t *testing.T) {
	routes := []censusRoute{{name: "clean"}, {name: "planted"}, {name: "planted field", field: "risk_assessment"}, {name: "other field", field: "x"}}
	got := censusLeaks(routes, []string{pinMountSrc}, func(r censusRoute) (int, string) {
		switch r.name {
		case "planted":
			return http.StatusOK, `{"spec":{"workspace_mounts":[{"source":"` + pinMountSrc + `"}]}}`
		case "planted field", "other field":
			return http.StatusOK, `{"risk_assessment":[{"value":"` + pinMountSrc + `→/w"}],"x":[]}`
		}
		return http.StatusOK, `{"spec":{"workspace_mounts":[{"source":"<redacted>"}]}}`
	})
	if !slices.Equal(got, []string{"planted", "planted field"}) {
		t.Errorf("the census reported %v, want exactly [planted, planted field]", got)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pathTree seeds Default → pp-a → pp-b → pp-c (pass-through), one home member in each, and the
// boundary in hard_limits. pp-a narrows max_holds and first_use_hold_seconds; pp-b restates the
// first, widens the second (its parent's meet keeps 20) and narrows auto_stop_after_sec. The rows go
// straight to the store, as a child written before its parent narrowed would be.
func pathTree(t *testing.T, e *govEnv) (a, b, c types.GovernanceUnit) {
	t.Helper()
	put := func(u types.GovernanceUnit) types.GovernanceUnit {
		u.CreatedBy = "seed"
		got, err := e.pg.UpsertGovernanceUnit(context.Background(), u)
		if err != nil {
			t.Fatalf("seed %s: %v", u.Name, err)
		}
		return got
	}
	eight, twenty, twentyFive, hour := 8, 20, 25, 3600
	def := store.DefaultUnitID
	a = put(types.GovernanceUnit{Name: "pp-a", ParentID: &def, Overlay: &types.CeilingOverlay{MaxHolds: &eight, FirstUseHoldSeconds: &twenty}})
	b = put(types.GovernanceUnit{Name: "pp-b", ParentID: &a.ID, Overlay: &types.CeilingOverlay{
		MaxHolds: &eight, FirstUseHoldSeconds: &twentyFive, AutoStopAfterSec: &hour}})
	c = put(types.GovernanceUnit{Name: "pp-c", ParentID: &b.ID, Mode: types.UnitModePassThrough, Overlay: &types.CeilingOverlay{}})
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-pp-a", 1, true)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-pp-b", 1, true)
	treeMember(t, e, c, types.CapabilitySubjectUser, "sub-pp-c", 1, true)
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	return a, b, c
}

// checkProvenanceRaw is POST /governance/check's effective.provenance for one user subject, as bytes.
func checkProvenanceRaw(t *testing.T, e *govEnv, sub string) string {
	t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["`+sub+`"]}`)
	var body struct {
		Effective struct {
			Provenance json.RawMessage `json:"provenance"`
		} `json:"effective"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("check %s = %d %s", sub, w.Code, w.Body)
	}
	return string(body.Effective.Provenance)
}

// TestCheckProvenanceGolden: POST /governance/check's provenance, byte for byte, as the tree's
// lane found it: the leaf names every knob its path changed, the boundary its own, never a unit
// above the leaf.
func TestCheckProvenanceGolden(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	pathTree(t, e)
	for sub, want := range map[string]string{
		"sub-pp-a":    goldenCheckA,
		"sub-pp-b":    goldenCheckB,
		"sub-pp-c":    goldenCheckC,
		"sub-pp-none": goldenCheckDefault,
	} {
		if got := checkProvenanceRaw(t, e, sub); got != want {
			t.Errorf("check %s provenance =\n%s\nwant\n%s", sub, got, want)
		}
	}
}

// TestRunProvenanceGolden: a member's run under a composed profile, the whole provenance of GET
// /runs/{id}/policy and of the run.policy.resolve row, byte for byte.
func TestRunProvenanceGolden(t *testing.T) {
	base, leaf := disclosureGraph()
	cs := assignedStore(&leaf)
	cs.govGraph = []types.GovernanceUnit{base, leaf}
	srv, st := pvFixture(t, cs)
	st.profiles = cs.govGraph
	member := pvReader{govSession(t, "sub-walled", []string{"eng"}, false)}
	resolve, _, raw := provenanceRun(t, srv, st, member, `{"agent":"claude-code","task":"t","inline_policy":{`+
		`"allowed_domains":["api.anthropic.com","off-wall.example"],"min_confinement_class":"CC3"}}`)
	var view, datum struct {
		Provenance json.RawMessage `json:"provenance"`
	}
	if json.Unmarshal([]byte(raw), &view) != nil || json.Unmarshal([]byte(resolve), &datum) != nil {
		t.Fatalf("decode: %s / %s", raw, resolve)
	}
	if got := string(view.Provenance); got != goldenRunView {
		t.Errorf("GET /runs/{id}/policy provenance =\n%s\nwant\n%s", got, goldenRunView)
	}
	if got := string(datum.Provenance); got != goldenRunResolve {
		t.Errorf("run.policy.resolve provenance =\n%s\nwant\n%s", got, goldenRunResolve)
	}
	if strings.Contains(raw+resolve, "path_provenance") {
		t.Errorf("a run view carries path_provenance: %s / %s", raw, resolve)
	}
}

// The golden records, captured from POST /governance/check and the run views at e767814e1, before
// path_provenance existed.
const (
	goldenCheckA       = `{"allow_all_egress":{"layer":"deployment","name":"deployment"},"allowed_domains":{"layer":"boundary"},"allowed_methods":{"layer":"deployment","name":"deployment"},"auto_stop_after_sec":{"layer":"deployment","name":"deployment"},"azure_devops_capabilities":{"layer":"deployment","name":"deployment"},"denied_domains":{"layer":"boundary"},"eligible_grants":{"layer":"deployment","name":"deployment"},"first_use_approval":{"layer":"deployment","name":"deployment"},"first_use_hold_seconds":{"layer":"unit","name":"pp-a"},"git_push_any_branch":{"layer":"deployment","name":"deployment"},"github_capabilities":{"layer":"deployment","name":"deployment"},"harness_inner_walls":{"layer":"deployment","name":"deployment"},"llm_inspection":{"layer":"deployment","name":"deployment"},"max_holds":{"layer":"unit","name":"pp-a"},"min_confinement_class":{"layer":"boundary"},"push_rules":{"layer":"deployment","name":"deployment"},"recording":{"layer":"deployment","name":"deployment"},"resources":{"layer":"unit","name":"pp-a"},"tool_rules":{"layer":"deployment","name":"deployment"},"ui_apps":{"layer":"deployment","name":"deployment"},"workspace_mounts":{"layer":"deployment","name":"deployment"},"workspace_repos":{"layer":"deployment","name":"deployment"}}`
	goldenCheckB       = `{"allow_all_egress":{"layer":"deployment","name":"deployment"},"allowed_domains":{"layer":"boundary"},"allowed_methods":{"layer":"deployment","name":"deployment"},"auto_stop_after_sec":{"layer":"unit","name":"pp-b"},"azure_devops_capabilities":{"layer":"deployment","name":"deployment"},"denied_domains":{"layer":"boundary"},"eligible_grants":{"layer":"deployment","name":"deployment"},"first_use_approval":{"layer":"deployment","name":"deployment"},"first_use_hold_seconds":{"layer":"unit","name":"pp-b"},"git_push_any_branch":{"layer":"deployment","name":"deployment"},"github_capabilities":{"layer":"deployment","name":"deployment"},"harness_inner_walls":{"layer":"deployment","name":"deployment"},"llm_inspection":{"layer":"deployment","name":"deployment"},"max_holds":{"layer":"unit","name":"pp-b"},"min_confinement_class":{"layer":"boundary"},"push_rules":{"layer":"deployment","name":"deployment"},"recording":{"layer":"deployment","name":"deployment"},"resources":{"layer":"unit","name":"pp-b"},"tool_rules":{"layer":"deployment","name":"deployment"},"ui_apps":{"layer":"deployment","name":"deployment"},"workspace_mounts":{"layer":"deployment","name":"deployment"},"workspace_repos":{"layer":"deployment","name":"deployment"}}`
	goldenCheckC       = `{"allow_all_egress":{"layer":"deployment","name":"deployment"},"allowed_domains":{"layer":"boundary"},"allowed_methods":{"layer":"deployment","name":"deployment"},"auto_stop_after_sec":{"layer":"unit","name":"pp-c"},"azure_devops_capabilities":{"layer":"deployment","name":"deployment"},"denied_domains":{"layer":"boundary"},"eligible_grants":{"layer":"deployment","name":"deployment"},"first_use_approval":{"layer":"deployment","name":"deployment"},"first_use_hold_seconds":{"layer":"unit","name":"pp-c"},"git_push_any_branch":{"layer":"deployment","name":"deployment"},"github_capabilities":{"layer":"deployment","name":"deployment"},"harness_inner_walls":{"layer":"deployment","name":"deployment"},"llm_inspection":{"layer":"deployment","name":"deployment"},"max_holds":{"layer":"unit","name":"pp-c"},"min_confinement_class":{"layer":"boundary"},"push_rules":{"layer":"deployment","name":"deployment"},"recording":{"layer":"deployment","name":"deployment"},"resources":{"layer":"unit","name":"pp-c"},"tool_rules":{"layer":"deployment","name":"deployment"},"ui_apps":{"layer":"deployment","name":"deployment"},"workspace_mounts":{"layer":"deployment","name":"deployment"},"workspace_repos":{"layer":"deployment","name":"deployment"}}`
	goldenCheckDefault = `{"allow_all_egress":{"layer":"deployment","name":"deployment"},"allowed_domains":{"layer":"boundary"},"allowed_methods":{"layer":"deployment","name":"deployment"},"auto_stop_after_sec":{"layer":"deployment","name":"deployment"},"azure_devops_capabilities":{"layer":"deployment","name":"deployment"},"denied_domains":{"layer":"boundary"},"eligible_grants":{"layer":"deployment","name":"deployment"},"first_use_approval":{"layer":"deployment","name":"deployment"},"first_use_hold_seconds":{"layer":"deployment","name":"deployment"},"git_push_any_branch":{"layer":"deployment","name":"deployment"},"github_capabilities":{"layer":"deployment","name":"deployment"},"harness_inner_walls":{"layer":"deployment","name":"deployment"},"llm_inspection":{"layer":"deployment","name":"deployment"},"max_holds":{"layer":"deployment","name":"deployment"},"min_confinement_class":{"layer":"boundary"},"push_rules":{"layer":"deployment","name":"deployment"},"recording":{"layer":"deployment","name":"deployment"},"resources":{"layer":"deployment","name":"deployment"},"tool_rules":{"layer":"deployment","name":"deployment"},"ui_apps":{"layer":"deployment","name":"deployment"},"workspace_mounts":{"layer":"deployment","name":"deployment"},"workspace_repos":{"layer":"deployment","name":"deployment"}}`
	goldenRunView      = `{"allow_all_egress":{"layer":"member"},"allowed_domains":{"layer":"unit","name":"team"},"allowed_methods":{"layer":"member"},"auto_stop_after_sec":{"layer":"member"},"azure_devops_capabilities":{"layer":"member"},"denied_domains":{"layer":"unit","name":"team"},"eligible_grants":{"layer":"member"},"first_use_approval":{"layer":"member"},"first_use_hold_seconds":{"layer":"member"},"git_push_any_branch":{"layer":"member"},"github_capabilities":{"layer":"member"},"harness_inner_walls":{"layer":"member"},"llm_inspection":{"layer":"member"},"max_holds":{"layer":"member"},"min_confinement_class":{"layer":"member"},"push_rules":{"layer":"member"},"recording":{"layer":"member"},"resources":{"layer":"unit","name":"team"},"tool_rules":{"layer":"member"},"ui_apps":{"layer":"member"},"workspace_mounts":{"layer":"member"},"workspace_repos":{"layer":"member"}}`
	goldenRunResolve   = `{"allow_all_egress":{"layer":"member"},"allowed_domains":{"layer":"unit","name":"team"},"allowed_methods":{"layer":"member"},"auto_stop_after_sec":{"layer":"member"},"azure_devops_capabilities":{"layer":"member"},"denied_domains":{"layer":"unit","name":"team"},"eligible_grants":{"layer":"member"},"first_use_approval":{"layer":"member"},"first_use_hold_seconds":{"layer":"member"},"git_push_any_branch":{"layer":"member"},"github_capabilities":{"layer":"member"},"harness_inner_walls":{"layer":"member"},"llm_inspection":{"layer":"member"},"max_holds":{"layer":"member"},"min_confinement_class":{"layer":"member"},"push_rules":{"layer":"member"},"recording":{"layer":"member"},"resources":{"layer":"unit","name":"team"},"tool_rules":{"layer":"member"},"ui_apps":{"layer":"member"},"workspace_mounts":{"layer":"member"},"workspace_repos":{"layer":"member"}}`
)

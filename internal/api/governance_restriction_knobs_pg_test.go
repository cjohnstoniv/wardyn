// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allKnobsRestriction carries all six narrow-only knobs a restriction can now hold.
func allKnobsRestriction() (types.RestrictionSpec, types.RestrictionLimits) {
	deny := true
	rec := types.RecordingScreenActions
	return types.RestrictionSpec{
			AllowedDomains: strs("b.example", "z.example"),
			ToolRules:      &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}},
			PushRules:      &types.PushRulesOverlay{DenyAll: &deny},
			Desktop:        &types.DesktopControls{CopyOut: types.TransferBlocked},
			Recording:      &rec,
		},
		types.RestrictionLimits{AllowedModels: strs("model-a")}
}

// TestRestrictionKnobsBindInEveryUnit: a person in two units, under one restriction carrying all six
// knobs, has each narrowed in the ceiling of either unit, and the policy record names the restriction.
func TestRestrictionKnobsBindInEveryUnit(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("rk-a", "a.example", "b.example")
	b := e.seedProfile("rk-b", "b.example", "c.example")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-rk-ctx", 5, false)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-rk-ctx", 1, false)
	spec, limits := allKnobsRestriction()
	r := seedRestriction(t, e, "rk-all", spec, limits, userMember("sub-rk-ctx"))
	want := policyref.Provenance{Layer: policyref.LayerRestriction, Name: "rk-all"}
	for name, unit := range map[string]types.GovernanceUnit{"a": a, "b": b} {
		c := treeCeiling(t, e, withContextChoice(treeCtx("sub-rk-ctx", nil, ""), contextChoice{RunIn: unit.ID}))
		if c.Spec.AllowAllEgress || !slices.Equal(c.Spec.AllowedDomains, []string{"b.example"}) {
			t.Errorf("run_in %s: egress %v %v, want closed with [b.example]", name, c.Spec.AllowAllEgress, c.Spec.AllowedDomains)
		}
		if !slices.ContainsFunc(c.Spec.ToolRules, func(r types.ToolRule) bool { return r.Tool == "Bash" && r.Effect == types.ToolDeny }) {
			t.Errorf("run_in %s: tool_rules %v, want Bash denied", name, c.Spec.ToolRules)
		}
		if c.Spec.PushRules == nil || !c.Spec.PushRules.DenyAll {
			t.Errorf("run_in %s: push_rules %+v, want deny_all", name, c.Spec.PushRules)
		}
		if c.Spec.Desktop == nil || c.Spec.Desktop.CopyOut != types.TransferBlocked {
			t.Errorf("run_in %s: desktop %+v, want copy_out blocked", name, c.Spec.Desktop)
		}
		if c.Spec.Recording != types.RecordingScreenActions {
			t.Errorf("run_in %s: recording %q, want screen_actions", name, c.Spec.Recording)
		}
		if !slices.Equal(c.Limits.AllowedModels, []string{"model-a"}) {
			t.Errorf("run_in %s: allowed_models %v, want [model-a]", name, c.Limits.AllowedModels)
		}
		if ids := restrictionIDsOf(c); len(ids) != 1 || ids[0] != r.ID {
			t.Errorf("run_in %s: restriction ids %v, want [%s]", name, ids, r.ID)
		}
		knobs := startProvenance(c.provenance())
		c.restrictionRunStep(knobs)
		for _, k := range []string{"allowed_domains", "tool_rules", "push_rules", "desktop", "recording"} {
			if knobs[k] != want {
				t.Errorf("run_in %s: %s recorded %+v, want the restriction", name, k, knobs[k])
			}
		}
	}
}

// TestRestrictionKnobValidation: the policy writer's own checks run on each new knob, and a refusal is the
// route's restriction_invalid.
func TestRestrictionKnobValidation(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	models := make([]string, 201)
	for i := range models {
		models[i] = fmt.Sprintf("model-%d", i)
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"tool rule":       `{"name":"x","spec":{"tool_rules":[{"tool":"Bash","effect":"sometimes"}]},"limits":{},"members":[]}`,
		"recording level": `{"name":"x","spec":{"recording":"everything"},"limits":{},"members":[]}`,
		"desktop review":  `{"name":"x","spec":{"desktop":{"copy_out":"review"}},"limits":{},"members":[]}`,
		"allowed_models":  `{"name":"x","spec":{},"limits":{"allowed_models":` + string(modelsJSON) + `},"members":[]}`,
		"push deny path":  `{"name":"x","spec":{"push_rules":{"deny_paths":[""]}},"limits":{},"members":[]}`,
		"allowed_domains": `{"name":"x","spec":{"allowed_domains":["http://a.example/path"]},"limits":{},"members":[]}`,
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonRestrictionInvalid {
			t.Errorf("%s: %d %s, want 422 %s", name, w.Code, w.Body, reasonRestrictionInvalid)
		}
	}
	good := `{"name":"ok","spec":{"allowed_domains":["a.example"],"tool_rules":[{"tool":"Bash","effect":"hold"}],` +
		`"push_rules":{"deny_all":false,"deny_paths":[".github/**"]},"desktop":{"copy_out":"blocked"},"recording":"terminal"},` +
		`"limits":{"allowed_models":["model-a"]},"members":[]}`
	if w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", good); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Errorf("a restriction with every knob valid: %d %s, want it stored", w.Code, w.Body)
	}
}

// TestRestrictionAllowedDomainsIntersect: a unit allowing [a,b] and a restriction allowing [b,c] give [b].
func TestRestrictionAllowedDomainsIntersect(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := e.seedProfile("ri-u", "a.example", "b.example")
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-ri", 1, false)
	seedRestriction(t, e, "ri-r", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionLimits{}, userMember("sub-ri"))
	c := treeCeiling(t, e, treeCtx("sub-ri", nil, ""))
	if !slices.Equal(c.Spec.AllowedDomains, []string{"b.example"}) {
		t.Errorf("allowed_domains = %v, want [b.example]", c.Spec.AllowedDomains)
	}
}

// TestRestrictionAbsentKnobsUnchanged: a restriction that names none of the new knobs, as one written before
// they existed, gives the ceiling it always did.
func TestRestrictionAbsentKnobsUnchanged(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := e.seedProfile("ru-u", "a.example", "pypi.org")
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-ru-plain", 1, false)
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-ru-bound", 1, false)
	var spec types.RestrictionSpec
	if err := json.Unmarshal([]byte(`{"denied_domains":["pypi.org"]}`), &spec); err != nil {
		t.Fatal(err)
	}
	seedRestriction(t, e, "ru-r", spec, types.RestrictionLimits{}, userMember("sub-ru-bound"))
	plain := treeCeiling(t, e, treeCtx("sub-ru-plain", nil, ""))
	on := true
	want, _, err := composer.ApplyOverlay(composer.Authority{Ceiling: plain.Spec, Limits: plain.Limits},
		composer.Overlay{Ceiling: types.CeilingOverlay{DeniedDomains: strs("pypi.org"), AllowAllEgress: &on}})
	if err != nil {
		t.Fatal(err)
	}
	got := treeCeiling(t, e, treeCtx("sub-ru-bound", nil, ""))
	wantJSON, _ := json.Marshal([]any{want.Ceiling, want.Limits})
	gotJSON, _ := json.Marshal([]any{got.Spec, got.Limits})
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("ceiling changed:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestRestrictionBreakGlassLiftsNewKnobs: an operator's ceiling carries none of the new knobs, while the
// boundary still binds it.
func TestRestrictionBreakGlassLiftsNewKnobs(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	spec, limits := allKnobsRestriction()
	seedRestriction(t, e, "rb-all", spec, limits, types.RestrictionMember{SubjectType: types.CapabilitySubjectAll})
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	for name, ctx := range map[string]context.Context{"admin token": context.Background()} {
		c := treeCeiling(t, e, ctx)
		if len(c.RestrictionIDs) != 0 || len(c.Limits.AllowedModels) != 0 || c.Spec.Recording == types.RecordingScreenActions ||
			(c.Spec.PushRules != nil && c.Spec.PushRules.DenyAll) || slices.ContainsFunc(c.Spec.ToolRules, func(r types.ToolRule) bool { return r.Tool == "Bash" }) {
			t.Errorf("%s: a restriction's knob reached the ceiling: %+v %+v", name, c.Spec, c.Limits)
		}
		if !slices.Contains(c.Spec.DeniedDomains, "evil.example") {
			t.Errorf("%s: the boundary no longer binds: %v", name, c.Spec.DeniedDomains)
		}
	}
}

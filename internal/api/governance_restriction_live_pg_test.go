// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reachablePolicy is a policy as a run under a unit holds it: both hosts reachable, every rule unset or wide.
func reachablePolicy() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains: []string{"a.example", "b.example"}, MinConfinementClass: types.CC2,
		ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}},
		Desktop:   &types.DesktopControls{CopyOut: types.TransferAllowed}, Recording: types.RecordingOff,
	}
}

func hasToolEffect(rules []types.ToolRule, tool string, effect types.ToolEffect) bool {
	return slices.ContainsFunc(rules, func(r types.ToolRule) bool { return r.Tool == tool && r.Effect == effect })
}

// TestRestrictionKnobsSealRunningRun: a restriction that gains an allowlist, tool rules, push rules, desktop
// rules and a recording floor narrows a running run's policy at the final seal, records itself for each
// knob, and withholds a credential for a host its allowlist leaves out at a live mint.
func TestRestrictionKnobsSealRunningRun(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	u := e.seedProfile("rl-u", "a.example", "b.example")
	spec, limits := allKnobsRestriction()
	r := seedRestriction(t, e, "rl-r", spec, limits, userMember("sub-rl"))
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rl", Agent: "claude-code", ConfinementClass: types.CC3,
		State: types.RunRunning, RunnerTarget: "docker", Task: "t", UnitID: &u.ID, RestrictionIDs: []uuid.UUID{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	policy := reachablePolicy()
	prov := knobProvenance{}
	e.srv.sealRunPolicy(ctx, run, &policy, prov)
	if policy.AllowAllEgress || !slices.Equal(policy.AllowedDomains, []string{"b.example"}) {
		t.Errorf("sealed egress = %v %v, want closed with [b.example]", policy.AllowAllEgress, policy.AllowedDomains)
	}
	if !hasToolEffect(policy.ToolRules, "Bash", types.ToolDeny) || hasToolEffect(policy.ToolRules, "Bash", types.ToolAllow) {
		t.Errorf("sealed tool_rules = %v, want Bash denied", policy.ToolRules)
	}
	if policy.PushRules == nil || !policy.PushRules.DenyAll {
		t.Errorf("sealed push_rules = %+v, want deny_all", policy.PushRules)
	}
	if policy.Desktop == nil || policy.Desktop.CopyOut != types.TransferBlocked {
		t.Errorf("sealed desktop = %+v, want copy_out blocked", policy.Desktop)
	}
	if policy.Recording != types.RecordingScreenActions {
		t.Errorf("sealed recording = %q, want screen_actions", policy.Recording)
	}
	want := policyref.Provenance{Layer: policyref.LayerRestriction, Name: "rl-r"}
	for _, k := range []string{"allowed_domains", "tool_rules", "push_rules", "desktop", "recording"} {
		if prov[k] != want {
			t.Errorf("the seal recorded %+v for %s, want the restriction", prov[k], k)
		}
	}
	w, err := e.srv.liveWithholding(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if !w.withheldAny([]string{"a.example"}) || w.withheldAny([]string{"b.example"}) {
		t.Errorf("live withholding: a.example %v b.example %v, want true false", w.withheldAny([]string{"a.example"}), w.withheldAny([]string{"b.example"}))
	}
	// A seal that has nothing to narrow leaves the policy as it was.
	again := policy
	e.srv.sealRunPolicy(ctx, run, &again, knobProvenance{})
	if a, b := mustJSON(again), mustJSON(policy); string(a) != string(b) {
		t.Errorf("a second seal moved the policy:\n got %s\nwant %s", a, b)
	}
}

// TestRestrictionAllowlistClosesOpenEgressAtSeal: a run that was open when it was frozen is closed to the
// restriction's allowlist.
func TestRestrictionAllowlistClosesOpenEgressAtSeal(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "rl-open", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionLimits{}, userMember("sub-rl-open"))
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rl-open", RestrictionIDs: []uuid.UUID{r.ID}}
	policy := types.RunPolicySpec{AllowAllEgress: true, MinConfinementClass: types.CC2}
	e.srv.sealRunPolicy(context.Background(), run, &policy, knobProvenance{})
	if policy.AllowAllEgress || !slices.Equal(policy.AllowedDomains, []string{"b.example"}) {
		t.Errorf("sealed egress = %v %v, want closed with [b.example]", policy.AllowAllEgress, policy.AllowedDomains)
	}
}

// TestRestrictionKnobsReviveHold: a revive holds a run to a restriction gained after it launched: the
// restriction's id is re-captured, the revive's seal narrows the frozen policy, and a credential for a host
// the allowlist leaves out is withheld.
func TestRestrictionKnobsReviveHold(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	u := e.seedProfile("rv-u", "a.example", "b.example")
	treeMember(t, e, u, types.CapabilitySubjectUser, "sub-rl-rv", 1, false)
	run, err := e.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rl-rv", UserType: types.UserTypeStandard, Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t", UnitID: &u.ID})
	if err != nil {
		t.Fatal(err)
	}
	spec, limits := allKnobsRestriction()
	r := seedRestriction(t, e, "rv-late", spec, limits, userMember("sub-rl-rv"))
	c, rerr := e.srv.reviveCeiling(treeCtx("sub-rl-rv", nil, ""), run, types.ActorHuman, "sub-rl-rv")
	if rerr != nil {
		t.Fatalf("owner revive: %+v", rerr)
	}
	if !slices.Equal(c.restrictionIDs, []uuid.UUID{r.ID}) {
		t.Fatalf("owner revive holds %v, want [%s]", c.restrictionIDs, r.ID)
	}
	run.RestrictionIDs = c.restrictionIDs
	policy := reachablePolicy()
	layers := e.srv.sealRunPolicy(ctx, run, &policy, nil)
	if !slices.Equal(policy.AllowedDomains, []string{"b.example"}) || policy.AllowAllEgress {
		t.Errorf("revived egress = %v %v, want closed with [b.example]", policy.AllowAllEgress, policy.AllowedDomains)
	}
	if !hasToolEffect(policy.ToolRules, "Bash", types.ToolDeny) || policy.PushRules == nil || !policy.PushRules.DenyAll ||
		policy.Desktop == nil || policy.Desktop.CopyOut != types.TransferBlocked || policy.Recording != types.RecordingScreenActions {
		t.Errorf("revived policy = %+v, want every knob narrowed", policy)
	}
	w := withholding{deny: c.deny, layers: layers}
	if !w.withheldAny([]string{"a.example"}) || w.withheldAny([]string{"b.example"}) {
		t.Errorf("revive withholding: a.example %v b.example %v, want true false", w.withheldAny([]string{"a.example"}), w.withheldAny([]string{"b.example"}))
	}
}

// TestRestrictionAllowlistApprovalRefused: an approval cannot admit a host a captured restriction's
// allowlist leaves out.
func TestRestrictionAllowlistApprovalRefused(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "rl-appr", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionLimits{}, userMember("sub-rl-appr"))
	run, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rl-appr", Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t", RestrictionIDs: []uuid.UUID{r.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(host string) uuid.UUID {
		ap, err := e.h.approvals.Request(context.Background(), types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return ap.ID
	}
	decider := e.otherServer(func(c *Config) { c.Approvals = e.h.approvals })
	approve := func(id uuid.UUID) int {
		return doSSO(t, decider, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", e.alice, `{}`).Code
	}
	if code := approve(ask("a.example")); code != http.StatusForbidden {
		t.Errorf("approving a host the allowlist leaves out: %d, want 403", code)
	}
	if code := approve(ask("b.example")); code != http.StatusOK {
		t.Errorf("approving a host the allowlist names: %d, want 200", code)
	}
}

// TestRestrictionEmptyPushRulesEqualsAbsent: push_rules {} is stored and changes no ceiling, as if absent.
func TestRestrictionEmptyPushRulesEqualsAbsent(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	w := e.admin(http.MethodPost, "/api/v1/governance/restrictions", `{"name":"empty-push","spec":{"push_rules":{}},"limits":{},"members":[]}`)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("an empty push_rules: %d %s, want it stored", w.Code, w.Body)
	}
	eff, err := restrictionEffective(types.RestrictionSpec{PushRules: &types.PushRulesOverlay{}}, types.RestrictionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := restrictionEffective(types.RestrictionSpec{}, types.RestrictionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(eff), mustJSON(absent); string(a) != string(b) {
		t.Errorf("empty push_rules changes the ceiling:\n got %s\nwant %s", a, b)
	}
}

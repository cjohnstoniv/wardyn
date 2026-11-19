// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// restriction's allowlist, whatever list of its own it carried; a closed run keeps only what both lists name.
func TestRestrictionAllowlistClosesOpenEgressAtSeal(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	r := seedRestriction(t, e, "rl-open", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionLimits{}, userMember("sub-rl-open"))
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rl-open", RestrictionIDs: []uuid.UUID{r.ID}}
	for _, tc := range []struct {
		open      bool
		own, want []string
	}{
		{true, nil, []string{"b.example"}},
		{true, []string{"a.example", "b.example"}, []string{"b.example"}},
		{false, []string{"a.example"}, []string{}},
	} {
		policy := types.RunPolicySpec{AllowAllEgress: tc.open, AllowedDomains: tc.own, MinConfinementClass: types.CC2}
		e.srv.sealRunPolicy(context.Background(), run, &policy, knobProvenance{})
		if policy.AllowAllEgress || !slices.Equal(policy.AllowedDomains, tc.want) {
			t.Errorf("open %v with %v: sealed egress = %v %v, want closed with %v", tc.open, tc.own, policy.AllowAllEgress, policy.AllowedDomains, tc.want)
		}
	}
}

// TestRestrictionAllowlistClosesOpenEgressAtCreate: a restriction created with an allowlist waits for a second
// person; once approved, a create meets its allowlist into an open ceiling as the seal does, so a person
// under the restriction in a unit that allows all egress is closed to the restriction's list, not to nothing.
func TestRestrictionAllowlistClosesOpenEgressAtCreate(t *testing.T) {
	e := newGovEnv(t)
	open := governanceCeiling{Unit: &ResolvedUnit{ID: uuid.New(), Name: "open"}, Spec: types.RunPolicySpec{AllowAllEgress: true}}
	w := e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, types.RestrictionWrite{Name: "rl-create",
		Spec: types.RestrictionSpec{AllowedDomains: strs("b.example")}, Members: []types.RestrictionMember{userMember("sub-rl-create")}}))
	e.approve(e.pending(w).ID)
	list, err := e.pg.ListRestrictions(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("restrictions after the approval: %v %v, want the one created", list, err)
	}
	c, err := e.srv.withRestrictions(context.Background(), open, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.Spec.AllowAllEgress || !slices.Equal(c.Spec.AllowedDomains, []string{"b.example"}) {
		t.Errorf("ceiling egress = %v %v, want closed with [b.example]", c.Spec.AllowAllEgress, c.Spec.AllowedDomains)
	}
}

// TestRestrictionCreateNamingAllowlistIsHeld: with the switch on, a create that names allowed_domains is held
// for a second person (202), which its proposer cannot approve, and stores nothing until it is approved, as a first list on an update is; a create
// that names none only narrows and applies directly (201).
func TestRestrictionCreateNamingAllowlistIsHeld(t *testing.T) {
	e := newGovEnv(t)
	post := func(body types.RestrictionWrite) *httptest.ResponseRecorder {
		return e.call(e.alice, http.MethodPost, "/api/v1/governance/restrictions", restrictionJSON(t, body))
	}
	stored := func() int {
		list, err := e.pg.ListRestrictions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	w := post(types.RestrictionWrite{Name: "held-create", Spec: types.RestrictionSpec{AllowedDomains: strs("b.example")}})
	ch := e.pending(w)
	if ch.TargetKind != govKindRestriction || ch.Op != "create" || stored() != 0 {
		t.Errorf("held create: kind %s op %s, %d stored, want a restriction create with nothing stored", ch.TargetKind, ch.Op, stored())
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" || stored() != 0 {
		t.Errorf("the proposer approving their own create: %d %s, %d stored, want 403 second_human_required and nothing stored", w.Code, w.Body, stored())
	}
	e.approve(ch.ID)
	if stored() != 1 {
		t.Errorf("after the approval: %d stored, want 1", stored())
	}
	if w := post(types.RestrictionWrite{Name: "direct-create", Spec: types.RestrictionSpec{DeniedDomains: strs("x.example")}}); w.Code != http.StatusCreated {
		t.Errorf("create naming no allowed_domains: %d %s, want 201", w.Code, w.Body)
	}
}

// TestRestrictionAllowlistSwapIsNotAtLeastAsStrict: a replacement is exempt from a second human only when
// its allowlist is a subset of the old one on the whole entry (host, port and literal) and it keeps every
// deny. A swap to another host, port or literal, the same name on another port, an added port or literal
// entry, a wildcard for a name, a list where the old one closed open egress with none, a dropped list and a
// dropped deny are each held. So is a name for the wildcard over it (a run naming that host gets nothing
// under the wildcard and the host under the name). Every first list is held, whatever it names (a name, a
// wildcard, or only another port): there is no old list to judge it within, and it is met with each run's
// own list. A dropped entry, a narrower wildcard, and a reorder with a duplicate are exempt.
func TestRestrictionAllowlistSwapIsNotAtLeastAsStrict(t *testing.T) {
	off := false
	for _, tc := range []struct {
		name      string
		old, next types.RestrictionSpec
		exempt    bool
	}{
		{"a name swapped", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionSpec{AllowedDomains: strs("evil.example")}, false},
		{"a port swapped", types.RestrictionSpec{AllowedDomains: strs("b.example:8443")}, types.RestrictionSpec{AllowedDomains: strs("b.example:9443")}, false},
		{"a name to a literal", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowedDomains: strs("203.0.113.7")}, false},
		{"a name to a port", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowedDomains: strs("evil.example:8443")}, false},
		{"the same name on another port", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example:8443")}, false},
		{"a port entry added", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example", "evil.example:8443")}, false},
		{"a literal added", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example", "203.0.113.7")}, false},
		{"a port added beside its bare name", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example", "b.example:22")}, false},
		{"a wildcard for a name", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowedDomains: strs("*.example")}, false},
		{"a name for the wildcard over it", types.RestrictionSpec{AllowedDomains: strs("*.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example")}, false},
		{"a first list naming a host", types.RestrictionSpec{}, types.RestrictionSpec{AllowedDomains: strs("b.example")}, false},
		{"a first list naming a wildcard", types.RestrictionSpec{}, types.RestrictionSpec{AllowedDomains: strs("*.example")}, false},
		{"a first list naming only another port", types.RestrictionSpec{}, types.RestrictionSpec{AllowedDomains: strs("b.example:8443")}, false},
		{"a list where none closed open egress", types.RestrictionSpec{AllowAllEgress: &off}, types.RestrictionSpec{AllowedDomains: strs("b.example")}, false},
		{"the list dropped", types.RestrictionSpec{AllowedDomains: strs("b.example")}, types.RestrictionSpec{AllowAllEgress: &off}, false},
		{"a deny dropped", types.RestrictionSpec{AllowedDomains: strs("b.example"), DeniedDomains: strs("x.b.example", "y.b.example")},
			types.RestrictionSpec{AllowedDomains: strs("b.example"), DeniedDomains: strs("x.b.example")}, false},
		{"a host dropped", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionSpec{AllowedDomains: strs("b.example")}, true},
		{"a port entry dropped", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example:8443")}, types.RestrictionSpec{AllowedDomains: strs("b.example")}, true},
		{"a narrower wildcard", types.RestrictionSpec{AllowedDomains: strs("*.example")}, types.RestrictionSpec{AllowedDomains: strs("*.a.example")}, true},
		{"reordered with a duplicate", types.RestrictionSpec{AllowedDomains: strs("b.example", "c.example")}, types.RestrictionSpec{AllowedDomains: strs("c.example", "b.example", "b.example")}, true},
	} {
		if got := restrictionAtLeastAsStrict(types.Restriction{Spec: tc.old}, types.RestrictionWrite{Spec: tc.next}); got != tc.exempt {
			t.Errorf("%s: at least as strict = %v, want %v", tc.name, got, tc.exempt)
		}
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

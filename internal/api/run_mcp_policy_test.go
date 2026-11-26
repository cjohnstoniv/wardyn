// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpPolicyComps is a run's components: Files, a server that starts from a command with the hosts given (one
// by default) and the given sandbox policy (nil reads same), then Remote, a remote MCP server the agent
// reaches only through the broker.
func mcpPolicyComps(policy *types.ComponentMCPSandboxPolicy, hosts ...string) runComponents {
	files := *localInline("Files").Inline
	if len(hosts) > 0 {
		files.Hosts = hosts
	}
	if policy != nil {
		files.MCP.Sandbox = &types.ComponentMCPSandbox{Policy: *policy}
	}
	remote := types.ComponentDefinition{MCP: &types.ComponentMCP{Transport: types.ComponentMCPTransportHTTP, URL: "https://mcp.remote.test/mcp"}}
	return runComponents{attached: []attachedComponent{
		{snapshot: types.RunComponent{Name: "Files", Definition: files}},
		{snapshot: types.RunComponent{Name: "Remote", Definition: remote}},
	}}
}

// mcpPolicyRun is P_run: two allowed hosts and one denied, a first-use posture, a host rule set that holds
// a.test's /admin, a class and a lifetime, and knobs an MCP sandbox never holds.
func mcpPolicyRun() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains: []string{"a.test", "b.test"}, DeniedDomains: []string{"d.test"},
		FirstUseApproval: types.FirstUseWaitForReview, FirstUseHoldSeconds: 60, MaxHolds: 3,
		HostRules:           []types.HostRuleSet{{Rules: []types.HostRule{{Host: "a.test", Paths: []string{"/admin"}, Effect: types.HostRuleHold}}}},
		MinConfinementClass: types.CC2, AutoStopAfterSec: 3600,
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/src", Target: "/workspace"}},
		ToolRules:       []types.ToolRule{{Tool: "*", Effect: types.ToolHold}},
		EligibleGrants:  []types.GrantSpec{{Kind: types.GrantGitHubToken}},
		Recording:       "full",
	}
}

// mcpHoldLayer is a boundary layer allowing allow, denying layer-deny.test, with a set that holds h.test.
func mcpHoldLayer(allow ...string) []sealLayer {
	l := listLayer(policyref.LayerBoundary, allow, "layer-deny.test")
	sets := []types.HostRuleSet{{Rules: []types.HostRule{{Host: "h.test", Effect: types.HostRuleHold}}}}
	l.egress.HostRules = &sets
	return []sealLayer{l}
}

func hostRuleEffects(sets []types.HostRuleSet) map[string]types.HostRuleEffect {
	out := map[string]types.HostRuleEffect{}
	for _, s := range sets {
		for _, r := range s.Rules {
			out[r.Host] = r.Effect
		}
	}
	return out
}

func mcpPolicyOf(t *testing.T, s *Server, b mcpPolicyBase) mcpPolicy {
	t.Helper()
	p, err := s.mcpSandboxPolicy(context.Background(), b, 0)
	if err != nil {
		t.Fatalf("mcpSandboxPolicy: %v", err)
	}
	return p
}

// TestMCPPolicySameBoundedByRunAndAdmittedHosts: "same" allows no host outside the seal of the run's
// allowlist and the server's hosts, denies every host the run denies and every brokered host, opens egress
// only when the run does, takes the run's class, lifetime and first-use posture, sizes the sandbox at the
// default, and holds none of the knobs an MCP sandbox never holds.
func TestMCPPolicySameBoundedByRunAndAdmittedHosts(t *testing.T) {
	comps := mcpPolicyComps(nil)
	layers := []sealLayer{listLayer(policyref.LayerBoundary, []string{"a.test", "api.files.test"})}
	for _, open := range []bool{false, true} {
		run := mcpPolicyRun()
		run.AllowAllEgress = open
		layers := layers
		if open {
			layers = nil // a boundary list closes an open run (sealPolicy); open egress survives only with none
		}
		p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, comps: comps, layers: layers})
		bound := types.RunPolicySpec{AllowedDomains: append(slices.Clone(run.AllowedDomains), comps.attached[0].snapshot.Definition.Hosts...)}
		sealPolicy(&bound, layers)
		if !composer.AllowlistWithin(p.spec.AllowedDomains, bound.AllowedDomains) {
			t.Errorf("open=%v: allowed %v outside seal(run ∪ admitted) %v", open, p.spec.AllowedDomains, bound.AllowedDomains)
		}
		if !open && slices.Contains(p.spec.AllowedDomains, "b.test") {
			t.Errorf("allowed %v keeps b.test, which the boundary removed", p.spec.AllowedDomains)
		}
		for _, d := range append(slices.Clone(run.DeniedDomains), "mcp.remote.test") {
			if !slices.Contains(p.spec.DeniedDomains, d) {
				t.Errorf("open=%v: denied %v lacks %s", open, p.spec.DeniedDomains, d)
			}
		}
		if p.spec.AllowAllEgress != open {
			t.Errorf("allow_all_egress = %v, want the run's %v", p.spec.AllowAllEgress, open)
		}
		if p.spec.MinConfinementClass != types.CC2 || p.spec.AutoStopAfterSec != 3600 || p.spec.FirstUseApproval != types.FirstUseWaitForReview ||
			p.spec.FirstUseHoldSeconds != 60 || p.spec.MaxHolds != 3 {
			t.Errorf("posture = %s %d %s %d %d, want the run's", p.spec.MinConfinementClass, p.spec.AutoStopAfterSec, p.spec.FirstUseApproval, p.spec.FirstUseHoldSeconds, p.spec.MaxHolds)
		}
		if r := p.spec.Resources; r == nil || r.CPUMillis != 500 || r.MemoryMiB != 512 {
			t.Errorf("resources = %+v, want 500 millicpu and 512 MiB", r)
		}
		if p.spec.WorkspaceMounts != nil || p.spec.ToolRules != nil || p.spec.EligibleGrants != nil || p.spec.Recording != "" {
			t.Errorf("knobs an MCP sandbox never holds: %+v", p.spec)
		}
	}
}

// TestMCPPolicySameDeniesBrokeredHosts: a server that lists the remote MCP server's host, under a run that
// allows all egress, still has it denied, and the git broker's denies in the run's policy are carried over.
func TestMCPPolicySameDeniesBrokeredHosts(t *testing.T) {
	run := mcpPolicyRun()
	run.AllowAllEgress = true
	run.AllowedDomains = append(run.AllowedDomains, "github.com")
	gitDenies := confineGitBrokerEgress(&run, map[string]uuid.UUID{"github.com": uuid.New()})
	if len(gitDenies) == 0 || slices.Contains(run.AllowedDomains, "github.com") {
		t.Fatalf("fixture: confineGitBrokerEgress = %v, allowed %v", gitDenies, run.AllowedDomains)
	}
	p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, comps: mcpPolicyComps(nil, "api.files.test", "mcp.remote.test")})
	for _, d := range append(slices.Clone(run.DeniedDomains), "mcp.remote.test") {
		if !slices.Contains(p.spec.DeniedDomains, d) {
			t.Errorf("denied %v lacks %s", p.spec.DeniedDomains, d)
		}
	}
}

// TestMCPPolicyNarrowerLeqSame: "narrower" is never wider than "same", and an overlay host "same" no longer
// permits is dropped with a warning.
func TestMCPPolicyNarrowerLeqSame(t *testing.T) {
	allow, off, review := []string{"api.files.test", "gone.test"}, false, types.FirstUseDenyWithReview
	ov := &types.CeilingOverlay{AllowedDomains: &allow, AllowAllEgress: &off, FirstUseApproval: &review}
	run := mcpPolicyRun()
	same := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, comps: mcpPolicyComps(nil)})
	narrower := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, comps: mcpPolicyComps(&types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxNarrower, Overlay: ov})})
	if !composer.Leq(narrower.spec, same.spec, types.GovernanceLimits{}, types.GovernanceLimits{}) {
		t.Fatalf("narrower %+v is not within same %+v", narrower.spec, same.spec)
	}
	if !slices.Equal(narrower.spec.AllowedDomains, []string{"api.files.test"}) {
		t.Errorf("narrower allowed = %v, want [api.files.test]", narrower.spec.AllowedDomains)
	}
	if !slices.ContainsFunc(narrower.warns, func(w string) bool { return strings.Contains(w, "gone.test") }) {
		t.Errorf("warnings %v name no dropped gone.test", narrower.warns)
	}
}

// TestMCPPolicyNarrowerRecomputedAfterSeal: a host the overlay names that the seal removed from the run
// between create and dispatch is absent from "narrower", with a warning, and the derivation does not fail.
func TestMCPPolicyNarrowerRecomputedAfterSeal(t *testing.T) {
	allow := []string{"b.test", "api.files.test"}
	comps := mcpPolicyComps(&types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{AllowedDomains: &allow}})
	run := mcpPolicyRun()
	if refused := mcpNarrowerRefusal(run, comps); refused != nil {
		t.Fatalf("at create the overlay is within same: %+v", refused.body)
	}
	run.AllowedDomains = []string{"a.test"} // the dispatch seal removed b.test
	p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, comps: comps})
	if slices.Contains(p.spec.AllowedDomains, "b.test") {
		t.Errorf("allowed = %v keeps b.test", p.spec.AllowedDomains)
	}
	if !slices.ContainsFunc(p.warns, func(w string) bool { return strings.Contains(w, "b.test") }) {
		t.Errorf("warnings %v name no dropped b.test", p.warns)
	}
}

// mcpOwnCeiling is a member's ceiling: two hosts, one deny, a class floor and a deny rule set.
func mcpOwnCeiling() governanceCeiling {
	return governanceCeiling{Spec: types.RunPolicySpec{
		AllowedDomains: []string{"own.test", "other.test"}, DeniedDomains: []string{"bad.test"}, MinConfinementClass: types.CC2,
		HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: "own.test", Paths: []string{"/keys"}, Effect: types.HostRuleDeny}}}},
		Resources: &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096},
	}, Limits: types.GovernanceLimits{MaxCPUMillis: 250}}
}

// TestMCPPolicyOwnLeqClampCeiling: "own" is clamped to the creator's ceiling, is within it, and its size is
// capped by the creator's per-sandbox caps.
func TestMCPPolicyOwnLeqClampCeiling(t *testing.T) {
	own := &types.RunPolicySpec{AllowedDomains: []string{"own.test", "wide.test"}, Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 256}}
	c := mcpOwnCeiling()
	run := mcpPolicyRun()
	run.MinConfinementClass = types.CC1
	p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: run, ceiling: c,
		comps: mcpPolicyComps(&types.ComponentMCPSandboxPolicy{Mode: types.ComponentMCPSandboxOwn, Spec: own})})
	if !composer.Leq(p.spec, c.Spec, c.Limits, c.Limits) {
		t.Fatalf("own %+v is not within its ceiling %+v", p.spec, c.Spec)
	}
	if slices.Contains(p.spec.AllowedDomains, "wide.test") || !slices.Contains(p.spec.AllowedDomains, "own.test") {
		t.Errorf("own allowed = %v, want own.test and not wide.test", p.spec.AllowedDomains)
	}
	if r := p.spec.Resources; r == nil || r.CPUMillis != 250 || r.MemoryMiB != 256 {
		t.Errorf("resources = %+v, want 250 millicpu (the cap) and the authored 256 MiB", r)
	}
	if own.AllowedDomains[1] != "wide.test" {
		t.Errorf("the definition's own spec was written: %v", own.AllowedDomains)
	}
}

// TestMCPPolicySealRunsLast: in every mode the finish's seal narrows what an earlier step widened: a host the
// server lists, or its own policy authors, that the outer layers do not allow is gone.
func TestMCPPolicySealRunsLast(t *testing.T) {
	allow := []string{"api.files.test", "a.test"}
	layers := []sealLayer{listLayer(policyref.LayerBoundary, []string{"a.test"})}
	for _, pol := range []*types.ComponentMCPSandboxPolicy{
		nil,
		{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{AllowedDomains: &allow}},
		{Mode: types.ComponentMCPSandboxOwn, Spec: &types.RunPolicySpec{AllowedDomains: []string{"api.files.test", "a.test"}}},
	} {
		p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: mcpPolicyRun(), comps: mcpPolicyComps(pol), layers: layers, ungoverned: true})
		if !slices.Equal(p.spec.AllowedDomains, []string{"a.test"}) {
			t.Errorf("%s: allowed = %v, want only the layer's a.test", p.mode, p.spec.AllowedDomains)
		}
	}
}

// TestMCPPolicyResealChangesNothing: in every mode, with an outer layer whose host rules hold h.test, the
// policy holds no hold rule, h.test's rule is deny, the ceiling's deny is in it, and the finish run again on
// its own output gives the same digest.
func TestMCPPolicyResealChangesNothing(t *testing.T) {
	allow := []string{"api.files.test", "a.test", "own.test"}
	layers, deny := mcpHoldLayer(allow...), []string{"Ceiling-Deny.test"}
	for _, pol := range []*types.ComponentMCPSandboxPolicy{
		nil,
		{Mode: types.ComponentMCPSandboxNarrower, Overlay: &types.CeilingOverlay{AllowedDomains: &allow}},
		{Mode: types.ComponentMCPSandboxOwn, Spec: &types.RunPolicySpec{AllowedDomains: []string{"own.test"}}},
	} {
		p := mcpPolicyOf(t, &Server{}, mcpPolicyBase{run: mcpPolicyRun(), comps: mcpPolicyComps(pol), layers: layers, deny: deny, ceiling: mcpOwnCeiling()})
		effects := hostRuleEffects(p.spec.HostRules)
		if effects["h.test"] != types.HostRuleDeny || slices.Contains(slices.Collect(maps.Values(effects)), types.HostRuleHold) {
			t.Errorf("%s: host rules %+v, want h.test deny and no hold", p.mode, p.spec.HostRules)
		}
		if p.holds == 0 {
			t.Errorf("%s: the finish rewrote no hold", p.mode)
		}
		if !slices.ContainsFunc(p.spec.DeniedDomains, func(d string) bool { return strings.EqualFold(d, "ceiling-deny.test") }) {
			t.Errorf("%s: denied %v lacks the ceiling's deny", p.mode, p.spec.DeniedDomains)
		}
		first := p.spec.Digest()
		again := p.spec.Clone()
		mcpPolicyFinish(&again, layers, deny)
		if again.Digest() != first {
			t.Errorf("%s: a second finish changed the policy:\n first %+v\n again %+v", p.mode, p.spec, again)
		}
	}
}

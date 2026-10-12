// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createdToolGate creates a run through the real door, waits for its dispatch
// and boots the proxy from the config dispatch wrote.
func createdToolGate(t *testing.T, body string) toolCallSite {
	t.Helper()
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.AgentImages = composeAgentImages
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	waitFor(t, "the dispatch", func() bool {
		fr.mu.Lock()
		defer fr.mu.Unlock()
		return fr.createCalls == 1
	})
	fr.mu.Lock()
	spec := fr.lastSpec
	fr.mu.Unlock()
	return bootToolGate(t, spec)
}

// doorToolPolicy is what the dry doors resolve for body: the preview spec's tool
// rules and who put each there, and the lane each door's access block shows
// for the harness.
func doorToolPolicy(t *testing.T, body string) (rules []types.ToolRule, sources map[string]string, lanes []string) {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.AgentImages = composeAgentImages // an image carries Claude Code, so the installed-harness check passes
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/policy-preview", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", w.Code, w.Body.String())
	}
	var resp policyPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	sources = map[string]string{}
	for _, p := range resp.Provenance {
		if p.Field == fieldToolRules {
			sources[p.Value] = p.Source.Kind
		}
	}
	for _, door := range []string{"/api/v1/runs/policy-preview", "/api/v1/runs/preflight"} {
		w := do(t, h.srv, http.MethodPost, door, adminToken, body)
		var got struct {
			Access struct {
				Harnesses []struct{ Lane string } `json:"harnesses"`
			} `json:"access"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Access.Harnesses) != 1 {
			t.Fatalf("%s = %d %s", door, w.Code, w.Body.String())
		}
		lanes = append(lanes, got.Access.Harnesses[0].Lane)
	}
	return resp.Spec.ToolRules, sources, lanes
}

// A person's block on a default background Claude Code run is enforced: the
// shared fold puts the run on the gated lane with "*: allow" (provenance
// posture) under the person's layer, so the denied tool is refused at the proxy
// while every tool nobody names still runs, as it did ungated.
func TestToolPolicy_RulesPutAutonomousClaudeOnGatedLane(t *testing.T) {
	const body = `{"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},` +
		`"tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"WebFetch","effect":"deny"}]}],"no_repositories_or_drives":true}`
	rules, sources, lanes := doorToolPolicy(t, body)
	if !slices.Contains(rules, types.ToolRule{Tool: "*", Effect: types.ToolAllow}) || sources["*:allow"] != provKindPosture {
		t.Errorf("preview rules %v sources %v, want the posture's *: allow", rules, sources)
	}
	if !slices.Equal(lanes, []string{toolLaneGated, toolLaneGated}) {
		t.Errorf("preview and preflight lanes %v, want gated at both", lanes)
	}
	gate := createdToolGate(t, body)
	if got := gate.call(t, "WebFetch", ""); got != "DENIED" {
		t.Errorf("the person's blocked tool: %s, want DENIED", got)
	}
	if got := gate.call(t, "Read", ""); got != "APPROVED" {
		t.Errorf("a tool nobody names: %s, want APPROVED", got)
	}
}

// A policy's block on a default background Claude Code run is enforced too: the
// run goes on the gated lane like a person's rule, with "*: allow" under it, so
// the blocked tool is refused at the proxy and every tool the policy does not
// name still runs.
func TestToolPolicy_PolicyDenyPutsBackgroundClaudeOnGatedLane(t *testing.T) {
	const body = `{"agent":"claude-code","repo":"acme/widgets","task":"fix the build","inline_policy":{"min_confinement_class":"CC2","tool_rules":[{"tool":"WebFetch","effect":"deny"}]}}`
	rules, sources, lanes := doorToolPolicy(t, body)
	if !slices.Contains(rules, types.ToolRule{Tool: "*", Effect: types.ToolAllow}) || sources["*:allow"] != provKindPosture {
		t.Errorf("preview rules %v sources %v, want the posture's *: allow", rules, sources)
	}
	if !slices.Equal(lanes, []string{toolLaneGated, toolLaneGated}) {
		t.Errorf("preview and preflight lanes %v, want gated at both", lanes)
	}
	gate := createdToolGate(t, body)
	if got := gate.call(t, "WebFetch", ""); got != "DENIED" {
		t.Errorf("the policy's blocked tool: %s, want DENIED", got)
	}
	if got := gate.call(t, "Read", ""); got != "APPROVED" {
		t.Errorf("a tool the policy does not name: %s, want APPROVED", got)
	}
}

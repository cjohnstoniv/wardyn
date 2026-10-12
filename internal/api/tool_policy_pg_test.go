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

// previewToolRules is what the policy preview resolves for body: the spec's tool
// rules and who put each there.
func previewToolRules(t *testing.T, body string) (rules []types.ToolRule, sources map[string]string) {
	t.Helper()
	h := newHarness(t)
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
	if w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s", w.Code, w.Body.String())
	}
	return resp.Spec.ToolRules, sources
}

// A person's block on a default background Claude Code run is enforced: the
// shared fold puts the run on the gated lane with "*: allow" (provenance
// posture) under the person's layer, so the denied tool is refused at the proxy
// while every tool nobody names still runs, as it did ungated.
func TestToolPolicy_RulesPutAutonomousClaudeOnGatedLane(t *testing.T) {
	const body = `{"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"fix the build"},` +
		`"tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"WebFetch","effect":"deny"}]}],"no_repositories_or_drives":true}`
	rules, sources := previewToolRules(t, body)
	if !slices.Contains(rules, types.ToolRule{Tool: "*", Effect: types.ToolAllow}) || sources["*:allow"] != provKindPosture {
		t.Errorf("preview rules %v sources %v, want the posture's *: allow", rules, sources)
	}
	gate := createdToolGate(t, body)
	if got := gate.call(t, "WebFetch", ""); got != "DENIED" {
		t.Errorf("the person's blocked tool: %s, want DENIED", got)
	}
	if got := gate.call(t, "Read", ""); got != "APPROVED" {
		t.Errorf("a tool nobody names: %s, want APPROVED", got)
	}
}

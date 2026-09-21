// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDispatchModeEnvCarriesTheAutonomyLevel pins the announcement half: the
// sandbox is told the level the launch gate froze on the RUN ROW, never a value
// a dispatch lane passed alongside it, so the env, the file and the audit trail
// cannot name three levels for one run. Absent — not blank — for every run no
// rubric bound, which is the absent-row rule the whole governance surface
// follows.
func TestDispatchModeEnvCarriesTheAutonomyLevel(t *testing.T) {
	for _, c := range []struct {
		name  string
		level types.AutonomyLevel
		want  string // "" means the key must be ABSENT
	}{
		{"gated", types.AutonomyL1, "L1"},
		{"unattended", types.AutonomyL2, "L2"},
		{"unrestricted still says which rung", types.AutonomyL3, "L3"},
		{"nothing bound this run", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{}
			applyDispatchModeEnv(env, types.AgentRun{ID: uuid.New(), AutonomyLevel: c.level}, dispatchParams{})
			got, present := env["WARDYN_AUTONOMY_LEVEL"]
			if c.want == "" {
				if present {
					t.Errorf("Env[WARDYN_AUTONOMY_LEVEL] = %q, want the key absent for an unbound run", got)
				}
				return
			}
			if got != c.want {
				t.Errorf("Env[WARDYN_AUTONOMY_LEVEL] = %q (present=%v), want %q", got, present, c.want)
			}
		})
	}
}

// agentPolicyRowFor dispatches one run at the given agent + level and returns
// the run.agent_policy row dispatch recorded, or nil.
//
// Driven through the REAL dispatchRun rather than by calling
// applyRunAgentPolicy directly: "an L1 claude-code dispatch carries the row, an
// L3 one does not" is a claim about dispatch, and a direct call would still
// pass with the call site deleted.
func agentPolicyRowFor(t *testing.T, agent string, level types.AutonomyLevel) (*types.AuditEvent, map[string]string) {
	t.Helper()
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Agent, run.AutonomyLevel = agent, level
	run.Task = "" // no agent exec / completion watcher: this is about composition
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})
	return findAudit(audit.events, run.ID, "run.agent_policy", "success"), fr.lastSpec.Env
}

// TestAgentPolicyDispatchRecordsGenerationForAGatedRun is the acceptance
// criterion's first half. The row is the only durable record that a managed
// agent-side layer was generated at all, and `delivered:false` is the field
// that keeps it honest while the root-owned delivery contract (#94) is still
// out of this tree: a row that claimed delivery would answer the one question
// an operator reads it to ask.
func TestAgentPolicyDispatchRecordsGenerationForAGatedRun(t *testing.T) {
	ev, env := agentPolicyRowFor(t, "claude-code", types.AutonomyL1)
	if ev == nil {
		t.Fatal("an L1 claude-code dispatch recorded no run.agent_policy row")
	}
	var data struct {
		Agent     string `json:"agent"`
		Level     string `json:"level"`
		Path      string `json:"path"`
		Bytes     int    `json:"bytes"`
		Delivered bool   `json:"delivered"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode run.agent_policy data %s: %v", ev.Data, err)
	}
	_, content, ok := agentpolicy.ForAgent("claude-code", types.AutonomyL1)
	if !ok {
		t.Fatal("agentpolicy.ForAgent(claude-code, L1) generated nothing")
	}
	if data.Agent != "claude-code" || data.Level != "L1" {
		t.Errorf("row names agent %q level %q, want claude-code/L1", data.Agent, data.Level)
	}
	if data.Path != agentpolicy.ClaudeCodeManagedSettingsPath {
		t.Errorf("row path = %q, want %q", data.Path, agentpolicy.ClaudeCodeManagedSettingsPath)
	}
	if data.Bytes != len(content) {
		t.Errorf("row bytes = %d, want %d (the generated document's size)", data.Bytes, len(content))
	}
	if data.Delivered {
		t.Error("row says delivered=true, but nothing in this tree places the file inside a sandbox yet (#94) — " +
			"a row that claims delivery answers the one question it exists to answer, wrongly")
	}
	if env["WARDYN_AUTONOMY_LEVEL"] != "L1" {
		t.Errorf("sandbox Env[WARDYN_AUTONOMY_LEVEL] = %q, want L1 alongside the generated file",
			env["WARDYN_AUTONOMY_LEVEL"])
	}
}

// TestAgentPolicyDispatchWritesNoRowWhereThereIsNoLayer is the acceptance
// criterion's second half plus the two shapes that would make the row noise: an
// unrestricted run, a run no rubric bound, and an agent with no managed-settings
// mechanism to express a level through. None of them has an agent-side layer, so
// none of them gets a governance row saying it did not.
func TestAgentPolicyDispatchWritesNoRowWhereThereIsNoLayer(t *testing.T) {
	for _, c := range []struct {
		name  string
		agent string
		level types.AutonomyLevel
	}{
		{"unrestricted level", "claude-code", types.AutonomyL3},
		{"nothing bound this run", "claude-code", ""},
		{"agent with no managed-settings file", "codex-cli", types.AutonomyL1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if ev, _ := agentPolicyRowFor(t, c.agent, c.level); ev != nil {
				t.Errorf("dispatch recorded run.agent_policy %s for a run with no agent-side layer", ev.Data)
			}
		})
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// agentPolicyDatum is the run.agent_policy payload.
type agentPolicyDatum struct {
	Agent     string `json:"agent"`
	Level     string `json:"level"`
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	Delivered bool   `json:"delivered"`
	Reason    string `json:"reason"`
}

// agentPolicyDispatch dispatches one run at the given agent + level through
// fr and returns the spec the runner received and the run.agent_policy row
// dispatch recorded (nil when there is none).
//
// Driven through the REAL dispatchRun rather than by calling the helpers
// directly: "an L1 claude-code dispatch carries the file and the row, an L3
// one carries neither" is a claim about dispatch, and a direct call would
// still pass with the call site deleted.
func agentPolicyDispatch(t *testing.T, fr *fakeRunner, agent string, level types.AutonomyLevel) (runner.SandboxSpec, *agentPolicyDatum, *dispatchTestStore) {
	t.Helper()
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Agent, run.AutonomyLevel = agent, level
	run.Task = "" // no agent exec / completion watcher: this is about composition
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})
	ev := findAudit(audit.events, run.ID, "run.agent_policy", "success")
	if ev == nil {
		return fr.lastSpec, nil, st
	}
	var data agentPolicyDatum
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode run.agent_policy data %s: %v", ev.Data, err)
	}
	return fr.lastSpec, &data, st
}

// TestAgentPolicyDispatchDeliversTheGatedRunsFile is the acceptance criterion:
// an L1 claude-code run's spec carries the L1 document, byte for byte, at the
// path Claude Code reads, and the row says it was delivered.
func TestAgentPolicyDispatchDeliversTheGatedRunsFile(t *testing.T) {
	spec, data, _ := agentPolicyDispatch(t, &fakeRunner{}, "claude-code", types.AutonomyL1)
	golden, err := os.ReadFile("../agentpolicy/testdata/L1-managed-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.ManagedFiles) != 1 {
		t.Fatalf("spec.ManagedFiles = %d entries, want the one L1 document", len(spec.ManagedFiles))
	}
	mf := spec.ManagedFiles[0]
	if mf.Path != agentpolicy.ClaudeCodeManagedSettingsPath {
		t.Errorf("managed file path = %q, want %q", mf.Path, agentpolicy.ClaudeCodeManagedSettingsPath)
	}
	if !bytes.Equal(mf.Content, golden) {
		t.Errorf("managed file content = %s, want the L1 golden %s", mf.Content, golden)
	}
	if err := runner.ValidateManagedFiles(spec.ManagedFiles); err != nil {
		t.Errorf("the spec's managed files fail the runner contract: %v", err)
	}
	if data == nil {
		t.Fatal("an L1 claude-code dispatch recorded no run.agent_policy row")
	}
	if data.Agent != "claude-code" || data.Level != "L1" || data.Path != agentpolicy.ClaudeCodeManagedSettingsPath {
		t.Errorf("row = %+v, want claude-code/L1 at %s", *data, agentpolicy.ClaudeCodeManagedSettingsPath)
	}
	if data.Bytes != len(golden) {
		t.Errorf("row bytes = %d, want %d", data.Bytes, len(golden))
	}
	if !data.Delivered || data.Reason != "" {
		t.Errorf("row delivered=%v reason=%q, want delivered=true with no reason", data.Delivered, data.Reason)
	}
	if spec.Env["WARDYN_AUTONOMY_LEVEL"] != "L1" {
		t.Errorf("sandbox Env[WARDYN_AUTONOMY_LEVEL] = %q, want L1 alongside the file", spec.Env["WARDYN_AUTONOMY_LEVEL"])
	}
}

// TestAgentPolicyDispatchWithoutTheCapabilityRunsUndelivered: a runner that
// cannot place the file root-owned gets no file — one it could not protect
// would be a ceiling the agent can rewrite — and the run still launches, with
// the row saying so and why.
func TestAgentPolicyDispatchWithoutTheCapabilityRunsUndelivered(t *testing.T) {
	fr := &fakeRunner{noManagedFiles: true}
	spec, data, _ := agentPolicyDispatch(t, fr, "claude-code", types.AutonomyL1)
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1: a runner without the capability still runs the run", fr.createCalls)
	}
	if len(spec.ManagedFiles) != 0 {
		t.Errorf("spec carried %d managed files to a runner that does not deliver them", len(spec.ManagedFiles))
	}
	if data == nil {
		t.Fatal("no run.agent_policy row for a gated run whose file was withheld")
	}
	if data.Delivered {
		t.Error("row says delivered=true, but the runner cannot deliver managed files")
	}
	if !strings.Contains(data.Reason, "does not deliver managed files") {
		t.Errorf("row reason = %q, want it to name the missing runner capability", data.Reason)
	}
}

// TestAgentPolicyDispatchImageRefusalFailsTheRunWithAHint: the Docker
// driver refuses a managed-file run whose image could undo the file (USER
// root, or /etc writable). That refusal must land on the run as its failure
// hint, and no row may claim delivery for a sandbox that never existed.
func TestAgentPolicyDispatchImageRefusalFailsTheRunWithAHint(t *testing.T) {
	refusal := `docker: managed files need an image whose USER is a non-root user; this image (USER "") runs its workload as root, which owns /etc and may rename /etc/claude-code aside and replace the file`
	fr := &fakeRunner{createErr: errors.New(refusal)}
	spec, data, st := agentPolicyDispatch(t, fr, "claude-code", types.AutonomyL1)
	if len(spec.ManagedFiles) != 1 {
		t.Fatal("the file never reached the spec — this test would then pass for the wrong reason")
	}
	if st.state != types.RunFailed {
		t.Errorf("run state = %s, want FAILED", st.state)
	}
	if !strings.Contains(st.failureHint, refusal) {
		t.Errorf("failure hint = %q, want it to carry the driver's refusal %q", st.failureHint, refusal)
	}
	if data != nil {
		t.Errorf("run.agent_policy %+v recorded for a sandbox that was never created", *data)
	}
}

// TestAgentPolicyDispatchCarriesNothingWhereThereIsNoLayer: an unrestricted
// run, a run no rubric bound, and an agent with no managed-settings mechanism
// get neither a file nor a row. A row saying "no file, and there was never
// going to be one" would be noise on every run.
func TestAgentPolicyDispatchCarriesNothingWhereThereIsNoLayer(t *testing.T) {
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
			spec, data, _ := agentPolicyDispatch(t, &fakeRunner{}, c.agent, c.level)
			if len(spec.ManagedFiles) != 0 {
				t.Errorf("spec carried %d managed files for a run with no agent-side layer", len(spec.ManagedFiles))
			}
			if data != nil {
				t.Errorf("dispatch recorded run.agent_policy %+v for a run with no agent-side layer", *data)
			}
		})
	}
}

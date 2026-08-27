// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// D1: `agent` is required EXCEPT for a task_mode=exec run that already names an
// image to run in.
//
// exec mode runs the task as a plain shell command — no agent harness, no model
// call — so naming an agent was a formality that made the CLI read AI-first to
// someone who wanted a governed shell. docs/CI.md demonstrated the workaround it
// forced: `--agent claude-code --image ubuntu:24.04 --task-mode exec`, an exec
// run naming an agent it never uses, in the doc whose whole audience is exec.
func TestCreateRun_AgentOptionalForExec(t *testing.T) {
	t.Run("exec with no agent and no image is 400, naming what is missing", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"task":"echo hi","task_mode":"exec"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
		}
		// The message must say what to DO. "agent is required" is false here —
		// an image would also satisfy it.
		body := w.Body.String()
		if !strings.Contains(body, "image") {
			t.Errorf("the 400 must name the image alternative, got: %s", body)
		}
	})

	t.Run("harness mode with no agent is still 400", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"task":"echo hi","task_mode":"harness","image":"ubuntu:24.04"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("harness mode with no agent must stay 400 (it would come up and run no agent), got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("default (unset) task_mode with no agent is still 400", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"task":"echo hi","image":"ubuntu:24.04"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("only exec mode relaxes the agent requirement, got %d: %s", w.Code, w.Body.String())
		}
	})

	// The relaxation itself: an exec run with an image gets PAST validation.
	// It is asserted as "not the agent-required 400" rather than as a 201,
	// because this cheap harness has no runner and fails later for unrelated
	// reasons — the point is that the agent rule no longer rejects it.
	t.Run("exec with an image and no agent passes the agent gate", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"task":"echo hi","task_mode":"exec","image":"ubuntu:24.04"}`)
		if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "agent is required") {
			t.Fatalf("an exec run naming an image must not be refused for a missing agent: %s", w.Body.String())
		}
	})
}

// 🔴 The security half of D1, and the reason the obvious implementation is
// wrong. Defaulting the empty agent to "claude-code" would have made every
// agentless exec run ELIGIBLE FOR THE OPERATOR'S LIVE SUBSCRIPTION CREDENTIAL:
// req.Agent feeds the managed-subscription eligibility test, and the
// container-login/injection lanes are keyed on the same literal.
//
// This pins that an empty agent stays empty — it is never silently filled in —
// so none of those agent-keyed lanes can select it.
func TestCreateRun_AgentlessExecGetsNoAgentKeyedCredential(t *testing.T) {
	for _, agent := range []string{"", "claude-code"} {
		// agentHarnessLogin is the container-login/injection selector. An empty
		// agent must select nothing; claude-code must still select its lane, so
		// this test fails if the empty case starts resolving like claude-code.
		_, ok := agentHarnessLogin(agent)
		if agent == "" && ok {
			t.Fatalf("an EMPTY agent resolved a harness-login convention — an agentless exec run must never select a credential lane")
		}
		if agent == "claude-code" && !ok {
			t.Fatalf("claude-code lost its harness-login convention; this test can no longer detect the empty case regressing into it")
		}
	}

	// And the image an empty agent would resolve to is not a real one, which is
	// the second reason not to default: agentImage falls back to
	// ghcr.io/cjohnstoniv/agent-<key>, and agent-, agent-byoa and agent-none are
	// all unpublished. An exec run must carry its OWN image.
	if got := agentImage("", nil); !strings.Contains(got, "agent-") {
		t.Fatalf("agentImage(\"\") = %q — unexpected shape", got)
	}
}

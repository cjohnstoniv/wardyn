// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// zero_ai_test.go pins the owner's #1 constraint: Wardyn is not agent-specific.
// A control plane with ZERO integrations and no AI env/secrets configured must
// still be a complete, working sandboxed-execution product: a governed command
// launches, an interactive/record session works, and first-run readiness reads
// green with nothing rendering "you haven't configured an AI agent" as a
// failure or a warning.
//
// Every sub-test below builds its own zero-AI Config: no Secrets, no
// SubscriptionToken, no ManagedToken, and no Bedrock knob touched.

// ─── (a) + (b): dispatch needs a real Store, so this reuses the same
// Postgres-gated harness task_mode_test.go and interactive_test.go already use
// for this class of assertion (pgHarnessWithRunner, interactive_test.go) —
// skips cleanly when WARDYN_TEST_PG is unset, runs for real in an environment
// that sets it. Neither harness call configures Secrets/SubscriptionToken/
// ManagedToken/Composer/Bedrock, so it is already the zero-AI fixture. ───────

// TestZeroAI_ExecGovernedCommandDispatches is Task 1(a): a task_mode:exec
// governed-command run creates and dispatches with no AI integration
// configured at all — Wardyn's baseline product needs no agent/model.
func TestZeroAI_ExecGovernedCommandDispatches(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create exec-mode run with zero AI configured: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.State == types.RunFailed {
		t.Errorf("run state = %q, want a dispatched state, not FAILED — a governed command needs no AI credential", run.State)
	}
	if fr.createCalls != 1 {
		t.Errorf("CreateSandbox calls = %d, want 1 (exec must dispatch with zero AI configured)", fr.createCalls)
	}
	if got := fr.execCount(); got != 1 {
		t.Errorf("Runner.Exec calls = %d, want 1 (the governed command itself must still run)", got)
	}
}

// TestZeroAI_InteractiveRunWorks is Task 1(b): an interactive (Record Mode)
// run dispatches the sandbox with no AI integration configured — a human
// drives the agent shell directly and Wardyn brokers no model access at all.
func TestZeroAI_InteractiveRunWorks(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"ignored when interactive","interactive":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create interactive run with zero AI configured: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.State != types.RunRunning {
		t.Errorf("interactive run state = %q, want RUNNING (idle, awaiting attach)", run.State)
	}
	if fr.createCalls != 1 {
		t.Errorf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	if got := fr.execCount(); got != 0 {
		t.Errorf("Runner.Exec calls = %d, want 0 (interactive skips exec regardless of AI config)", got)
	}
}

// ─── (c) + (d): GET /setup/status ────────────────────────────────────────────

// zeroAIRelatedCheckIDs are the /setup/status rows whose entire concern is "is
// an AI/model/harness provider configured" (setup_checks.go / setup.go).
// Deliberately excluded: agent_image (a container-image TOOLCHAIN fact,
// unrelated to whether any AI credential exists — TestAgentImageCheck in
// setup_test.go pins that the shipped default legitimately reads "warn"
// regardless of AI config) and age_key/host_proxy/scm_provider/runner (each
// grades something with no connection to AI credentials at all).
var zeroAIRelatedCheckIDs = map[string]bool{
	"llm_provider":                true,
	"bedrock_provider":            true,
	"claude_subscription_staging": true,
	"harness_credential":          true,
	"harness_credential_aws":      true,
}

// findCheck looks up one SetupCheck by id — the []SetupCheck twin of
// compose_setup_test.go's findItem ([]SetupItem).
func findCheck(checks []SetupCheck, id string) (SetupCheck, bool) {
	for _, c := range checks {
		if c.ID == id {
			return c, true
		}
	}
	return SetupCheck{}, false
}

// TestZeroAI_SetupStatus is Task 1(c)+(d). The fixture wires ONLY a live
// runner — no Secrets, no SubscriptionToken, no ManagedToken, no Composer, no
// Bedrock knob — and HOME is reset to a scratch dir so a resident CLI login on
// the machine actually running `go test` (this very agent harness, for one —
// see setup_check_ids_test.go's setupCheckIds) cannot smuggle a real
// credential signal into a fixture meant to have none.
func TestZeroAI_SetupStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := New(Config{
		AdminToken: adminToken,
		Runner:     &fakeRunner{}, // the ONLY thing wired
	})
	code, status := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("GET /setup/status: code = %d, want 200; ", code)
	}

	// Sanity: this really is the zero-integrations fixture the invariant is about.
	if len(status.Integrations) != 0 {
		t.Fatalf("Integrations = %+v, want none — this fixture configures zero integrations", status.Integrations)
	}

	t.Run("ready depends only on the runner", func(t *testing.T) {
		if !status.Ready {
			t.Errorf("Ready = false, want true — a live runner is the ONLY readiness gate, and no AI provider/secret/" +
				"subscription/bedrock was configured")
		}
	})

	t.Run("every AI-related check is info-tone, never fail/warn", func(t *testing.T) {
		for _, c := range status.Checks {
			if c.Status == "fail" {
				t.Errorf("check %q (%s) = fail; a zero-AI control plane with a live runner must never render fail anywhere", c.ID, c.Label)
			}
			if zeroAIRelatedCheckIDs[c.ID] && c.Status != "ok" && c.Status != "info" {
				t.Errorf("AI-related check %q status = %q, want ok or info (never fail/warn) — Wardyn is not agent-specific", c.ID, c.Status)
			}
		}
		// llm_provider is the one AI-related row ALWAYS rendered (the rest are
		// gated on some AI signal being present, so they are absent here by
		// construction) — assert it by name so this test cannot pass vacuously
		// if the gating around it ever widens.
		llm, ok := findCheck(status.Checks, "llm_provider")
		if !ok || llm.Status != "info" {
			t.Errorf("llm_provider = %+v (present=%v), want present and info", llm, ok)
		}
	})
}

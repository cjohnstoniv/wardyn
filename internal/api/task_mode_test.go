// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
)

// TestCreateRun_UnknownTaskModeIs400 asserts the closed task_mode enum fails
// closed at the HTTP layer, before any store write (same shape as the
// confinement-class validation).
func TestCreateRun_UnknownTaskModeIs400(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","task_mode":"yolo"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown task_mode, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDispatch_ExecTaskMode_SetsSandboxEnv asserts the exec discriminator
// rides the sandbox env for task_mode=exec and is ABSENT for the default
// harness mode (agent-run branches on WARDYN_TASK_MODE; nothing else about the
// spec may change).
func TestDispatch_ExecTaskMode_SetsSandboxEnv(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","task_mode":"exec"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create exec-mode run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.lastSpec.Env["WARDYN_TASK_MODE"] != "exec" {
		t.Errorf("Env[WARDYN_TASK_MODE] = %q, want exec", fr.lastSpec.Env["WARDYN_TASK_MODE"])
	}

	w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create default-mode run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if v, ok := fr.lastSpec.Env["WARDYN_TASK_MODE"]; ok {
		t.Errorf("Env[WARDYN_TASK_MODE] = %q on a default run, want absent", v)
	}
}

// TestCreateRun_UnknownInteractiveStartIs400 asserts interactive_start gets the
// same closed-enum treatment as task_mode above: fail closed at the HTTP layer,
// before any store write.
func TestCreateRun_UnknownInteractiveStartIs400(t *testing.T) {
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","interactive":true,"interactive_start":"vim"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown interactive_start, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDispatch_InteractiveStart_SetsSandboxEnv is task-mode's interactive
// counterpart: interactive_start=agent rides the sandbox env as
// WARDYN_INTERACTIVE_START (attach-bashrc.sh branches on it), it is ABSENT for
// the default shell start, and — the case that matters — it is absent on a
// BATCH run that asks for it anyway. That last one is what proves the
// `interactive &&` gate in applyDispatchModeEnv, so "ignored for a
// non-interactive run" is structure rather than documentation.
func TestDispatch_InteractiveStart_SetsSandboxEnv(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)

	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","interactive":true,"interactive_start":"agent"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create agent-start run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.lastSpec.Env["WARDYN_INTERACTIVE_START"] != "agent" {
		t.Errorf("Env[WARDYN_INTERACTIVE_START] = %q, want agent", fr.lastSpec.Env["WARDYN_INTERACTIVE_START"])
	}

	w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","interactive":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create default interactive run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if v, ok := fr.lastSpec.Env["WARDYN_INTERACTIVE_START"]; ok {
		t.Errorf("Env[WARDYN_INTERACTIVE_START] = %q on a default interactive run, want absent", v)
	}

	w = do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi","interactive_start":"agent"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create batch run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if v, ok := fr.lastSpec.Env["WARDYN_INTERACTIVE_START"]; ok {
		t.Errorf("Env[WARDYN_INTERACTIVE_START] = %q on a BATCH run, want absent (the interactive gate)", v)
	}
}

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

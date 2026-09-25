// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// Adopted verbatim from the independent 0.8 review (F13,
// REVIEWER-FEEDBACK-0.8.md) — RED on main aa49a242b: an exec run naming a
// single, image-backed workspace via the singular workspace_id was refused
// "agent is required" before seedRequestWorkspace ever got a chance to read
// the workspace's base_image. See #1065.
func TestReviewExecWithSingleWorkspacePassesAgentGate(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: types.Workspace{
		ID:        id,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: "registry", Image: "ubuntu:24.04"},
	}}))
	body := fmt.Sprintf(`{"workspace_id":%q,"task_mode":"exec","task":"echo hi"}`, id.String())
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "agent is required") {
		t.Fatalf("a command with an image-backed workspace must reach workspace/image validation, got: %s", w.Body.String())
	}
}

// The sibling case #1065 names: a workspace with NO base image (the
// "recommended" default — no override) still has nothing for an agent-less
// exec run to run the command in. agentRequirementError defers this question
// to seedRequestWorkspace (post-seed, once base_image had its one chance to
// supply req.Image); this pins the refusal that deferral resolves to.
func TestReviewExecWithNoBaseImageWorkspaceRefusesWithNoImage(t *testing.T) {
	h := newHarness(t)
	id := uuid.New()
	srv := New(baseTestConfig(h, &workspaceStoreFake{ws: types.Workspace{
		ID:      id,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		// No BaseImage at all: an ephemeral-only workspace with no build
		// choice — the "recommended" default, which sets no req.Image.
	}}))
	body := fmt.Sprintf(`{"workspace_id":%q,"task_mode":"exec","task":"echo hi"}`, id.String())
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "attach a workspace") {
		t.Errorf("body = %s, must not tell the caller to attach a workspace — one already is", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no base image to run a command in") {
		t.Errorf("body = %s, want it to name the missing base image", w.Body.String())
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The import flow's Record/Verify runs bind-mount a local_dir workspace. Because
// WorkspaceMount.ReadOnly is a *bool whose SAFE DEFAULT is read-only when omitted,
// wireWorkspaceSource used to leave it nil — mounting EVERY imported workspace
// read-only, with no opt-in anywhere. That silently made the Record step's own
// promise ("so the agent can make changes") impossible to keep: `pnpm install`
// cannot write node_modules, a build cannot emit artifacts, and no source file can
// be edited. These tests pin both directions: read-only stays the default, and an
// operator's explicit Writable opt-in is actually honored.
func TestWireWorkspaceSource_LocalDirReadOnlyByDefault(t *testing.T) {
	s := &Server{}
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{
		ID:     uuid.New(),
		Kind:   types.WorkspaceKindLocalDir,
		Source: "/home/me/projects/thing",
		// Writable deliberately left false.
	}

	s.wireWorkspaceSource(context.Background(), uuid.New(), time.Now(), &run, &policy, ws)

	if len(policy.WorkspaceMounts) != 1 {
		t.Fatalf("want exactly 1 workspace mount, got %d", len(policy.WorkspaceMounts))
	}
	m := policy.WorkspaceMounts[0]
	if m.Source != ws.Source {
		t.Errorf("mount source = %q, want %q", m.Source, ws.Source)
	}
	// The flag must be set EXPLICITLY (not left nil): the effective value is what
	// matters, and ReadOnlyOrDefault must resolve to read-only.
	if m.ReadOnly == nil {
		t.Fatal("ReadOnly must be set explicitly, not left nil")
	}
	if !m.ReadOnlyOrDefault() {
		t.Error("a workspace without the Writable opt-in must mount READ-ONLY")
	}
}

func TestWireWorkspaceSource_WritableOptInIsHonored(t *testing.T) {
	s := &Server{}
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{
		ID:       uuid.New(),
		Kind:     types.WorkspaceKindLocalDir,
		Source:   "/home/me/projects/thing",
		Writable: true, // operator explicitly ticked it
	}

	s.wireWorkspaceSource(context.Background(), uuid.New(), time.Now(), &run, &policy, ws)

	if len(policy.WorkspaceMounts) != 1 {
		t.Fatalf("want exactly 1 workspace mount, got %d", len(policy.WorkspaceMounts))
	}
	if policy.WorkspaceMounts[0].ReadOnlyOrDefault() {
		t.Error("Writable=true must mount the workspace READ-WRITE (install/build/edit)")
	}
	if run.WorkspacePath != ws.Source {
		t.Errorf("run.WorkspacePath = %q, want %q", run.WorkspacePath, ws.Source)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

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
	var run types.AgentRun
	var policy types.RunPolicySpec
	const path = "/home/me/projects/thing"
	ws := types.Workspace{
		ID: uuid.New(),
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: path},
			// Writable deliberately left false.
		},
	}

	if _, _, err := wireWorkspaceSource(&run, &policy, ws); err != nil {
		t.Fatalf("wireWorkspaceSource: %v", err)
	}

	if len(policy.WorkspaceMounts) != 1 {
		t.Fatalf("want exactly 1 workspace mount, got %d", len(policy.WorkspaceMounts))
	}
	m := policy.WorkspaceMounts[0]
	if m.Source != path {
		t.Errorf("mount source = %q, want %q", m.Source, path)
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
	var run types.AgentRun
	var policy types.RunPolicySpec
	const path = "/home/me/projects/thing"
	ws := types.Workspace{
		ID: uuid.New(),
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: path, Writable: true}, // operator explicitly ticked it
		},
	}

	if _, _, err := wireWorkspaceSource(&run, &policy, ws); err != nil {
		t.Fatalf("wireWorkspaceSource: %v", err)
	}

	if len(policy.WorkspaceMounts) != 1 {
		t.Fatalf("want exactly 1 workspace mount, got %d", len(policy.WorkspaceMounts))
	}
	if policy.WorkspaceMounts[0].ReadOnlyOrDefault() {
		t.Error("Writable=true must mount the workspace READ-WRITE (install/build/edit)")
	}
	if run.WorkspacePath != path {
		t.Errorf("run.WorkspacePath = %q, want %q", run.WorkspacePath, path)
	}
}

// TestWireWorkspaceSource_RefIsHonored is the W9-S1-3 regression: a repo
// source's Ref (git branch/tag/sha — part of the source's identity, same as
// Path/Source itself) used to be dropped the instant a repo source became a
// run's types.WorkspaceRepo, so it never reached buildRepoRecords/WARDYN_REPOS
// and no clone ever checked it out — silently cloning the default branch
// regardless of what the source declared. Ref must now survive the same
// wire-through Target already gets.
func TestWireWorkspaceSource_RefIsHonored(t *testing.T) {
	var run types.AgentRun
	var policy types.RunPolicySpec
	ws := types.Workspace{
		ID: uuid.New(),
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/payments", Ref: "release-2.0"},
		},
	}

	if _, _, err := wireWorkspaceSource(&run, &policy, ws); err != nil {
		t.Fatalf("wireWorkspaceSource: %v", err)
	}

	if len(policy.WorkspaceRepos) != 1 {
		t.Fatalf("want exactly 1 workspace repo, got %d", len(policy.WorkspaceRepos))
	}
	if got := policy.WorkspaceRepos[0].Ref; got != "release-2.0" {
		t.Errorf("WorkspaceRepos[0].Ref = %q, want %q", got, "release-2.0")
	}
}

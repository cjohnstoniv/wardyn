// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApplyWorkspaces_Multi: several onboarded selections (dirs + repos) become
// distinct-target mounts + a repo list. First local dir gets /home/agent/work;
// additional dirs get a subdir; first git repo drives run.Repo, additional repos
// ride WorkspaceRepos.
func TestApplyWorkspaces_Multi(t *testing.T) {
	var run composer.RunInput
	var spec types.RunPolicySpec
	wss := []composer.Workspace{
		{Kind: composer.WorkspaceLocal, Path: "/home/me/proj"},
		{Kind: composer.WorkspaceLocal, Path: "/home/me/lib"},
		{Kind: composer.WorkspaceGit, Repo: "octocat/Hello-World"},
		{Kind: composer.WorkspaceGit, Repo: "octocat/Spoon-Knife"},
	}
	if _, code, err := applyWorkspaces(&run, &spec, wss); err != nil {
		t.Fatalf("applyWorkspaces: code=%d err=%v", code, err)
	}

	if len(spec.WorkspaceMounts) != 2 {
		t.Fatalf("want 2 mounts, got %d", len(spec.WorkspaceMounts))
	}
	if spec.WorkspaceMounts[0].Target != "/home/agent/work" {
		t.Errorf("first dir target = %q, want /home/agent/work", spec.WorkspaceMounts[0].Target)
	}
	if spec.WorkspaceMounts[1].Target != "/home/agent/work/lib" {
		t.Errorf("second dir target = %q, want /home/agent/work/lib", spec.WorkspaceMounts[1].Target)
	}
	if run.Repo != "local:proj" {
		t.Errorf("run.Repo = %q, want local:proj (first selection is primary)", run.Repo)
	}
	// First git repo is NOT here (the first selection was a local dir → run.Repo);
	// both git repos ride WorkspaceRepos since neither is index 0.
	if len(spec.WorkspaceRepos) != 2 {
		t.Fatalf("want 2 workspace repos, got %d: %+v", len(spec.WorkspaceRepos), spec.WorkspaceRepos)
	}
}

// TestApplyWorkspaces_GitPrimary: when the first selection is a git repo, it
// ADDITIONALLY drives the legacy run.Repo label — but, like every other git
// selection, it also rides WorkspaceRepos (the corrected contract: the primary
// git workspace must resolve via referencedWorkspaces/wsRefs, which reads
// WorkspaceRepos and never run.Repo — see applyWorkspaces' WorkspaceGit case).
func TestApplyWorkspaces_GitPrimary(t *testing.T) {
	var run composer.RunInput
	var spec types.RunPolicySpec
	wss := []composer.Workspace{
		{Kind: composer.WorkspaceGit, Repo: "octocat/Hello-World"},
		{Kind: composer.WorkspaceGit, Repo: "octocat/Spoon-Knife"},
	}
	if _, _, err := applyWorkspaces(&run, &spec, wss); err != nil {
		t.Fatal(err)
	}
	if run.Repo != "octocat/Hello-World" {
		t.Errorf("run.Repo = %q, want octocat/Hello-World", run.Repo)
	}
	if len(spec.WorkspaceRepos) != 2 ||
		spec.WorkspaceRepos[0].Repo != "octocat/Hello-World" ||
		spec.WorkspaceRepos[1].Repo != "octocat/Spoon-Knife" {
		t.Errorf("WorkspaceRepos = %+v, want [Hello-World, Spoon-Knife] (the primary included)", spec.WorkspaceRepos)
	}
}

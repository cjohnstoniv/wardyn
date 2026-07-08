// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wsRefStore is a minimal store.Store for validateWorkspaceSources tests: it
// embeds the interface (nil — any other method would panic if called) and
// overrides ONLY ListWorkspaces, which is all the validator touches.
type wsRefStore struct {
	store.Store
	ws []types.Workspace
}

func (s wsRefStore) ListWorkspaces(context.Context) ([]types.Workspace, error) { return s.ws, nil }

func TestValidateWorkspaceSources(t *testing.T) {
	onboarded := []types.Workspace{
		{Kind: types.WorkspaceKindLocalDir, Source: "/home/me/project"},
		{Kind: types.WorkspaceKindRepo, Source: "octocat/Hello-World"},
	}
	srv := &Server{cfg: Config{Store: wsRefStore{ws: onboarded}}}
	ro := true
	mount := func(src, tgt string) types.WorkspaceMount {
		return types.WorkspaceMount{Source: src, Target: tgt, ReadOnly: &ro}
	}

	cases := []struct {
		name    string
		spec    types.RunPolicySpec
		wantErr bool
	}{
		{"no user workspaces", types.RunPolicySpec{}, false},
		{"onboarded local dir", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/home/me/project", "/home/agent/work")}}, false},
		{"non-onboarded local dir rejected", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/home/me/other", "/home/agent/work")}}, true},
		{"system .claude mount exempt", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/anything/creds/.claude", claudeCredTarget)}}, false},
		{"system .claude.json mount exempt", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/anything/creds/.claude.json", claudeCredJSONTarget)}}, false},
		{"onboarded repo", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "octocat/Hello-World"}}}, false},
		{"non-onboarded repo rejected", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "evil/repo"}}}, true},
		{"mixed onboarded (dir + system + repo)", types.RunPolicySpec{
			WorkspaceMounts: []types.WorkspaceMount{mount("/home/me/project", "/home/agent/work"), mount("/x/creds/.claude", claudeCredTarget)},
			WorkspaceRepos:  []types.WorkspaceRepo{{Repo: "octocat/Hello-World"}},
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := srv.validateWorkspaceSources(context.Background(), tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got ok")
				}
				if code != http.StatusUnprocessableEntity {
					t.Errorf("code = %d, want 422", code)
				}
			} else if err != nil {
				t.Fatalf("expected ok, got %v (code %d)", err, code)
			}
		})
	}
}

// A nil store fails CLOSED when a user workspace is present (onboarding cannot be
// verified without a store) — but a run with no user workspace still passes.
func TestValidateWorkspaceSources_NilStoreFailsClosed(t *testing.T) {
	srv := &Server{cfg: Config{}} // no Store wired
	ro := true
	withMount := types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
		{Source: "/home/me/project", Target: "/home/agent/work", ReadOnly: &ro},
	}}
	if code, err := srv.validateWorkspaceSources(context.Background(), withMount); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("nil store + user mount must fail closed (422), got code=%d err=%v", code, err)
	}
	if code, err := srv.validateWorkspaceSources(context.Background(), types.RunPolicySpec{}); err != nil {
		t.Fatalf("no user workspace must pass even with a nil store, got code=%d err=%v", code, err)
	}
}

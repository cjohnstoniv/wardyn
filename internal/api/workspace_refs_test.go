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
		{Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/me/project"}}},
		{Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "octocat/Hello-World"}}},
		// A single workspace composed of SEVERAL sources — only possible under the
		// composition model. The membership sets are built from every workspace's
		// Sources (indexWorkspacesBySource), not a single-source Kind/Source mirror,
		// so a multi-source workspace's second/third entry must clear the gate
		// exactly like a single-source workspace's only entry.
		{Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/home/me/multi-dir"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/multi-repo"},
		}},
	}
	srv := &Server{cfg: Config{
		Store: wsRefStore{ws: onboarded},
		// A 0.7 ceiling that still names the retired ~/.claude mounts: blessing
		// them there exempts nothing any more.
		DefaultPolicy: types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/host/creds/.claude", Target: claudeCredTarget},
			{Source: "/host/creds/.claude.json", Target: claudeCredJSONTarget},
		}},
	}}
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
		{"retired .claude mount no longer exempt, even from the ceiling's source", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/host/creds/.claude", claudeCredTarget)}}, true},
		{"retired .claude.json mount no longer exempt, even from the ceiling's source", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/host/creds/.claude.json", claudeCredJSONTarget)}}, true},
		// H8: naming that TARGET with an arbitrary host source (e.g. the host's
		// ~/.ssh) is refused too.
		{"retired target with any other source rejected (H8)", types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{mount("/home/attacker/.ssh", claudeCredTarget)}}, true},
		{"onboarded repo", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "octocat/Hello-World"}}}, false},
		{"non-onboarded repo rejected", types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: "evil/repo"}}}, true},
		{"mixed onboarded (dir + repo)", types.RunPolicySpec{
			WorkspaceMounts: []types.WorkspaceMount{mount("/home/me/project", "/home/agent/work")},
			WorkspaceRepos:  []types.WorkspaceRepo{{Repo: "octocat/Hello-World"}},
		}, false},
		// (a) Both of ONE multi-source workspace's sources clear the gate.
		{"multi-source workspace: both its own sources pass", types.RunPolicySpec{
			WorkspaceMounts: []types.WorkspaceMount{mount("/home/me/multi-dir", "/home/agent/work")},
			WorkspaceRepos:  []types.WorkspaceRepo{{Repo: "acme/multi-repo"}},
		}, false},
		// (b) A path that is NOT any onboarded workspace's source is still refused
		// even alongside a mount that legitimately belongs to a multi-source
		// workspace — membership is per-source, not "the run touched a known workspace".
		{"multi-source workspace: an un-onboarded third source still rejected", types.RunPolicySpec{
			WorkspaceMounts: []types.WorkspaceMount{
				mount("/home/me/multi-dir", "/home/agent/work"),
				mount("/home/me/not-onboarded-at-all", "/home/agent/other"),
			},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason, err := srv.validateWorkspaceSources(context.Background(), tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got ok")
				}
				if code != http.StatusUnprocessableEntity {
					t.Errorf("code = %d, want 422", code)
				}
				// #656 H1: every not-onboarded refusal here shares ONE reason with
				// authorizeSpecWorkspaceSources' own not-onboarded arm — a
				// distinguishable one would be the cross-member existence oracle
				// that byte-identical sentence exists to close.
				if reason != reasonWorkspaceSourceNotOnboarded {
					t.Errorf("reason = %q, want %q", reason, reasonWorkspaceSourceNotOnboarded)
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
	if code, reason, err := srv.validateWorkspaceSources(context.Background(), withMount); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("nil store + user mount must fail closed (422), got code=%d err=%v", code, err)
	} else if reason != reasonWorkspaceSourcesStoreUnavailable {
		t.Errorf("reason = %q, want %q", reason, reasonWorkspaceSourcesStoreUnavailable)
	}
	if code, _, err := srv.validateWorkspaceSources(context.Background(), types.RunPolicySpec{}); err != nil {
		t.Fatalf("no user workspace must pass even with a nil store, got code=%d err=%v", code, err)
	}
}

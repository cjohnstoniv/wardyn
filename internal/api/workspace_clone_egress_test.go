// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A confined replay's baseline allowlist has to cover the clone of EVERY repo
// source the workspace holds. The composition model made that a real gap: the
// old code read ws.Source — the derived mirror, populated only when a workspace
// has exactly one source — so a second ADO/GitLab/self-hosted repo lost its
// clone host and the replay denied the very clone it was launched to prove.
// GitHub sources hide the bug (broker-routed, no egress entry), so the cases
// below deliberately mix a GitHub source in with the others.
func TestWorkspaceCloneEgress_CoversEveryRepoSource(t *testing.T) {
	repo := func(src string) types.WorkspaceSource {
		return types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: src}
	}
	for _, tc := range []struct {
		name    string
		sources []types.WorkspaceSource
		want    []string
	}{
		{
			name:    "single non-GitHub repo",
			sources: []types.WorkspaceSource{repo("https://gitlab.corp.internal/team/api.git")},
			want:    []string{"gitlab.corp.internal"},
		},
		{
			name: "GitHub first, self-hosted second — the masked case",
			sources: []types.WorkspaceSource{
				repo("acme/payments"),
				repo("https://gitlab.corp.internal/team/api.git"),
			},
			want: []string{"gitlab.corp.internal"},
		},
		{
			name: "several non-GitHub repos, all covered, deduped",
			sources: []types.WorkspaceSource{
				repo("https://gitlab.corp.internal/team/api.git"),
				repo("https://ado.corp.internal/team/web.git"),
				repo("https://gitlab.corp.internal/team/lib.git"),
			},
			want: []string{"gitlab.corp.internal", "ado.corp.internal"},
		},
		{
			name:    "GitHub only — nothing to allow (the broker is on-segment)",
			sources: []types.WorkspaceSource{repo("acme/payments"), repo("acme/web")},
			want:    nil,
		},
		{
			name: "no repo sources at all",
			sources: []types.WorkspaceSource{
				{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/app"},
				{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workspaceCloneEgress(types.Workspace{Sources: tc.sources})
			if len(got) != len(tc.want) {
				t.Fatalf("hosts = %v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing clone host %q (got %v)", w, got)
				}
			}
		})
	}
}

// confinedEgressDomains unions the clone hosts with the workspace's own trusted
// egress; the clone half must not regress to the single-source mirror.
func TestConfinedEgressDomains_UnionsCloneHostsAndProfile(t *testing.T) {
	ws := types.Workspace{
		Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/payments"},
			{Type: types.WorkspaceSourceTypeRepo, Source: "https://gitlab.corp.internal/team/api.git"},
		},
		Profile:        mustJSON(map[string]any{"egress_domains": []string{"registry.npmjs.org"}}),
		ApprovedEgress: []string{"api.stripe.com"},
	}
	got := confinedEgressDomains(ws)
	for _, want := range []string{"gitlab.corp.internal", "registry.npmjs.org", "api.stripe.com"} {
		if !slices.Contains(got, want) {
			t.Errorf("confined allowlist missing %q (got %v)", want, got)
		}
	}
}

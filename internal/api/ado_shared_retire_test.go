// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRetiredADOSharedSecretNames: the sweep names the token, key and
// known-hosts secret of every Azure DevOps host — the well-known ones, each
// provider row's (disabled ones included), and each visualstudio.com scm_hosts
// entry — and never another forge's, nor a name Wardyn's own Azure DevOps
// credentials are stored under.
func TestRetiredADOSharedSecretNames(t *testing.T) {
	sc := types.SiteConfig{
		ScmHosts: []string{"github.com", "Beta.VisualStudio.com", "git.corp.example"},
		WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
			{ID: "ados", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://tfs.corp.example/acme"}},
			{ID: "ado", Kind: types.GitProviderAzureDevOps, BaseURLs: []string{"https://dev.azure.com/acme", "https://acme.visualstudio.com"}},
			{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"}},
		}},
	}
	got := RetiredADOSharedSecretNames(sc)
	for _, want := range []string{
		"git-pat-dev-azure-com", "ssh-key-dev-azure-com", "known-hosts-dev-azure-com",
		"git-pat-ssh-dev-azure-com", "ssh-key-ssh-dev-azure-com", "known-hosts-ssh-dev-azure-com",
		"git-pat-vs-ssh-visualstudio-com", "ssh-key-vs-ssh-visualstudio-com",
		"git-pat-tfs-corp-example", "ssh-key-tfs-corp-example", "known-hosts-tfs-corp-example",
		"git-pat-acme-visualstudio-com", "git-pat-beta-visualstudio-com",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the sweep does not name %s; names = %v", want, got)
		}
	}
	for _, never := range []string{"git-pat-github-com", "ssh-key-github-com", "git-pat-git-corp-example", "wardyn-harness-ado-ado-oauth"} {
		if slices.Contains(got, never) {
			t.Errorf("the sweep names %s, which is not a shared Azure DevOps credential", never)
		}
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("names are not sorted and unique: %v", got)
	}
	if bare := RetiredADOSharedSecretNames(types.SiteConfig{}); !slices.Contains(bare, "git-pat-dev-azure-com") || slices.Contains(bare, "git-pat-tfs-corp-example") {
		t.Errorf("with no rows the sweep names %v, want the well-known hosts only", bare)
	}
}

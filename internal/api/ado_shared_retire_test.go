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
	got, skipped := RetiredADOSharedSecretNames(sc)
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
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none: no other forge shares an Azure DevOps host here", skipped)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("names are not sorted and unique: %v", got)
	}
	bare, _ := RetiredADOSharedSecretNames(types.SiteConfig{})
	if !slices.Contains(bare, "git-pat-dev-azure-com") || slices.Contains(bare, "git-pat-tfs-corp-example") {
		t.Errorf("with no rows the sweep names %v, want the well-known hosts only", bare)
	}
}

// TestRetiredADOSharedSecretNames_SpareOtherForges (#1429 review F1): a host
// that a GitHub row also names — the same host, or one that slugs alike — is not
// on the list, and comes back as skipped so the sweep can say so. github.com is
// never swept, whatever an Azure DevOps row says.
func TestRetiredADOSharedSecretNames_SpareOtherForges(t *testing.T) {
	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		{ID: "ados", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://git.corp.example/tfs"}},
		{ID: "ghes", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/org"}},
		{ID: "ados2", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://tfs.corp.example/acme"}},
		{ID: "ghes2", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://tfs-corp.example/org"}},
		{ID: "ados3", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://ado.corp.example/acme"}},
		{ID: "ados4", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://github.com/x"}},
	}}}
	names, skipped := RetiredADOSharedSecretNames(sc)
	for _, never := range []string{"git-pat-git-corp-example", "ssh-key-git-corp-example", "known-hosts-git-corp-example",
		"git-pat-tfs-corp-example", "git-pat-github-com", "ssh-key-github-com"} {
		if slices.Contains(names, never) {
			t.Errorf("the sweep names %s, which another forge's row also claims", never)
		}
	}
	if !slices.Contains(names, "git-pat-ado-corp-example") || !slices.Contains(names, "git-pat-dev-azure-com") {
		t.Errorf("names = %v, want the unclaimed Azure DevOps hosts kept", names)
	}
	if want := []string{"git.corp.example", "github.com", "tfs.corp.example"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
}

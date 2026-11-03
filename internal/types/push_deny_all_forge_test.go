// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"slices"
	"testing"
)

// The deny_all guard's self-hosted code hosts come from the site's SCM hosts,
// every Git provider row (a disabled one too; a GitHub row brings its API,
// upload and gist hosts), and the run's git_pat and ssh_key grants. Other
// grants name no code host.
func TestDenyAllForgeHosts(t *testing.T) {
	scope := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	site := SiteConfig{
		ScmHosts: []string{"Bitbucket.Corp.Example.", "dev.azure.com"},
		WorkspaceProviders: &WorkspaceProviders{Git: []GitProvider{
			{ID: "ghes", Kind: GitProviderGitHub, BaseURLs: []string{"https://ghe.corp.example/acme"}},
			{ID: "ados", Kind: GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://tfs.corp.example/tfs"}},
		}},
	}
	grants := []GrantSpec{
		{Kind: GrantGitPAT, Scope: scope(map[string]string{"host": "gitlab.corp.example", "secret_name": "pat"})},
		{Kind: GrantSSHKey, Scope: scope(map[string]string{"host": "git.corp.example:2222", "key_secret_ref": "k"})},
		{Kind: GrantEnvSecret, Scope: scope(map[string]string{"host": "not.a.forge.example", "name": "X"})},
		{Kind: GrantAPIKey, Scope: scope(map[string]string{"host": "api.vendor.example"})},
	}
	want := []string{"api.ghe.corp.example", "bitbucket.corp.example", "dev.azure.com", "ghe.corp.example",
		"gist.ghe.corp.example", "git.corp.example", "gitlab.corp.example", "tfs.corp.example", "uploads.ghe.corp.example"}
	if got := DenyAllForgeHosts(site, grants); !slices.Equal(got, want) {
		t.Fatalf("DenyAllForgeHosts = %q, want %q", got, want)
	}
	if got := DenyAllForgeHosts(SiteConfig{}, nil); got != nil {
		t.Fatalf("no hosts: %q, want nil", got)
	}
}

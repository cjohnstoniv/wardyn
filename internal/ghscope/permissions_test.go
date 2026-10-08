// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"maps"
	"slices"
	"testing"
)

// TestPermissionsForGolden is the capability -> GitHub permission table,
// written out rather than derived from the map it tests. The keys and levels
// are those GitHub's "Create an installation access token for an app" request
// lists (docs.github.com/en/rest/apps/apps); the combinations are the
// endpoint permissions on "Permissions required for GitHub Apps"
// (docs.github.com/en/rest/authentication/permissions-required-for-github-apps)
// and the git access rule on "Choosing permissions for a GitHub App"
// (contents, plus workflows to change a workflow file).
func TestPermissionsForGolden(t *testing.T) {
	m := func(kv ...string) map[string]string {
		out := map[string]string{"metadata": "read"}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	for _, tc := range []struct {
		name string
		caps []Capability
		want map[string]string
	}{
		{"nothing is metadata, never an empty request", nil, m()},
		{"identity needs no permission", []Capability{CapIdentity}, m()},
		{"code_read reads pull requests too", []Capability{CapCodeRead}, m("contents", "read", "pull_requests", "read")},
		{"code_write", []Capability{CapCodeWrite}, m("contents", "write")},
		{"workflows_write is a combination", []Capability{CapWorkflowsWrite}, m("contents", "write", "workflows", "write")},
		{"pr", []Capability{CapPR}, m("pull_requests", "write")},
		{"issues_read", []Capability{CapIssuesRead}, m("issues", "read")},
		{"issues_write", []Capability{CapIssuesWrite}, m("issues", "write")},
		{"actions_read", []Capability{CapActionsRead}, m("actions", "read")},
		{"actions_execute", []Capability{CapActionsExecute}, m("actions", "write")},
		{"actions_admin shares actions_execute's permission", []Capability{CapActionsAdmin}, m("actions", "write")},
		{"packages_read", []Capability{CapPackagesRead}, m("packages", "read")},
		{"packages_write", []Capability{CapPackagesWrite}, m("packages", "write")},
		{"repo_admin", []Capability{CapRepoAdmin}, m("administration", "write")},
		{"org_read", []Capability{CapOrgRead}, m("members", "read")},
		{"org_admin is a combination", []Capability{CapOrgAdmin}, m("members", "write", "organization_administration", "write")},
		{"write wins over read on one key", []Capability{CapCodeWrite, CapCodeRead}, m("contents", "write", "pull_requests", "read")},
		{"order does not matter", []Capability{CapCodeRead, CapCodeWrite, CapPR}, m("contents", "write", "pull_requests", "write")},
		{"the default profile", ProfileDefault(), m("contents", "read", "pull_requests", "read")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PermissionsFor(tc.caps)
			if err != nil || !maps.Equal(got, tc.want) {
				t.Fatalf("PermissionsFor(%v) = %v, %v, want %v", tc.caps, got, err, tc.want)
			}
		})
	}
}

// A capability that is not grantable is an error, never an empty set — an
// empty permission request is GitHub's widest token.
func TestPermissionsForRefusesWhatNobodyHolds(t *testing.T) {
	for _, c := range []Capability{CapMetadata, CapRepoContentWriteREST, CapDeniedSecrets, CapUnclassifiedWrite, CapUnclassifiedRead, "", "admin"} {
		if got, err := PermissionsFor([]Capability{CapCodeRead, c}); err == nil {
			t.Errorf("PermissionsFor(code_read, %q) = %v, want an error", c, got)
		}
	}
}

// Every grantable capability has a row, and no row asks for a permission this
// catalogue refuses the routes of, or for a level the installation-token
// request does not list for that key.
func TestPermissionTableCoversTheCatalogue(t *testing.T) {
	levels := map[string][]string{
		"contents": {"read", "write"}, "workflows": {"write"}, "pull_requests": {"read", "write"},
		"issues": {"read", "write"}, "actions": {"read", "write"}, "packages": {"read", "write"},
		"administration": {"read", "write"}, "members": {"read", "write"},
		"organization_administration": {"read", "write"},
	}
	for _, c := range GrantableCapabilities() {
		row, ok := capabilityPermissions[c]
		if !ok {
			t.Errorf("%s has no permission row", c)
		}
		for k, v := range row {
			if !slices.Contains(levels[k], v) {
				t.Errorf("%s asks %s: %s, which this catalogue does not grant", c, k, v)
			}
		}
	}
	for c := range capabilityPermissions {
		if !c.Grantable() {
			t.Errorf("permission row for %q, which is not grantable", c)
		}
	}
}

func TestCatalogue(t *testing.T) {
	if got := GrantableCapabilities(); len(got) != 15 || !slices.IsSorted(got) {
		t.Fatalf("GrantableCapabilities() = %v, want the 15 grantable capabilities in order", got)
	}
	all := append(GrantableCapabilities(), CapMetadata, CapDeniedTokens, CapDeniedHooks, CapDeniedSecrets,
		CapDeniedAccount, CapDeniedSearch, CapDeniedGraphQL, CapRepoContentWriteREST, CapUnclassifiedWrite, CapUnclassifiedRead)
	for _, c := range all {
		if !c.Valid() || Label(c) == "" {
			t.Errorf("%q: valid=%v label=%q", c, c.Valid(), Label(c))
		}
		if c.Grantable() && c.Denied() {
			t.Errorf("%q is both grantable and denied", c)
		}
	}
	for _, c := range []Capability{"", "admin", "read", "Code_Read"} {
		if c.Valid() || c.Grantable() || Label(c) != "" {
			t.Errorf("%q reads as a capability", c)
		}
	}
	if !slices.Equal(ProfileDefault(), []Capability{CapCodeRead}) {
		t.Errorf("ProfileDefault() = %v, want [code_read]", ProfileDefault())
	}
	if GrantableCapabilityList() == "" {
		t.Error("GrantableCapabilityList() is empty")
	}
}

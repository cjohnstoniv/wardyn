// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestWidenCeilingRepoAllowlist_NeverMutatesItsInput: the ceiling passed in is
// commonly s.cfg.DefaultPolicy, so a widening written into it in place would
// hand every later run the repos one Record Mode profile grounded.
func TestWidenCeilingRepoAllowlist_NeverMutatesItsInput(t *testing.T) {
	ceiling := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"permissions":{"contents":"read"}}`)},
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/named"]}`)},
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example.com"}`)},
	}}
	before := make([][]byte, len(ceiling.EligibleGrants))
	for i, g := range ceiling.EligibleGrants {
		before[i] = bytes.Clone(g.Scope)
	}

	out := widenCeilingRepoAllowlist(ceiling, []string{"acme/app"})

	for i, g := range ceiling.EligibleGrants {
		if !bytes.Equal(g.Scope, before[i]) {
			t.Fatalf("input grant %d scope = %s, was %s: the shared ceiling was written in place", i, g.Scope, before[i])
		}
	}
	if got := ghScopeRepos(out.EligibleGrants[0].Scope); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("the repo-less github_token grant was not widened: repos = %v", got)
	}
	var perms struct {
		Permissions map[string]string `json:"permissions"`
	}
	_ = json.Unmarshal(out.EligibleGrants[0].Scope, &perms)
	if perms.Permissions["contents"] != "read" {
		t.Errorf("widening dropped the grant's permissions: %s", out.EligibleGrants[0].Scope)
	}
	if !bytes.Equal(out.EligibleGrants[1].Scope, before[1]) {
		t.Errorf("a ceiling grant that already names repos was changed: %s", out.EligibleGrants[1].Scope)
	}
	if !bytes.Equal(out.EligibleGrants[2].Scope, before[2]) {
		t.Errorf("a grant of another kind was changed: %s", out.EligibleGrants[2].Scope)
	}
}

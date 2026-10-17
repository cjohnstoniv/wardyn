// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func githubAPIGrant(t *testing.T, caps ...ghscope.Capability) types.GrantSpec {
	t.Helper()
	scope, err := json.Marshal(map[string]any{"repos": []string{"acme/app"}, "api": true, "capabilities": caps})
	if err != nil {
		t.Fatal(err)
	}
	return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 3600}
}

func mintGitHubAPI(t *testing.T, gh *FakeGitHubMinter, spec types.GrantSpec) error {
	t.Helper()
	db := newFakeDB()
	b := New(db, nil, &fakeAudit{}, nil, gh)
	runID := uuid.New()
	_, err := b.MintForGrant(context.Background(), callerFor(runID), seedGrant(db, runID, spec))
	return err
}

// The CLI grant's token carries what its capabilities need, narrowed to the
// App's own grant — never the git lane's contents/pull_requests clamp. Issues
// is a permission the clamp drops; actions is one the App here lacks.
func TestGitHubAPI_PermissionsFromCapabilities(t *testing.T) {
	gh := &FakeGitHubMinter{Token: "ghs_cli", AppPermissions: map[string]string{
		"metadata": "read", "contents": "write", "issues": "write", "pull_requests": "read",
	}}
	if err := mintGitHubAPI(t, gh, githubAPIGrant(t, ghscope.CapIssuesRead, ghscope.CapCodeRead, ghscope.CapActionsExecute)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	want := map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read", "issues": "read"}
	if !maps.Equal(gh.LastPermissions, want) {
		t.Fatalf("minted permissions %v, want %v", gh.LastPermissions, want)
	}
}

// A capability the CLI may never hold refuses the mint rather than being
// dropped, and so does an empty list or an unreadable App grant: none of them
// may become a wider or an unbounded token.
func TestGitHubAPI_ScopeRefusals(t *testing.T) {
	app := map[string]string{"metadata": "read", "administration": "write", "issues": "write"}
	for name, tc := range map[string]struct {
		gh   *FakeGitHubMinter
		caps []ghscope.Capability
	}{
		"refused capability": {&FakeGitHubMinter{AppPermissions: app}, []ghscope.Capability{ghscope.CapIssuesRead, ghscope.CapRepoAdmin}},
		"no capability":      {&FakeGitHubMinter{AppPermissions: app}, nil},
		"app unreadable":     {&FakeGitHubMinter{AppPermissionsErr: ErrGitHubRefused}, []ghscope.Capability{ghscope.CapIssuesRead}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := mintGitHubAPI(t, tc.gh, githubAPIGrant(t, tc.caps...)); err == nil {
				t.Fatal("mint succeeded")
			}
			if tc.gh.Calls != 0 {
				t.Fatalf("a token was requested from GitHub %d times", tc.gh.Calls)
			}
		})
	}
}

// Only create authors a CLI grant: a policy that sets api or capabilities is
// refused at write.
func TestGitHubAPI_PolicyCannotAuthorScope(t *testing.T) {
	for _, scope := range []string{
		`{"repos":["acme/app"],"api":true}`,
		`{"repos":["acme/app"],"capabilities":["issues_read"]}`,
	} {
		err := ValidateGitHubScopeShape(json.RawMessage(scope))
		if err == nil || !strings.Contains(err.Error(), "GitHub CLI component") {
			t.Errorf("%s: err = %v, want the CLI-only refusal", scope, err)
		}
	}
}

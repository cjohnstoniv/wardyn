// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// traversalLocators are the shapes V1 lens A proved admitted past an org-scoped
// base URL. The server compared the DECODED, unsquashed path against the row's
// prefix; the sandbox's git squashed the dot segments client-side and sent %2F
// raw, so `https://github.com/acme/../evil/repo.git` reached github.com as
// `/evil/repo.git` and the PAT broker minted the acme org credential for it.
//
// One list, three surfaces: the admission chokepoint (parseCloneTarget), the two
// write doors, and the end-to-end run-create leg below all read it, so a shape
// added here has to be refused everywhere or this file goes red.
var traversalLocators = []struct {
	name    string
	locator string
}{
	{"parent dot segment", "https://github.com/acme/../evil/repo.git"},
	{"mixed . and .. segments", "https://github.com/acme/./x/../../evil/repo.git"},
	{"percent-encoded separator", "https://github.com/acme%2Fevil/repo.git"},
	{"empty segment", "https://github.com/acme//../evil/repo.git"},
	{"backslash", `https://github.com/acme\..\evil/repo.git`},
	{"scp form", "git@github.com:acme/../evil/repo.git"},
	{"ssh:// form", "ssh://git@github.com/acme/%2e%2e/evil/repo.git"},
	{"bare slug", "acme/../evil"},
}

// TestRepoLocatorPathSafeAdmitsOrdinaryAddresses is the other direction: the
// guard must not refuse a repository address anybody actually writes. A rule
// that fails closed on `octocat/Hello-World` would be refused by every door in
// the tree at once, so the admitted list is pinned beside the refused one.
func TestRepoLocatorPathSafeAdmitsOrdinaryAddresses(t *testing.T) {
	for _, ok := range []string{
		"octocat/Hello-World",
		"https://github.com/acme/app.git",
		"https://github.com/acme/app/",
		"http://git.corp.example/acme/sub/app.git",
		"https://user@github.com/acme/app.git",
		"git@github.com:acme/app.git",
		"ssh://git@ssh.dev.azure.com/v3/acme/proj/repo",
		"https://dev.azure.com/acme/_git/repo",
		"https://github.com", // no path at all: nothing to traverse
		"",
	} {
		if !repoLocatorPathSafe(ok) {
			t.Errorf("repoLocatorPathSafe(%q) = false, want true — an ordinary repository address", ok)
		}
	}
	for _, tc := range traversalLocators {
		if repoLocatorPathSafe(tc.locator) {
			t.Errorf("%s: repoLocatorPathSafe(%q) = true, want false", tc.name, tc.locator)
		}
	}
}

// TestTraversalLocatorsRefusedAtBothWriteDoors is the AUTHORING half: a
// never-clonable locator is a 400 ("you wrote this wrong") at the doors that
// store one, regardless of provider mode — there is no provider policy under
// which `…/acme/../evil/repo.git` is a legitimate repository address, so it must
// not be storable and waiting for a row to widen.
func TestTraversalLocatorsRefusedAtBothWriteDoors(t *testing.T) {
	for _, tc := range traversalLocators {
		t.Run(tc.name, func(t *testing.T) {
			lib := validateSourceWrite(types.Source{Kind: types.SourceRepo, Name: "n", Locator: tc.locator})
			if want := fmt.Sprintf(repo400LocatorShape, "locator"); lib != want {
				t.Errorf("validateSourceWrite = %q, want the DRAFT shape sentence %q", lib, want)
			}
			ws := validateWorkspaceSource(types.WorkspaceSource{
				Type: types.WorkspaceSourceTypeRepo, Source: tc.locator})
			if want := fmt.Sprintf(repo400LocatorShape, "source"); ws != want {
				t.Errorf("validateWorkspaceSource = %q, want the DRAFT shape sentence %q", ws, want)
			}
		})
	}
	// …and an ordinary address still passes both, so the guard is not simply
	// refusing every repo source.
	if msg := validateSourceWrite(types.Source{Kind: types.SourceRepo, Name: "n", Locator: "acme/app"}); msg != "" {
		t.Errorf("validateSourceWrite(acme/app) = %q, want accepted", msg)
	}
	if msg := validateWorkspaceSource(types.WorkspaceSource{
		Type: types.WorkspaceSourceTypeRepo, Source: "acme/app"}); msg != "" {
		t.Errorf("validateWorkspaceSource(acme/app) = %q, want accepted", msg)
	}
}

// TestTraversalRepoRefusedEndToEndAtRunCreate is the blocker's own end-to-end
// pin: a MEMBER whose workspace sits under the `acme` row POSTs a run over the
// resolved spec naming a traversal URL, and gets the member refusal — not a 201
// with the org's PAT wired for `evil/repo`. No run row is created at all, which
// is what proves persistRunGrants and augmentGitBrokerGrants never ran.
func TestTraversalRepoRefusedEndToEndAtRunCreate(t *testing.T) {
	const traversal = "https://github.com/acme/../evil/repo.git"
	door := admitRunDoor("POST /runs (resolved spec)", true, func(repo string) string {
		return `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
			`"workspace_repos":[{"repo":` + quote(repo) + `,"target":"/home/agent/work/app"}]}}`
	})

	// The control: the SAME door, the same row, the repository the admin
	// actually scoped — admitted. Without this leg a fixture that refused
	// everything would pass the assertion below for the wrong reason.
	_, w := door.fire(t, admitSite(), admitOnRow, false)
	assertNotAdmissionRefused(t, w, "a repository inside the row's base URL is admitted")

	_, w = door.fire(t, admitSite(), traversal, false)
	assertMemberRefused(t, w)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a member: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, admitBaseURL) || strings.Contains(body, "evil") {
		t.Errorf("body = %s, must name neither the base URL nor the traversed org", body)
	}

	// The OPERATOR half of the same request: 422, and still no admission.
	_, wOp := door.fire(t, admitSite(), traversal, true)
	if wOp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("operator status = %d, want 422: %s", wOp.Code, wOp.Body.String())
	}
}

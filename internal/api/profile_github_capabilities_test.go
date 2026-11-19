// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member's ceiling whose github_capabilities is [code_read] bounds a run that names none: the
// run's GitHub pushes are withheld, the git token is minted read-only, and the GitHub CLI
// (a code_read+code_write row) keeps code_read alone.
func TestCeilingGitHubCapabilitiesBoundTheMemberRun(t *testing.T) {
	f, ref := githubCLIWallsFixtureWith(t, nil, func(p *types.RunPolicySpec) {
		p.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead}
	})
	fr := f.srv.cfg.Runner.(*fakeRunner)
	body := componentBody(ref)
	body["repo"] = "acme/app"
	if w := f.ask(t, componentDoors[0], body); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	fr.waitForSandbox(t)
	fr.mu.Lock()
	pc := fr.lastSpec.ProxyConfig
	fr.mu.Unlock()
	if want := map[string]string{"github.com": "GitHub"}; !maps.Equal(pc.GitPushWithheld, want) {
		t.Errorf("GitPushWithheld = %v, want %v", pc.GitPushWithheld, want)
	}
	if pc.GitHubAPI == nil || !slices.Equal(pc.GitHubAPI.Capabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Fatalf("GitHubAPI = %+v, want the door with code_read alone", pc.GitHubAPI)
	}
	gitID, doorID := pc.GitGrants["acme/app"], pc.GitHubAPI.Grants["acme"]
	var sawGit, sawDoor bool
	for _, g := range f.st.govEscapeStore.grants {
		switch g.ID {
		case gitID:
			var sc struct {
				Permissions map[string]string `json:"permissions"`
			}
			if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil || sc.Permissions["contents"] != "read" {
				t.Errorf("git grant scope %s (%v), want contents read", g.Spec.Scope, err)
			}
			sawGit = true
		case doorID:
			if got := string(g.Spec.Scope); got != `{"api":true,"capabilities":["code_read"],"repos":["acme/app"]}` {
				t.Errorf("door grant scope %s, want code_read alone", got)
			}
			sawDoor = true
		}
	}
	if !sawGit || !sawDoor {
		t.Errorf("grant rows written: git %v, door %v; want both", sawGit, sawDoor)
	}
}

// A ceiling list that shares nothing with the GitHub CLI's row refuses the run, in plain words,
// rather than leaving the door on the row's capabilities.
func TestCeilingGitHubCapabilitiesDisjointFromTheCLIIsRefused(t *testing.T) {
	f, ref := githubCLIWallsFixtureWith(t, nil, func(p *types.RunPolicySpec) {
		p.GitHubCapabilities = []ghscope.Capability{ghscope.CapIssuesWrite}
	})
	body := componentBody(ref)
	body["repo"] = "acme/app"
	w := f.ask(t, componentDoors[0], body)
	got := decodeErrorBody(t, w)
	if w.Code != http.StatusForbidden || got.Reason != reasonGovernanceOverlayUnsatisfiable || !strings.Contains(got.Error, "github_capabilities") {
		t.Fatalf("create = %d %+v, want 403 %s naming github_capabilities", w.Code, got, reasonGovernanceOverlayUnsatisfiable)
	}
}

// A member's list that shares nothing with their ceiling's is refused before the clamp: an empty
// meet would read as the default, so it never runs. A shared entry, or no list on either side,
// goes on to the clamp.
func TestGitHubCapabilitiesDisjoint(t *testing.T) {
	caps := func(c ...ghscope.Capability) []ghscope.Capability { return c }
	r, w, iw := ghscope.CapCodeRead, ghscope.CapCodeWrite, ghscope.CapIssuesWrite
	if ref := githubCapabilitiesDisjoint(caps(iw), caps(r)); ref == nil || ref.status != http.StatusForbidden ||
		ref.body.Reason != reasonGovernanceOverlayUnsatisfiable || !strings.Contains(ref.body.Error, "github_capabilities (issues_write)") {
		t.Errorf("disjoint = %+v, want 403 %s naming the list", ref, reasonGovernanceOverlayUnsatisfiable)
	}
	for name, tc := range map[string][2][]ghscope.Capability{
		"shares code_read":  {caps(r, w), caps(r)},
		"member names none": {nil, caps(r)},
		"ceiling silent":    {caps(iw), nil},
	} {
		if ref := githubCapabilitiesDisjoint(tc[0], tc[1]); ref != nil {
			t.Errorf("%s: refused %+v, want it left to the clamp", name, ref)
		}
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// launchGitLaneUnderWalls creates a run on acme/app through the doors and answers the proxy's
// GitPushWithheld and the contents permission of the git lane's grant.
func launchGitLaneUnderWalls(t *testing.T, walls *types.PersonalWallsSpec) (map[string]string, string) {
	t.Helper()
	f, ref := githubCLIWallsFixture(t, walls)
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
	id, ok := pc.GitGrants["acme/app"]
	if !ok {
		t.Fatalf("GitGrants = %v, want acme/app brokered", pc.GitGrants)
	}
	for _, g := range f.st.govEscapeStore.grants {
		if g.ID == id {
			var sc struct {
				Permissions map[string]string `json:"permissions"`
			}
			if err := json.Unmarshal(g.Spec.Scope, &sc); err != nil {
				t.Fatalf("git grant scope %s: %v", g.Spec.Scope, err)
			}
			return pc.GitPushWithheld, sc.Permissions["contents"]
		}
	}
	t.Fatal("the git lane's grant row was not written")
	return nil, ""
}

// A wall without code_write withholds GitHub pushes: the proxy refuses them and the git lane's
// token is minted read-only. A wall with code_write, and no wall, leave the lane as it was.
func TestWallsWithoutCodeWriteWithholdGitHubPushes(t *testing.T) {
	read, write := ghscope.CapCodeRead, ghscope.CapCodeWrite
	for _, tc := range []struct {
		name     string
		walls    *types.PersonalWallsSpec
		withheld map[string]string
		contents string
	}{
		{"a read-only wall", &types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{read}}, map[string]string{"github.com": "GitHub"}, "read"},
		{"a wall with code_write", &types.PersonalWallsSpec{GitHubCapabilities: &[]ghscope.Capability{read, write}}, nil, "write"},
		{"no walls", nil, nil, "write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withheld, contents := launchGitLaneUnderWalls(t, tc.walls)
			if !maps.Equal(withheld, tc.withheld) || contents != tc.contents {
				t.Errorf("GitPushWithheld %v, git grant contents %q; want %v and %q", withheld, contents, tc.withheld, tc.contents)
			}
		})
	}
}

// A push needs code_write in the policy's list where it names one and in the walls' list where they name one.
func TestPushWithheldBy(t *testing.T) {
	read, write := adoscope.CapCodeRead, adoscope.CapCodeWrite
	for _, tc := range []struct {
		name   string
		policy []adoscope.Capability
		wall   *[]adoscope.Capability
		want   bool
	}{
		{"neither names a list", nil, nil, false},
		{"the policy leaves out code_write", []adoscope.Capability{read}, nil, true},
		{"the policy keeps code_write but the walls leave it out", []adoscope.Capability{read, write}, &[]adoscope.Capability{read}, true},
		{"the policy and the walls both keep code_write", []adoscope.Capability{read, write}, &[]adoscope.Capability{read, write}, false},
		{"the queue's start finds walls saved since create without code_write", []adoscope.Capability{write}, &[]adoscope.Capability{}, true},
		{"the policy keeps code_write and no walls name a list", []adoscope.Capability{read, write}, nil, false},
		{"the walls leave it out where the policy names none", nil, &[]adoscope.Capability{read}, true},
		{"the walls keep it", nil, &[]adoscope.Capability{write}, false},
	} {
		if got := pushWithheldBy(tc.policy, tc.wall, write); got != tc.want {
			t.Errorf("%s: pushWithheldBy = %v, want %v", tc.name, got, tc.want)
		}
	}
	got := gitPushWithheld{ado: true}.hosts([]string{"dev.azure.com"})
	if !maps.Equal(got, map[string]string{"dev.azure.com": "Azure DevOps"}) {
		t.Errorf("hosts = %v, want the Azure DevOps git host alone", got)
	}
	if got := (gitPushWithheld{}).hosts([]string{"dev.azure.com"}); got != nil {
		t.Errorf("hosts = %v, want nil when nothing is withheld", got)
	}
}

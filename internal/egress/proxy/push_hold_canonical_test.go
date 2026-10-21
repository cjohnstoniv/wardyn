// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPATPushTargetCanonicalRepos pins the git_pat lane's held-push repository and key (#2749). Off,
// they are the bytes git named, as before. On, a github.com path drops ".git" and is lowercased, the
// App lane's form; every other forge's path, Azure DevOps' included, is untouched.
func TestPATPushTargetCanonicalRepos(t *testing.T) {
	g := PATGrant{GrantID: uuid.MustParse("00000000-0000-4000-8000-000000002749")}
	cmds := []gitpack.Command{{Ref: "refs/heads/work", New: strings.Repeat("a", 40)}}
	paths := []string{".github/workflows/ci.yml"}
	for _, c := range []struct {
		host, rest string
		canonical  bool
		want       string
	}{
		{"github.com", "/Owner/Repo.git/git-receive-pack", false, "github.com/Owner/Repo.git"},
		{"github.com", "/owner/repo/git-receive-pack", false, "github.com/owner/repo"},
		{"github.com", "/Owner/Repo.git/git-receive-pack", true, "github.com/owner/repo"},
		{"github.com", "/OWNER/repo/git-receive-pack", true, "github.com/owner/repo"},
		{"gitlab.com", "/Org/Repo.git/git-receive-pack", true, "gitlab.com/Org/Repo.git"},
		{"dev.azure.com", "/Org/Project/_git/Repo/git-receive-pack", true, "dev.azure.com/Org/Project/_git/Repo"},
		{"org.visualstudio.com", "/Project/_git/Repo.git/git-receive-pack", true, "org.visualstudio.com/Project/_git/Repo.git"},
	} {
		scope, key := pushScope(paths, cmds, patPushTarget(c.host, c.rest, g, c.canonical))
		wantKey := c.want + "\x00" + types.PushPathsDigest(paths) + "\x00refs/heads/work " + strings.Repeat("a", 40)
		if scope.Repo != c.want || key != wantKey || scope.ActsAs != "git_pat:"+g.GrantID.String() {
			t.Errorf("%s%s canonical=%v: repo %q key %q acts_as %q, want %q / %q", c.host, c.rest, c.canonical, scope.Repo, key, scope.ActsAs, c.want, wantKey)
		}
	}
}

// TestCanonicalPushReposOffAddsNoConfigByte: off, the proxy config a run is dispatched with carries no
// new key, so an older proxy reads it as it did.
func TestCanonicalPushReposOffAddsNoConfigByte(t *testing.T) {
	off, err := json.Marshal(Config{RunID: uuid.Nil})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(off, []byte("canonical_push_repos")) {
		t.Errorf("off config = %s, want no canonical_push_repos key", off)
	}
	on, _ := json.Marshal(Config{CanonicalPushRepos: true})
	var back Config
	if err := json.Unmarshal(on, &back); err != nil || !back.CanonicalPushRepos {
		t.Errorf("on config %s did not round-trip: %+v, %v", on, back.CanonicalPushRepos, err)
	}
}

// TestPushHoldCanonicalReposKeepsADenyAcrossSpellings: with the switch on, a push an admin denied for
// github.com/Owner/Repo.git stays denied when the same commits go to /owner/repo, and nothing is asked
// again. Off, the second spelling raises its own approval, as before.
func TestPushHoldCanonicalReposKeepsADenyAcrossSpellings(t *testing.T) {
	setPATBranchNS(t, "true")
	for _, c := range []struct {
		canonical  bool
		wantRaises int
	}{{false, 2}, {true, 1}} {
		up := newPATBrokerUpstream(t, "pat-token", "oauth2")
		p, _ := newPATBrokerProxySpec(t, reviewSpec(1, []string{".github/workflows/**"}),
			map[string]PATGrant{"github.com": {GrantID: uuid.New(), Username: "oauth2"}}, upstreamAddr(up.srv))
		p.canonicalPushRepos = c.canonical
		cp := newPushCP(t, types.ApprovalDenied)
		cp.wire(t, p)
		body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
		for _, path := range []string{"Owner/Repo.git", "owner/repo"} {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, "/wardyn/git/github.com/"+path+"/git-receive-pack", bytes.NewReader(body)))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("canonical=%v %s: status = %d body %q, want 403", c.canonical, path, rec.Code, rec.Body.String())
			}
		}
		if raises, _ := cp.counts(); raises != c.wantRaises || up.gitHits != 0 {
			t.Errorf("canonical=%v: raises = %d, forge hits %d; want %d raises and no forward", c.canonical, raises, up.gitHits, c.wantRaises)
		}
	}
}

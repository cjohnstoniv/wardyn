// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const prSetsServerHost = "tfs.corp.example"

// adoServerLane is the token lane on an Azure DevOps Server host with the push
// rules carried per SCM entry (no run-wide block): collection "coll" denies a
// workflow file, under the base path the provider row recorded, if any. push
// sends a workflow-editing push to the repository at path.
func adoServerLane(t *testing.T, basePath string) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, func(path string) *httptest.ResponseRecorder) {
	t.Helper()
	up := newPATBrokerUpstream(t, "pat-token", "oauth2")
	p, sink := newPATBrokerProxySpec(t, types.RunPolicySpec{},
		map[string]PATGrant{prSetsServerHost: {GrantID: uuid.New(), Username: "oauth2"}}, upstreamAddr(up.srv))
	p.pushSets = []types.PushRuleSet{{
		Provider: string(types.GitProviderAzureDevOps), Host: prSetsServerHost, Org: "coll", BasePath: basePath,
		Rules: types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
	}}
	body := string(recordedPush(t, "refs/heads/main", map[string]string{".github/workflows/ci.yml": "on: push\n"}))
	push := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
			"/wardyn/git/"+prSetsServerHost+"/"+path+"/git-receive-pack", strings.NewReader(body)))
		return rec
	}
	return p, sink, up, push
}

// The CARRY's first test: a base path recorded on the row keys the remote to the
// right collection, so its rules apply and another collection's do not.
func TestGitRoute_ADOServerUsesTheRowBasePath(t *testing.T) {
	_, sink, up, push := adoServerLane(t, "/tfs")
	if rec := push("tfs/coll/proj/_git/app"); rec.Code != http.StatusForbidden || !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Fatalf("collection with rules: %d %q log %q, want a content-rules refusal", rec.Code, rec.Body.String(), sink.String())
	}
	if up.gitHits != 0 {
		t.Errorf("a refused push reached the forge %d times", up.gitHits)
	}
	// Another collection under the same prefix has no set: no content rules, the push goes on.
	if rec := push("tfs/other/proj/_git/app"); rec.Code != http.StatusOK || up.gitHits != 1 {
		t.Errorf("collection without rules: %d %q, forge hits %d, want 200 and one hit", rec.Code, rec.Body.String(), up.gitHits)
	}
}

// The CARRY's second test: a path prefix with no recorded base path is ambiguous
// ("tfs/coll/proj/_git/app" reads as no collection at all), so the push is
// refused with its own rule source and never treated as "no rules".
func TestGitRoute_ADOServerPathPrefixWithoutBasePathRefused(t *testing.T) {
	_, sink, up, push := adoServerLane(t, "")
	rec := push("tfs/coll/proj/_git/app")
	if rec.Code != http.StatusForbidden || !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushEntry+`"`) {
		t.Fatalf("%d %q log %q, want a %s refusal", rec.Code, rec.Body.String(), sink.String(), ruleSourceGitPushEntry)
	}
	if up.gitHits != 0 || up.mintCalls != 0 {
		t.Errorf("an underivable entry minted %d and reached the forge %d times", up.mintCalls, up.gitHits)
	}
}

// An error from the lookup refuses the push on every route that applies rules; it
// is never read as "this entry has no rules".
func TestGitRoute_PushRulesForRemoteErrorRefuses(t *testing.T) {
	p := &Proxy{pushSets: []types.PushRuleSet{{Provider: string(types.GitProviderGitHub), Host: "ghes.corp.example", Org: "acme",
		Rules: types.PushRulesSpec{DenyPaths: []string{"infra/**"}}}}}
	for _, path := range []string{"acme/../other/repo.git", "acme//repo.git", "acme/%2e%2e/repo.git", "acme/%zz/repo.git"} {
		if rules, msg := p.rulesForRemote("ghes.corp.example", path); rules != nil || msg == "" {
			t.Errorf("%q: rules %v msg %q, want a refusal", path, rules, msg)
		}
	}
	if rules, msg := p.rulesForRemote("ghes.corp.example", "acme/repo.git/git-receive-pack"); rules == nil || msg != "" {
		t.Errorf("a derivable remote: rules %v msg %q, want the entry's rules", rules, msg)
	}
	if rules, msg := p.rulesForRemote("ghes.corp.example", "other/repo.git/git-receive-pack"); rules != nil || msg != "" {
		t.Errorf("an entry without a set: rules %v msg %q, want none", rules, msg)
	}
	if rules, msg := p.rulesForRemote("github.com", "acme/repo"); rules != nil || msg != "" {
		t.Errorf("a host with no set: rules %v msg %q, want none", rules, msg)
	}
}

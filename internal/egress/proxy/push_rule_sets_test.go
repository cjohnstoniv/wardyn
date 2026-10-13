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
// workflow file and secrets/, under the base path the provider row recorded, if
// any; the run's base rules, which an entry with no set is held to, deny secrets/
// only. push sends a workflow-editing push to the repository at path, pushSecrets
// one that edits secrets/.
func adoServerLane(t *testing.T, basePath string) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, func(path string) *httptest.ResponseRecorder) {
	p, sink, up, push, _ := adoServerLaneBoth(t, basePath)
	return p, sink, up, push
}

func adoServerLaneBoth(t *testing.T, basePath string) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, func(path string) *httptest.ResponseRecorder, func(path string) *httptest.ResponseRecorder) {
	t.Helper()
	up := newPATBrokerUpstream(t, "pat-token", "oauth2")
	p, sink := newPATBrokerProxySpec(t, types.RunPolicySpec{},
		map[string]PATGrant{prSetsServerHost: {GrantID: uuid.New(), Username: "oauth2"}}, upstreamAddr(up.srv))
	p.pushSets = []types.PushRuleSet{{
		Provider: string(types.GitProviderAzureDevOps), Host: prSetsServerHost, Org: "coll", BasePath: basePath,
		Rules: types.PushRulesSpec{DenyPaths: []string{".github/workflows/**", "secrets/**"}},
	}}
	// The run's base rules, as the ceiling left them: what an entry with no set is held to.
	p.pushBase = compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"secrets/**"}})
	sender := func(files map[string]string) func(path string) *httptest.ResponseRecorder {
		body := string(recordedPush(t, "refs/heads/main", files))
		return func(path string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
				"/wardyn/git/"+prSetsServerHost+"/"+path+"/git-receive-pack", strings.NewReader(body)))
			return rec
		}
	}
	return p, sink, up, sender(map[string]string{".github/workflows/ci.yml": "on: push\n"}), sender(map[string]string{"secrets/key": "k\n"})
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
}

// An entry the run reaches but did not bring (no set of its own) is held to the
// run's base rules, never to "no rules": the credential reaches every collection on
// the host, and the policy's and the ceiling's deny still stand there. A set only adds.
func TestGitRoute_PushToUnbroughtEntryKeepsTheBaseRules(t *testing.T) {
	_, sink, up, push, pushSecrets := adoServerLaneBoth(t, "/tfs")
	rec := pushSecrets("tfs/other/proj/_git/app")
	if rec.Code != http.StatusForbidden || !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Fatalf("an unbrought collection: %d %q log %q, want a content-rules refusal", rec.Code, rec.Body.String(), sink.String())
	}
	if up.gitHits != 0 || up.mintCalls != 0 {
		t.Errorf("the refused push minted %d and reached the forge %d times", up.mintCalls, up.gitHits)
	}
	// The set only adds: the workflow deny is the brought collection's, and the base does not carry it.
	if rec := push("tfs/other/proj/_git/app"); rec.Code != http.StatusOK || up.gitHits != 1 {
		t.Errorf("a workflow edit in an unbrought collection: %d %q hits %d, want 200 (the base denies secrets/ only)", rec.Code, rec.Body.String(), up.gitHits)
	}
}

// With no base rules (the policy had none) an entry with no set has none: the
// push goes on, so the base is the only thing that decides the case above.
func TestGitRoute_NoBaseRulesMeansNoneForAnUnbroughtEntry(t *testing.T) {
	p, _, up, _, pushSecrets := adoServerLaneBoth(t, "/tfs")
	p.pushBase = nil
	if rec := pushSecrets("tfs/other/proj/_git/app"); rec.Code != http.StatusOK || up.gitHits != 1 {
		t.Errorf("%d %q hits %d, want 200 and one hit", rec.Code, rec.Body.String(), up.gitHits)
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
	p.pushBase = compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"base/**"}})
	if rules, msg := p.rulesForRemote("ghes.corp.example", "other/repo.git/git-receive-pack"); rules != p.pushBase || msg != "" {
		t.Errorf("an entry without a set: rules %v msg %q, want the base", rules, msg)
	}
	if rules, msg := p.rulesForRemote("github.com", "acme/repo"); rules != p.pushBase || msg != "" {
		t.Errorf("a host with no set: rules %v msg %q, want the base", rules, msg)
	}
	if rules, msg := p.rulesForADOGrant("dev.azure.com", "someorg"); rules != p.pushBase || msg != "" {
		t.Errorf("an Azure DevOps grant with no set: rules %v msg %q, want the base", rules, msg)
	}
}

// The row's base path decides how the remote is read: a host-only row reads the
// collection from the remote, a collection-scoped row has it in the base path and
// takes no further collection segment, and any other shape is refused.
func TestGitRoute_ADOServerBasePathShapes(t *testing.T) {
	for _, tc := range []struct {
		name, base, path string
		want             int // 403 either way; the rule source tells which refusal
		source           string
	}{
		{"host-only row", "", "coll/proj/_git/app", http.StatusForbidden, ruleSourceGitRules},
		{"collection-scoped row", "/tfs/coll", "tfs/coll/proj/_git/app", http.StatusForbidden, ruleSourceGitRules},
		{"collection-scoped row, a segment too many", "/tfs/coll", "tfs/coll/extra/proj/_git/app", http.StatusForbidden, ruleSourceGitPushEntry},
		{"host-only row, a prefix too many", "", "tfs/coll/proj/_git/app", http.StatusForbidden, ruleSourceGitPushEntry},
		{"no project before _git", "", "coll/_git/app", http.StatusForbidden, ruleSourceGitPushEntry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sink, up, push := adoServerLane(t, tc.base)
			rec := push(tc.path)
			if rec.Code != tc.want || !strings.Contains(sink.String(), `"rule_source":"`+tc.source+`"`) {
				t.Fatalf("%d %q log %q, want a %s refusal", rec.Code, rec.Body.String(), sink.String(), tc.source)
			}
			if up.gitHits != 0 || up.mintCalls != 0 {
				t.Errorf("a refused push minted %d and reached the forge %d times", up.mintCalls, up.gitHits)
			}
		})
	}
}

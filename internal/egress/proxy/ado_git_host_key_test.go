// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The git broker finds the Azure DevOps grant under every spelling of its host (hostKey), and holds a push
// that names the host in another spelling to the rules of the host's own entry: the lane is handed the
// grant's key, so the entry's set is found and the push is not held to the run's base rules alone.
func TestGitBrokerHoldsAnAzureDevOpsHostInAnotherSpellingToItsEntrysRules(t *testing.T) {
	for _, tc := range []struct {
		grant    string
		spelling []string
	}{
		{adoServerHost, []string{adoServerHost, adoServerHost + ".", strings.ToUpper(adoServerHost)}},
		{"1.2.3.11", []string{"1.2.3.11", "::ffff:1.2.3.11", "0:0:0:0:0:ffff:102:30b", "1.2.3.11."}},
	} {
		h := newADOServerHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
		if tc.grant != adoServerHost {
			h.p.adoGrants = adoGrantsByHost{tc.grant: h.p.adoGrants[adoServerHost]}
			h.p.inject.byHost[tc.grant] = h.p.inject.byHost[adoServerHost]
		}
		// The entry's set denies workflow files; the run's base rules do not.
		h.p.pushSets = []types.PushRuleSet{{
			Provider: string(types.GitProviderAzureDevOps), Host: tc.grant, Org: "DefaultCollection", BasePath: "/tfs",
			Rules: types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
		}}
		h.p.pushBase = compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"secrets/**"}})
		branch := "refs/heads/wardyn/" + h.runID.String() + "/x"
		push := func(host string, files map[string]string) (*httptest.ResponseRecorder, int) {
			h.front.mu.Lock()
			before := len(h.front.seen)
			h.front.mu.Unlock()
			h.sink.Reset()
			rec := httptest.NewRecorder()
			h.p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
				"/wardyn/git/"+host+"/tfs/DefaultCollection/proj/_git/app/git-receive-pack", bytes.NewReader(recordedPush(t, branch, files))))
			h.front.mu.Lock()
			defer h.front.mu.Unlock()
			return rec, len(h.front.seen) - before
		}
		for _, host := range tc.spelling {
			rec, forwarded := push(host, map[string]string{".github/workflows/ci.yml": "on: push\n"})
			if rec.Code != http.StatusForbidden || forwarded != 0 || !strings.Contains(h.sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
				t.Errorf("grant %q, a workflow push naming %q: %d, forwarded %d times, log %q; want the entry's content-rules refusal",
					tc.grant, host, rec.Code, forwarded, h.sink.String())
			}
			if !strings.Contains(h.sink.String(), `"Host":"`+tc.grant+`"`) {
				t.Errorf("grant %q, a push naming %q: the decision row names another host: %q", tc.grant, host, h.sink.String())
			}
			// The control: a push the entry's set allows reaches the forge under every spelling.
			if _, forwarded := push(host, map[string]string{"README.md": "hi\n"}); forwarded != 1 {
				t.Errorf("grant %q, an allowed push naming %q: forwarded %d times, want 1", tc.grant, host, forwarded)
			}
		}
	}
}

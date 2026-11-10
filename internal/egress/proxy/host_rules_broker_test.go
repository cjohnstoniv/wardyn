// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every brokered route re-originates outside the CONNECT and plain lanes, so the forward itself is judged
// (outboundHostRuleRefused, from roundTripUpstream and the egress transport's Proxy hook). Each test sends
// one forward under four rule sets on the route's upstream host, set on the compiled policy so nothing else
// about the fixture changes: an allow rule matching the method and path the host receives lets it through;
// a deny rule matching it, an allow rule for another method, and one for another path each refuse it with a
// policy:host-rule row before it reaches the host.

// brokerSend sends the route's one forward and reports the status and whether the upstream saw it.
type brokerSend func(t *testing.T) (status int, reached bool)

func checkBrokerHostRules(t *testing.T, p *Proxy, log func() string, host, method, path string, send brokerSend) {
	t.Helper()
	other := http.MethodPut
	if method == other {
		other = http.MethodPatch
	}
	rule := func(e types.HostRuleEffect, m, path string) types.HostRule {
		return types.HostRule{Host: host, Methods: []string{m}, Paths: []string{path}, Effect: e}
	}
	for _, tc := range []struct {
		name  string
		rules []types.HostRule
		pass  bool
	}{
		{"allow rule matching", []types.HostRule{rule(types.HostRuleAllow, method, path)}, true},
		{"deny rule matching", []types.HostRule{rule(types.HostRuleDeny, method, path), {Host: host, Effect: types.HostRuleAllow}}, false},
		{"allow rule for another method", []types.HostRule{rule(types.HostRuleAllow, other, path)}, false},
		{"allow rule for another path", []types.HostRule{rule(types.HostRuleAllow, method, "/elsewhere")}, false},
	} {
		p.policy.hostRules = []types.HostRuleSet{{Rules: tc.rules}}
		rows := strings.Count(log(), `"rule_source":"`+ruleSourceHostRule+`"`)
		status, reached := send(t)
		refused := strings.Count(log(), `"rule_source":"`+ruleSourceHostRule+`"`) > rows
		ok := status >= 200 && status < 300
		if tc.pass && (!ok || !reached || refused) {
			t.Errorf("%s: status %d reached=%v host-rule row=%v, want it forwarded", tc.name, status, reached, refused)
		}
		if !tc.pass && (status != http.StatusForbidden || reached || !refused || strings.Contains(log(), "builtin:dial-failed")) {
			t.Errorf("%s: status %d reached=%v host-rule row=%v, want 403 with a policy:host-rule row (and no dial-failed row) before the host", tc.name, status, reached, refused)
		}
	}
}

func gitHitsOf(u *gitBrokerUpstream) int { u.mu.Lock(); defer u.mu.Unlock(); return u.gitHits }

func TestHostRulesJudgeTheModelRoute(t *testing.T) {
	up := newRawUpstream(t, `{}`)
	p, buf := llmChecksProxy(t, types.RunPolicySpec{}, "", up)
	checkBrokerHostRules(t, p, buf.String, openaiHost, http.MethodGet, "/v1/models", func(t *testing.T) (int, bool) {
		up.reached = false
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+"v1/models", nil))
		return rec.Code, up.reached
	})
}

func TestHostRulesJudgeTheGitBroker(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, buf := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	checkBrokerHostRules(t, p, buf.String, "github.com", http.MethodGet, "/octocat/hello-world.git", func(t *testing.T) (int, bool) {
		hits := gitHitsOf(up)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
		return rec.Code, gitHitsOf(up) > hits
	})
}

func TestHostRulesJudgeTheGitCredentialBroker(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, buf := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
	checkBrokerHostRules(t, p, buf.String, "gitlab.com", http.MethodGet, "/org/repo.git", func(t *testing.T) (int, bool) {
		hits := gitHitsOf(up)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil))
		return rec.Code, gitHitsOf(up) > hits
	})
}

func TestHostRulesJudgeTheMCPBroker(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	checkBrokerHostRules(t, f.p, f.buf.String, "mcp.linear.test", http.MethodPost, "/mcp", func(t *testing.T) (int, bool) {
		hits := f.up.hits
		rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
		return rec.Code, f.up.hits > hits
	})
}

// The kube and git-forge API doors are reached inside a terminated tunnel, where the request is judged
// before the door (serveAdmittedMITMRequest). These call the door itself, so the forward's own check is
// what decides: the kube door sends on roundTripUpstream or, with the row's CA, on its own clone of the
// egress transport (bindTransport); the API door on roundTripUpstream.
func TestHostRulesJudgeTheKubeDoor(t *testing.T) {
	for name, o := range map[string]kubeOpts{"egress transport": {trustCA: true}, "row CA transport": {caPEM: true}} {
		t.Run(name, func(t *testing.T) {
			h := newKubeHarness(t, o)
			checkBrokerHostRules(t, h.p, h.log, kubeHost, http.MethodGet, "/api/v1/namespaces/team-a", func(t *testing.T) (int, bool) {
				seen := len(h.upstreamSaw())
				raw := "GET /api/v1/namespaces/team-a/pods HTTP/1.1\r\nHost: " + kubeHost + ":6443\r\nAuthorization: " + kubeSentinel + "\r\n\r\n"
				req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				h.p.serveAPIDoor(rec, req, kubeHost, kubePort)
				return rec.Code, len(h.upstreamSaw()) > seen
			})
		})
	}
}

func TestHostRulesJudgeThePATAPIDoor(t *testing.T) {
	h := newAPIHarness(t, "gitea", PATGrant{})
	checkBrokerHostRules(t, h.p, h.log.String, apiHost, http.MethodGet, gtBase, func(t *testing.T) (int, bool) {
		hits := h.forgeHits()
		rec := httptest.NewRecorder()
		h.p.serveAPIDoor(rec, rawAPIRequest(t, http.MethodGet, gtBase+"/issues", "", "", nil), apiHost, 443)
		return rec.Code, h.forgeHits() > hits
	})
}

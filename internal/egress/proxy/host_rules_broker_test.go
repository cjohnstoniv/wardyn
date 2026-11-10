// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every brokered route re-originates outside the CONNECT and plain lanes, so the forward itself is judged
// (outboundHostRuleRefused, from roundTripUpstream and the egress transport's Proxy hook). Each test puts a
// deny-all rule on the route's upstream host, set on the compiled policy so nothing else about the fixture
// changes, and proves the forward is refused with a policy:host-rule row and never reaches the host.

func denyHostRules(p *Proxy, host string) {
	p.policy.hostRules = []types.HostRuleSet{{Rules: []types.HostRule{{Host: host, Effect: types.HostRuleDeny}}}}
}

func wantHostRuleRefusal(t *testing.T, route string, status int, reached bool, buf *bytes.Buffer) {
	t.Helper()
	if reached {
		t.Errorf("%s: the forward reached a host whose rules deny it", route)
	}
	if status >= 200 && status < 300 {
		t.Errorf("%s: status %d, want a refusal", route, status)
	}
	if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceHostRule+`"`) {
		t.Errorf("%s: no %s row:\n%s", route, ruleSourceHostRule, buf.String())
	}
}

func TestHostRulesJudgeTheModelRoute(t *testing.T) {
	up := newRawUpstream(t, `{}`)
	p, buf := llmChecksProxy(t, types.RunPolicySpec{}, "", up)
	denyHostRules(p, openaiHost)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, llmOpenAIPrefix+"v1/models", nil))
	wantHostRuleRefusal(t, "model route", rec.Code, up.reached, buf)
}

func TestHostRulesJudgeTheGitBroker(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, buf := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
	denyHostRules(p, "github.com")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
	wantHostRuleRefusal(t, "git broker", rec.Code, up.gitPath != "", buf)
}

func TestHostRulesJudgeTheGitCredentialBroker(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, buf := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
	denyHostRules(p, "gitlab.com")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil))
	wantHostRuleRefusal(t, "git credential broker", rec.Code, up.gitPath != "", buf)
}

func TestHostRulesJudgeTheMCPBroker(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	denyHostRules(f.p, "mcp.linear.test")
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
	wantHostRuleRefusal(t, "MCP broker", rec.Code, f.up.hits > 0, f.buf)
}

// The kube and git-forge API doors are reached inside a terminated tunnel, where the request is judged
// before the door (serveAdmittedMITMRequest). These call the door itself, so the forward's own check is
// what refuses: the kube door sends on its clone of the egress transport, the API door on roundTripUpstream.
func TestHostRulesJudgeTheKubeDoor(t *testing.T) {
	// trustCA sends on roundTripUpstream; caPEM on the door's own clone of the egress transport.
	for name, o := range map[string]kubeOpts{"egress transport": {trustCA: true}, "row CA transport": {caPEM: true}} {
		h := newKubeHarness(t, o)
		denyHostRules(h.p, kubeHost)
		rec := httptest.NewRecorder()
		raw := "GET /api/v1/namespaces/team-a/pods HTTP/1.1\r\nHost: " + kubeHost + ":6443\r\nAuthorization: " + kubeSentinel + "\r\n\r\n"
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		h.p.serveAPIDoor(rec, req, kubeHost, kubePort)
		var buf bytes.Buffer
		buf.WriteString(h.log())
		wantHostRuleRefusal(t, "kube door, "+name, rec.Code, len(h.upstreamSaw()) > 0, &buf)
	}
}

func TestHostRulesJudgeThePATAPIDoor(t *testing.T) {
	h := newAPIHarness(t, "gitlab", PATGrant{})
	denyHostRules(h.p, apiHost)
	rec := httptest.NewRecorder()
	h.p.serveAPIDoor(rec, rawAPIRequest(t, http.MethodGet, glBase+"/issues", "", "", nil), apiHost, 443)
	wantHostRuleRefusal(t, "git forge API door", rec.Code, h.forgeHits() > 0, h.log)
}

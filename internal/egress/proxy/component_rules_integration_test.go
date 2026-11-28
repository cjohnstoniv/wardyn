// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A custom component's request rules reach the proxy as one host rule set of their own, appended beside the
// policy's sets when a run attaches the component, with the component's egress hosts among the run's allowed
// hosts.
// Nothing marks the set as a component's, so the proxy judges it as it judges any other. These tests build
// that run policy from a definition a save accepts, then check the outcomes on the sandbox's HTTPS calls and
// on the MCP broker's forward.

// componentRunSpec is the policy of a run that attaches the custom component def beside the policy's own
// sets: def is checked as a person's save checks it (ComponentDefinition.Validate), its hosts are allowed as
// run create allows them (a remote MCP server's own host left out, since the broker reaches it), and its
// request rules are one more set (types.AppendHostRuleSets).
func componentRunSpec(t *testing.T, def types.ComponentDefinition, policy ...types.HostRuleSet) types.RunPolicySpec {
	t.Helper()
	if err := def.Validate(ValidDomainEntry, true); err != nil {
		t.Fatalf("the component's definition is refused at save: %v", err)
	}
	var allowed []string
	for _, h := range def.Hosts {
		if def.RemoteMCP() {
			server, err := types.MCPURLDestination(def.MCP.URL)
			d, perr := types.ParseDestination(h)
			if err != nil || perr != nil || d.Host == server.Host {
				continue
			}
		}
		allowed = append(allowed, h)
	}
	return types.RunPolicySpec{
		AllowedDomains: allowed,
		HostRules:      types.AppendHostRuleSets(policy, []types.HostRuleSet{{Rules: def.RequestRules}}),
	}
}

// crSend sends one HTTPS request to hrHost through the proxy at proxyURL, in a tunnel of its own on port
// 443, trusting caPEM, and returns the response with its body read.
func crSend(t *testing.T, proxyURL string, caPEM []byte, method, path string) *http.Response {
	t.Helper()
	status, conn, _ := hrConnect(t, proxyURL, "443")
	if status != http.StatusOK {
		t.Fatalf("CONNECT to a rule host = %d, want 200", status)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tc := tls.Client(conn, &tls.Config{ServerName: hrHost, RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("the proxy did not open the rule host's tunnel: %v", err)
	}
	if _, err := io.WriteString(tc, method+" "+path+" HTTP/1.1\r\nHost: "+hrHost+"\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatalf("write %s %s: %v", method, path, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// crRows is the decision rows for method and path with the given source and decision.
func crRows(t *testing.T, buf *bytes.Buffer, source string, d egress.Decision, method, path string) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	for _, row := range hrRows(t, buf) {
		if row.RuleSource == source && row.Decision == d && row.Request.Method == method && row.Request.Path == path {
			out = append(out, row)
		}
	}
	return out
}

// TestComponentRuleHold: a custom component's one set (allow GET /a, hold POST /a, deny /b) decides each of
// the sandbox's HTTPS calls to its host. GET /a reaches the host. A call to /b, and a call no rule names, is
// refused before the host with a host-rule row and asks nobody. POST /a asks a person through the host call
// hold: while nobody answers, it waits out the window and is refused as pending, never sent (it fails
// closed); once a person approves, it reaches the host and its allow row names the approval.
func TestComponentRuleHold(t *testing.T) {
	def := types.ComponentDefinition{Hosts: []string{hrHost}, RequestRules: []types.HostRule{
		{Host: hrHost, Methods: []string{http.MethodGet}, Paths: []string{"/a"}, Effect: types.HostRuleAllow},
		{Host: hrHost, Methods: []string{http.MethodPost}, Paths: []string{"/a"}, Effect: types.HostRuleHold},
		{Host: hrHost, Paths: []string{"/b"}, Effect: types.HostRuleDeny},
	}}
	cu := captureUpstream(t, true, "ok")
	p, buf, caPEM := hrProxy(t, hrOpts{}, cu.srv)
	p.policy = CompilePolicy(componentRunSpec(t, def))
	cp := newHCCP(t, p)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	send := func(method, path string) (*http.Response, bool) {
		cu.reached = false
		resp := crSend(t, srv.URL, caPEM, method, path)
		return resp, cu.reached
	}

	if resp, reached := send(http.MethodGet, "/a"); resp.StatusCode != http.StatusOK || !reached {
		t.Fatalf("allowed GET /a = %d reached=%v, want 200 at the host", resp.StatusCode, reached)
	}
	for _, c := range []struct{ method, path string }{{http.MethodGet, "/b"}, {http.MethodPost, "/b"}, {http.MethodGet, "/c"}} {
		resp, reached := send(c.method, c.path)
		if resp.StatusCode != http.StatusForbidden || reached || resp.Header.Get(egressHeaderStatus) == egressRefusalPending {
			t.Errorf("%s %s = %d reached=%v (%q), want a 403 refusal before the host", c.method, c.path,
				resp.StatusCode, reached, resp.Header.Get(egressHeaderStatus))
		}
		if n := len(crRows(t, buf, ruleSourceHostRule, egress.Deny, c.method, c.path)); n != 1 {
			t.Errorf("%s %s: %d host-rule deny rows, want 1:\n%s", c.method, c.path, n, buf)
		}
	}
	if _, raises, _ := cp.counts(); raises != 0 {
		t.Fatalf("an allowed or refused call asked a person: raises=%d", raises)
	}

	start := time.Now()
	resp, reached := send(http.MethodPost, "/a")
	if resp.StatusCode != http.StatusForbidden || reached || resp.Header.Get(egressHeaderStatus) != egressRefusalPending {
		t.Fatalf("undecided POST /a = %d reached=%v (%q), want a pending 403 before the host", resp.StatusCode, reached,
			resp.Header.Get(egressHeaderStatus))
	}
	if took, window := time.Since(start), hostCallWindow*hostCallSecond; took < window {
		t.Errorf("the undecided hold answered after %v, before its %v window ended", took, window)
	}
	rows, raises, _ := cp.counts()
	if rows != 1 || raises != 1 || cp.badScopes != 0 {
		t.Fatalf("rows=%d raises=%d bad=%d, want one host call asked", rows, raises, cp.badScopes)
	}
	cp.mu.Lock()
	asked := *cp.rows[0]
	cp.mu.Unlock()
	want := types.HostCallScope{Host: hrHost, Method: http.MethodPost, Path: "/a", HoldSeconds: hostCallWindow}
	want.CallSHA256 = want.ComputeKey()
	if asked.scope != want {
		t.Fatalf("raised scope = %+v, want %+v", asked.scope, want)
	}
	waits := crRows(t, buf, ruleSourceHostRuleApprovalWait, egress.Pending, http.MethodPost, "/a")
	if len(waits) != 1 || waits[0].ApprovalID == nil || *waits[0].ApprovalID != asked.id {
		t.Fatalf("wait rows naming %s: %+v, want one:\n%s", asked.id, waits, buf)
	}

	hostCallSecond = time.Second // the approved call need not rejoin within a 90 ms window; newHCCP restores it
	cp.decideAll(types.ApprovalApproved)
	if resp, reached := send(http.MethodPost, "/a"); resp.StatusCode != http.StatusOK || !reached {
		t.Fatalf("approved POST /a = %d reached=%v, want 200 at the host", resp.StatusCode, reached)
	}
	released := crRows(t, buf, ruleSourceHostRuleApprovalApproved, egress.Allow, http.MethodPost, "/a")
	if len(released) != 1 || released[0].ApprovalID == nil || *released[0].ApprovalID != asked.id {
		t.Fatalf("release rows naming %s: %+v, want one:\n%s", asked.id, released, buf)
	}
	if rows, _, _ := cp.counts(); rows != 1 {
		t.Errorf("the approved call opened another approval: rows=%d", rows)
	}
}

// TestComponentRulesJudgeTheMCPBroker: a remote MCP server added as a custom component, with a rule that
// denies the broker's POST /mcp, is refused at the broker's forward with a host-rule row, before the server.
// A policy set that allows everything on the host does not lift that deny, since a component's set only
// narrows. The same component with a rule that allows the call reaches the server.
func TestComponentRulesJudgeTheMCPBroker(t *testing.T) {
	const host = "mcp.linear.test"
	component := func(rules ...types.HostRule) types.ComponentDefinition {
		return types.ComponentDefinition{Hosts: []string{host}, RequestRules: rules,
			MCP: &types.ComponentMCP{URL: "https://" + host + "/mcp", Transport: types.ComponentMCPTransportHTTP}}
	}
	call := func(e types.HostRuleEffect) types.HostRule {
		return types.HostRule{Host: host, Methods: []string{http.MethodPost}, Paths: []string{"/mcp"}, Effect: e}
	}
	allowHost := types.HostRule{Host: host, Effect: types.HostRuleAllow}
	for _, tc := range []struct {
		name string
		spec types.RunPolicySpec
		pass bool
	}{
		{"a rule allowing the call", componentRunSpec(t, component(call(types.HostRuleAllow))), true},
		{"a rule denying the call", componentRunSpec(t, component(call(types.HostRuleDeny), allowHost)), false},
		{"a rule denying the call beside a policy allowing the host",
			componentRunSpec(t, component(call(types.HostRuleDeny), allowHost), types.HostRuleSet{Rules: []types.HostRule{allowHost}}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPFixture(t, tc.spec, nil, nil)
			hits := f.up.hitCount()
			rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
			reached := f.up.hitCount() > hits
			refused := strings.Contains(f.buf.String(), `"rule_source":"`+ruleSourceHostRule+`"`)
			ok := rec.Code >= 200 && rec.Code < 300
			if tc.pass && (!ok || !reached || refused) {
				t.Errorf("status %d reached=%v host-rule row=%v, want it forwarded", rec.Code, reached, refused)
			}
			if !tc.pass && (rec.Code != http.StatusForbidden || reached || !refused) {
				t.Errorf("status %d reached=%v host-rule row=%v, want 403 with a policy:host-rule row before the server",
					rec.Code, reached, refused)
			}
		})
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// nonStandardSpellings are ASCII hosts a parser can read as an address that net.ParseIP does not: inet_aton
// forms of public addresses, forms inet_aton refuses and a decimal parser reads (leading zeros with an
// 8 or 9), forms glibc and the URL standard read as 0.0.0.0 (an empty 0x), numbers no parser reads as
// one address, and a percent or colon spelling net.ParseIP refuses.
var nonStandardSpellings = []string{
	"134744072", "0x8080808", "8.526344", "8.8.2056", "010.010.010.010", "0x8.0x8.0x8.0x8", "010.0.0.1",
	"008.008.008.008", "8.8.8.008", "09.1.1.1", "08.0.0.1",
	"0x", "0x.0x.0x.0x",
	"4294967296", "1.2.3.4.5",
	"8.8.8.8%eth0", "[2001:4860:4860::8888%25eth0]", "[::ffff:010.8.8.8]",
}

// nonASCIIHosts are hosts with a character outside ASCII, which programs map differently.
var nonASCIIHosts = []string{
	"８.８.８.８", "8。8。8。8", "１２７.０.０.１", "０ｘ８０８０８０８", // the URL standard maps these to 8.8.8.8, 127.0.0.1, 0x8080808
	"8.8.8.8。", "8.8.8.8．", "127.0.0.1。", // and these to 8.8.8.8. and 127.0.0.1., which it reads as the address
	// The URL standard maps none of these to a name; a looser mapper drops the joiner, maps the full-width
	// digit, colon or percent sign, and a dialer reads the zone.
	"8.8.8.8\u200d", "8.8.\u200c8.8", "[2001:4860:4860::8888%25é]", "[２００１:4860:4860::8888]", "8.8.8.8％eth0",
	"[::ffff:８.8.8.8]", "８.8.8.8%25eth0", "2001：4860：4860：：8888",
	// Names: the URL standard maps the first to the denied example.com; a client sends the others as xn-- names.
	"ｅｘａｍｐｌｅ.com", "bücher.de", "１２３.com", "_x.bücher.de",
	"b\xfccher.de", // bytes that are not UTF-8 (the name in Latin-1) are outside ASCII too
}

// spellingPolicies are the policies the spelling refusals hold under: each says nothing of the spelling.
var spellingPolicies = map[string]types.RunPolicySpec{
	"allow_all_egress":    {AllowAllEgress: true},
	"with a deny list":    {AllowAllEgress: true, DeniedDomains: []string{"8.8.8.8", "2001:4860:4860::8888", "example.com", "127.0.0.1"}},
	"refuse_ip_literals":  {AllowAllEgress: true, RefuseIPLiterals: true},
	"default-deny":        {AllowedDomains: []string{"8.8.8.8", "1.2.3.4"}},
	"wait_for_review ask": {AllowedDomains: []string{"8.8.8.8"}, FirstUseApproval: types.FirstUseWaitForReview},
}

// evalSpelling is the step-0-to-policy verdict and rule source for a CONNECT to authority:443 under spec.
func evalSpelling(t *testing.T, spec types.RunPolicySpec, authority string) (egress.Decision, string) {
	t.Helper()
	p, _ := newRuleTestProxy(t, spec, "127.0.0.1:1", false, "", nil)
	host, port := splitHostPort(authority+":443", 443)
	d, _, log := p.evaluate(context.Background(), host, port, http.MethodConnect, "")
	if log == nil {
		return d, ""
	}
	return d, log.RuleSource
}

// Every non-standard spelling of an address is refused at step 0 on every run, whatever the policy
// says, before an approval could be raised, and never handed to a corporate upstream: programs differ
// on which address such a spelling names, so no check of one decoded address binds what that hop reads.
// The blocked-range refusal still comes first for a spelling of a blocked address, and the standard
// spellings and host names keep their verdicts.
func TestNonStandardIPSpellingIsRefusedOnEveryRun(t *testing.T) {
	for name, spec := range spellingPolicies {
		for _, a := range nonStandardSpellings {
			if d, src := evalSpelling(t, spec, a); d != egress.Deny || src != ruleSourceIPSpelling {
				t.Errorf("%s: %s = %s %s, want deny %s", name, a, d, src, ruleSourceIPSpelling)
			}
		}
	}
	allowAll := types.RunPolicySpec{AllowAllEgress: true}
	for a, want := range map[string]string{
		"8.8.8.8": "policy:allowed", "[::ffff:8.8.8.8]": "policy:allowed", "[2001:4860:4860:0::8888]": "policy:allowed",
		"example.com": "policy:allowed", "0x8.example.com": "policy:allowed", "1..2": "policy:allowed",
		"0177.0.0.1": "builtin:private-ip", "1234": "builtin:private-ip", "[fe80::1%25eth0]": "builtin:private-ip",
	} {
		if _, src := evalSpelling(t, allowAll, a); src != want {
			t.Errorf("allow_all_egress: %s = %s, want %s", a, src, want)
		}
	}
	// net/http itself answers 400 to the two authorities it cannot read; every other one reaches the proxy.
	connectRefusals(t, slices.DeleteFunc(slices.Clone(nonStandardSpellings), func(a string) bool {
		return a == "8.8.8.8%eth0" || a == "[::ffff:010.8.8.8]"
	}), ipSpellingDetail)
}

// A host with a character outside ASCII is refused at step 0 on every run, whatever the policy says, and
// never handed to a corporate upstream: one program maps it to a name or an address the policy lists (a
// full-width example.com is the denied example.com, 8.8.8.8 with an ideographic full stop is 8.8.8.8) and
// the next maps it to another or to nothing, so no check of the host as written binds what the hop that
// resolves it reads. The client sends the name's xn-- form, which the lists match; a blocked-range address
// keeps the blocked-range refusal.
func TestNonASCIIHostIsRefusedOnEveryRun(t *testing.T) {
	for name, spec := range spellingPolicies {
		for _, a := range nonASCIIHosts {
			if d, src := evalSpelling(t, spec, a); d != egress.Deny || src != ruleSourceNonASCIIHost {
				t.Errorf("%s: %s = %s %s, want deny %s", name, a, d, src, ruleSourceNonASCIIHost)
			}
		}
	}
	for a, want := range map[string]struct {
		spec types.RunPolicySpec
		src  string
	}{
		"xn--bcher-kva.de":  {types.RunPolicySpec{AllowAllEgress: true}, "policy:allowed"},
		"xn--bcher-kva.de.": {types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"xn--bcher-kva.de"}}, "policy:denied"},
		"[fe80::1%25é]":     {types.RunPolicySpec{AllowAllEgress: true}, "builtin:private-ip"},
	} {
		if _, src := evalSpelling(t, want.spec, a); src != want.src {
			t.Errorf("%s = %s, want %s", a, src, want.src)
		}
	}
	// net/http itself answers 400 to the two bracketed authorities it cannot read; every other one reaches
	// the proxy, with an ASCII Host header.
	connectRefusals(t, slices.DeleteFunc(slices.Clone(nonASCIIHosts), func(a string) bool {
		return a == "[２００１:4860:4860::8888]" || a == "[::ffff:８.8.8.8]"
	}), nonASCIIHostDetail)
}

// connectRefusals CONNECTs to each authority:443 through a proxy that allows all egress and chains a corp
// proxy, and wants a 403 with detail and nothing handed to the corp proxy; then a control goes through.
func connectRefusals(t *testing.T, authorities []string, detail string) {
	t.Helper()
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newUpstreamProxyPolicy(t, up, types.RunPolicySpec{AllowAllEgress: true}))
	defer srv.Close()
	for _, a := range authorities {
		conn, status := connectWithHost(t, srv.URL, a+":443", "x.test:443")
		_ = conn.Close()
		if !strings.Contains(status, " 403 ") || !strings.Contains(status, egressHeaderDetail+": "+detail+"\r\n") {
			t.Errorf("CONNECT %s:443 = %q, want a 403 with the detail %q", a, status, detail)
		}
		if got, _ := f.snapshot(); got != "" {
			t.Fatalf("the corp proxy was handed %q for %s", got, a)
		}
	}
	conn, status := connectThrough(t, srv.URL, "1.1.1.1:443")
	_ = conn.Close()
	if got, _ := f.snapshot(); !strings.Contains(status, "200") || got != "CONNECT 1.1.1.1:443" {
		t.Fatalf("control: status %q, corp proxy saw %q", status, got)
	}
}

// A host table compiled from an operator's IP literal is keyed by the address (hostKey), as the policy
// and the injection rules are, so a request in the standard spelling meets the refusal or the door the
// entry configures whichever standard spelling the operator wrote ([::ffff:1.2.3.4], an expanded IPv6),
// and a request in a non-standard spelling is refused before any of them is asked.
func TestIPLiteralEntriesMeetEveryRequestSpelling(t *testing.T) {
	allowAll := types.RunPolicySpec{AllowAllEgress: true, HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: "1.2.3.4", Paths: []string{"/ok"}}}}}}
	p, _ := newRuleTestProxy(t, allowAll, "127.0.0.1:1", false, "", nil)
	p.mcp = newMCPBroker([]MCPServerConfig{{Key: "m", URL: "https://1.2.3.4/mcp"}}, nil) // validateMCP refuses an IPv6 MCP URL
	p.localModels = compileLocalModels(map[string]types.LocalModelSettings{"http://[::ffff:1.2.3.6]:11434": {Models: []string{"m"}}}, nil)
	for authority, want := range map[string]string{
		"1.2.3.6":  ruleSourceLocalModel,
		"16909060": ruleSourceIPSpelling, "01.02.03.04": ruleSourceIPSpelling, // aliases of the MCP server and rule host 1.2.3.4
		"1.2.3.4": ruleSourceMCPDirect,
	} {
		host, port := splitHostPort(authority+":443", 443)
		if d, _, log := p.evaluate(context.Background(), host, port, http.MethodConnect, ""); d != egress.Deny || log == nil || log.RuleSource != want {
			t.Errorf("%s = %s %v, want deny %s", authority, d, log, want)
		}
	}
	if !p.policy.namesRuleHost("1.2.3.4") {
		t.Error("the host rule does not name its own address")
	}

	// The doors, gates and routes keyed through patAPIHost, azureHostKey and the Azure DevOps grants, and
	// the via_org routes.
	p.kube = newKubeAPIDoor(&KubeAPIGrant{Host: "::ffff:1.2.3.7", Port: 6443, Namespaces: []string{"a"}}, map[string]bool{}, map[string]int{})
	if !p.isKubeAPIHost("1.2.3.7") {
		t.Error("a kubectl door written [::ffff:1.2.3.7] does not see a request to 1.2.3.7")
	}
	if got := patAPIHost("2606:4700:0000::10"); got != "2606:4700::10" {
		t.Errorf("patAPIHost(2606:4700:0000::10) = %q", got)
	}
	if g := newAzureGates([]AzureGateConfig{{Host: "::ffff:1.2.3.8", Route: "chat"}}); len(g.forHost("1.2.3.8")) != 1 {
		t.Error("an Azure gate written [::ffff:1.2.3.8] does not gate a request to 1.2.3.8")
	}
	if _, ok := newADOGrantsByHost(&ADOGrantConfig{Organization: "o", Hosts: []string{"::ffff:1.2.3.11"}}).ADOGrantFor("1.2.3.11"); !ok {
		t.Error("an Azure DevOps grant written [::ffff:1.2.3.11] does not gate a request to 1.2.3.11")
	}
	p.viaOrgRoutes = []ViaOrgRoute{{GrantID: uuid.New(), Host: "::ffff:1.2.3.9"}}
	if _, ok := p.viaOrgRouteForHost("1.2.3.9"); !ok {
		t.Error("a via_org route written [::ffff:1.2.3.9] does not carry a request to 1.2.3.9")
	}

	// The organisation's engine keys its destination the same way: a grant stored as ::ffff:1.2.3.10 meets
	// the cleartext refusal its rule for 1.2.3.10 sets.
	f := newViaOrgFixture(t, nil, func(c *Config) {
		c.MITMHosts = []string{"http://1.2.3.10"}
		c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "1.2.3.10")
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/x", strings.NewReader("{}"))
	r.Host = "[::ffff:1.2.3.10]"
	f.engine.ServeInjected(w, r, "::ffff:1.2.3.10")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errViaOrgCleartext.Error()) {
		t.Errorf("a cleartext destination stored as ::ffff:1.2.3.10 = %d %q, want 403 %q", w.Code, w.Body.String(), errViaOrgCleartext)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a cleartext destination was dialled %d times", hits)
	}
}

// connectWithHost is connectThrough with its own Host header: net/http refuses a non-ASCII Host header but
// reads the CONNECT target from the request line.
func connectWithHost(t *testing.T, proxyURL, hostport, hostHeader string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "CONNECT "+hostport+" HTTP/1.1\r\nHost: "+hostHeader+"\r\n\r\n")
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(buf)
	return conn, string(buf[:n])
}

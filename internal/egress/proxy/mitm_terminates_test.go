// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestTerminatesTLSMatchesHandleConnect pins Config.TerminatesTLS to the decision handleConnect
// makes at its two TLS-terminating branches, over a real Proxy built from the same settings.
func TestTerminatesTLSMatchesHandleConnect(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	ghGrant := &GitHubAPIGrant{Grants: map[string]uuid.UUID{"o": uuid.New()}}
	patGrants := map[string]PATGrant{"git.corp.example": {GrantID: uuid.New(), API: true}}
	mitm := []string{"artifacts.corp.example:8443", "plain.corp.example", "api.openai.com:8443"}

	withARM := func(c *Config, o *Options) {
		g := &AzureARMGrant{TenantID: "t", Subscriptions: []string{"s"}}
		c.AzureARM, o.AzureARM = g, g
	}
	withSigV4 := func(c *Config, o *Options) {
		g := &AWSSigV4Config{GrantID: uuid.New(), PortalHost: sigv4Portal, AccountID: sigv4Account, RoleName: sigv4Role, Hosts: []string{sigv4Bedrock}}
		c.AWSSigV4, o.AWSSigV4 = g, g
	}
	withKube := func(c *Config, o *Options) {
		g := &KubeAPIGrant{Host: "kube.corp.example", Port: 6443, Namespaces: []string{"ns"}}
		c.KubeAPI, o.KubeAPI = g, g
	}
	withViaOrg := func(c *Config, o *Options) {
		c.ViaOrgRoutes = []ViaOrgRoute{{GrantID: uuid.New(), Host: "pkg.corp.example"}}
		o.ViaOrgRoutes = c.ViaOrgRoutes
	}
	withViaOrgArtifacts := func(c *Config, o *Options) {
		c.ViaOrgRoutes = []ViaOrgRoute{{GrantID: uuid.New(), Host: "artifacts.corp.example"}}
		o.ViaOrgRoutes = c.ViaOrgRoutes
	}

	cases := []struct {
		name  string
		ca    bool
		llm   bool
		hosts []string
		host  string
		port  int
		want  bool
		mod   func(*Config, *Options)
	}{
		{"llm host, MITMLLM on", true, true, nil, "api.anthropic.com", 443, true, nil},
		{"llm host, MITMLLM off", true, false, nil, "api.anthropic.com", 443, false, nil},
		{"openai host, MITMLLM on", true, true, nil, "api.openai.com", 443, true, nil},
		{"llm host, other port, no entry", true, true, nil, "api.anthropic.com", 8080, true, nil},
		{"llm host entry scoped to another port", true, true, mitm, "api.openai.com", 443, false, nil},
		{"llm host entry scoped, its port", true, true, mitm, "api.openai.com", 8443, true, nil},
		{"llm host, no CA", false, true, nil, "api.anthropic.com", 443, false, nil},
		{"corp host, its port", true, false, mitm, "artifacts.corp.example", 8443, true, nil},
		{"corp host, other port", true, false, mitm, "artifacts.corp.example", 443, false, nil},
		{"corp host, no port in the entry", true, false, mitm, "plain.corp.example", 9999, true, nil},
		{"corp host, no CA", false, false, mitm, "artifacts.corp.example", 8443, false, nil},
		{"host in neither", true, true, mitm, "pypi.org", 443, false, nil},
		{"uppercase spelling", true, false, mitm, "ARTIFACTS.Corp.Example", 8443, true, nil},
		{"trailing dot", true, false, mitm, "artifacts.corp.example.", 8443, true, nil},
		{"uppercase llm host", true, true, nil, "API.Anthropic.COM.", 443, true, nil},
		{"github api door host", true, false, nil, "api.github.com", 443, true, nil},
		{"github api door host, other port", true, false, nil, "api.github.com", 8443, false, nil},
		{"pat api door host", true, false, nil, "git.corp.example", 443, true, nil},
		{"azure arm door host", true, false, nil, armscope.Host, 443, true, withARM},
		{"azure arm door host, other port", true, false, nil, armscope.Host, 8443, false, withARM},
		{"azure login door host", true, false, nil, AzureLoginHost, 443, true, withARM},
		{"azure arm door, no CA", false, false, nil, armscope.Host, 443, false, withARM},
		{"sigv4 door host", true, false, nil, sigv4Bedrock, 443, true, withSigV4},
		{"sigv4 door host, other port", true, false, nil, sigv4Bedrock, 8443, false, withSigV4},
		{"sigv4 door, no CA", false, false, nil, sigv4Bedrock, 443, false, withSigV4},
		{"kube door host, its port", true, false, nil, "kube.corp.example", 6443, true, withKube},
		{"kube door host, port 443", true, false, nil, "kube.corp.example", 443, false, withKube},
		{"via_org host, 443", true, false, nil, "pkg.corp.example", 443, true, withViaOrg},
		{"via_org host, other port", true, false, nil, "pkg.corp.example", 8443, true, withViaOrg},
		{"via_org host, uppercase", true, false, nil, "PKG.Corp.Example", 9999, true, withViaOrg},
		{"via_org host, no CA", false, false, nil, "pkg.corp.example", 443, false, withViaOrg},
		{"via_org host also a scoped corp host, other port", true, false, mitm, "artifacts.corp.example", 9999, true, withViaOrgArtifacts},
		{"host a via_org route does not name", true, false, nil, "other.corp.example", 443, false, withViaOrg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MITMHosts: tc.hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, PATGrants: patGrants}
			opts := Options{
				RunID: uuid.New(), Sink: &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
				MITMHosts: tc.hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, PATGrants: patGrants,
			}
			if tc.mod != nil {
				tc.mod(cfg, &opts)
			}
			if tc.ca {
				cfg.MITMCACertPEM, cfg.MITMCAKeyPEM = string(certPEM), string(keyPEM)
				opts.CA = ca
			}
			p := newProxy(opts)
			_, viaOrg := p.viaOrgRouteForHost(strings.TrimSuffix(tc.host, "."))
			real := p.ca != nil && viaOrg ||
				p.ca != nil && p.isCorpMITMHost(tc.host) && p.mitmPortAllowed(tc.host, tc.port) ||
				p.isLLMHost(tc.host) && p.mitmLLMHost(tc.host, tc.port)
			if real != tc.want {
				t.Fatalf("the proxy's own decision = %v, case expects %v", real, tc.want)
			}
			if got := cfg.TerminatesTLS(tc.host, tc.port); got != real {
				t.Fatalf("TerminatesTLS(%q, %d) = %v, the proxy decides %v", tc.host, tc.port, got, real)
			}
		})
	}
}

func TestInspectsEntry(t *testing.T) {
	cfg := func(llm, ca bool, hosts ...string) *Config {
		c := &Config{MITMHosts: hosts, MITMLLM: llm}
		if ca {
			c.MITMCACertPEM, c.MITMCAKeyPEM = "cert", "key"
		}
		return c
	}
	viaOrgCfg := func(ca bool, host string) *Config {
		c := cfg(false, ca)
		c.ViaOrgRoutes = []ViaOrgRoute{{GrantID: uuid.New(), Host: host}}
		return c
	}
	for _, tc := range []struct {
		name  string
		c     *Config
		entry string
		want  bool
	}{
		{"bare llm host, MITMLLM on", cfg(true, true), "api.anthropic.com", true},
		{"bare llm host, MITMLLM off", cfg(false, true), "api.anthropic.com", false},
		{"bare corp host on a scoped port, any port counts", cfg(false, true, "artifacts.corp.example:8443"), "artifacts.corp.example", true},
		{"bare corp host, no CA", cfg(false, false, "artifacts.corp.example:8443"), "artifacts.corp.example", false},
		{"bare via_org host", viaOrgCfg(true, "pkg.corp.example"), "pkg.corp.example", true},
		{"bare via_org host, no CA", viaOrgCfg(false, "pkg.corp.example"), "pkg.corp.example", false},
		{"via_org host with any port", viaOrgCfg(true, "pkg.corp.example"), "pkg.corp.example:8443", true},
		{"wildcard over a via_org host", viaOrgCfg(true, "pkg.corp.example"), "*.corp.example", true},
		{"wildcard with a port over a via_org host", viaOrgCfg(true, "pkg.corp.example"), "*.corp.example:8443", true},
		{"bare host in neither", cfg(true, true, "artifacts.corp.example"), "pypi.org", false},
		{"corp host entry with its port", cfg(false, true, "artifacts.corp.example:8443"), "artifacts.corp.example:8443", true},
		{"wildcard over an inspected llm host", cfg(true, true), "*.anthropic.com", true},
		{"wildcard over an inspected corp host", cfg(false, true, "artifacts.corp.example"), "*.corp.example", true},
		{"wildcard over a scoped corp host, bare", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example", true},
		{"wildcard with another port than the corp host", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example:443", false},
		{"wildcard with the corp host's port", cfg(false, true, "artifacts.corp.example:8443"), "*.corp.example:8443", true},
		{"wildcard covering nothing", cfg(true, true, "artifacts.corp.example"), "*.pypi.org", false},
		{"wildcard over the llm host with MITMLLM off", cfg(false, true), "*.anthropic.com", false},
		{"wildcard over a corp host, no CA", cfg(true, false, "artifacts.corp.example"), "*.corp.example", false},
		{"wildcard does not match its own apex", cfg(false, true, "corp.example"), "*.corp.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.InspectsEntry(tc.entry); got != tc.want {
				t.Fatalf("InspectsEntry(%q) = %v, want %v", tc.entry, got, tc.want)
			}
		})
	}
}

func TestTerminatedHosts(t *testing.T) {
	cfg := func(llm, ca bool, hosts ...string) *Config {
		c := &Config{MITMHosts: hosts, MITMLLM: llm}
		if ca {
			c.MITMCACertPEM, c.MITMCAKeyPEM = "cert", "key"
		}
		return c
	}
	viaOrg := func(c *Config, hosts ...string) *Config {
		for _, h := range hosts {
			c.ViaOrgRoutes = append(c.ViaOrgRoutes, ViaOrgRoute{GrantID: uuid.New(), Host: h})
		}
		return c
	}
	for _, tc := range []struct {
		name string
		c    *Config
		want []string
	}{
		{"no CA", viaOrg(cfg(true, false, "a.corp.example"), "v.corp.example"), []string{}},
		{"nothing configured", cfg(false, true), []string{}},
		{"MITMLLM on", cfg(true, true), []string{"api.anthropic.com", "api.openai.com"}},
		{"MITMLLM off", cfg(false, true), []string{}},
		{"corp hosts, one scoped", cfg(false, true, "b.corp.example:8443", "a.corp.example"), []string{"a.corp.example", "b.corp.example:8443"}},
		{"via_org host", viaOrg(cfg(false, true), "V.Corp.Example."), []string{"v.corp.example"}},
		{"via_org host that is also scoped reads any port", viaOrg(cfg(false, true, "v.corp.example:8443"), "v.corp.example"), []string{"v.corp.example"}},
		{"via_org with MITMLLM", viaOrg(cfg(true, true, "a.corp.example"), "v.corp.example"), []string{"a.corp.example", "api.anthropic.com", "api.openai.com", "v.corp.example"}},
		{"llm host scoped by an entry", cfg(true, true, "api.openai.com:8443"), []string{"api.anthropic.com", "api.openai.com:8443"}},
		{"llm host scoped, MITMLLM off", cfg(false, true, "api.openai.com:8443"), []string{}},
		{"door hosts", func() *Config {
			c := cfg(false, true)
			c.KubeAPI = &KubeAPIGrant{Host: "kube.corp.example", Port: 6443, Namespaces: []string{"ns"}}
			c.AzureARM = &AzureARMGrant{TenantID: "t", Subscriptions: []string{"s"}}
			return c
		}(), []string{"kube.corp.example:6443", AzureLoginHost + ":443", armscope.Host + ":443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.TerminatedHosts()
			if got == nil || !slices.Equal(got, tc.want) {
				t.Fatalf("TerminatedHosts() = %#v, want %#v", got, tc.want)
			}
			for _, e := range got {
				host, port, _ := strings.Cut(e, ":")
				_ = port
				if !tc.c.InspectsEntry(e) || !tc.c.InspectsEntry(host) {
					t.Errorf("%q is listed as terminated but InspectsEntry disagrees", e)
				}
			}
		})
	}
}

func TestTerminatedOutside(t *testing.T) {
	c := &Config{MITMHosts: []string{"a.corp.example", "b.corp.example:8443", "c.other.example"}, MITMLLM: true,
		MITMCACertPEM: "cert", MITMCAKeyPEM: "key", ViaOrgRoutes: []ViaOrgRoute{{GrantID: uuid.New(), Host: "v.corp.example"}},
		Policy: types.RunPolicySpec{AllowAllEgress: true}}
	for _, tc := range []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"nothing allowed", nil, []string{"a.corp.example", "api.anthropic.com", "api.openai.com", "b.corp.example:8443", "c.other.example", "v.corp.example"}},
		{"exact and bare cover host and host:port", []string{"a.corp.example", "b.corp.example", "api.anthropic.com", "api.openai.com", "c.other.example", "v.corp.example"}, []string{}},
		{"a wildcard covers its hosts", []string{"*.corp.example", "*.anthropic.com", "*.openai.com", "c.other.example"}, []string{}},
		{"an exact entry on another port still names the host", []string{"b.corp.example:443", "v.corp.example:443"}, []string{"a.corp.example", "api.anthropic.com", "api.openai.com", "c.other.example"}},
		{"a wildcard covers its ports only", []string{"*.corp.example:8443"}, []string{"a.corp.example", "api.anthropic.com", "api.openai.com", "c.other.example", "v.corp.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.TerminatedOutside(tc.allowed); !slices.Equal(got, tc.want) {
				t.Fatalf("TerminatedOutside(%v) = %#v, want %#v", tc.allowed, got, tc.want)
			}
		})
	}
	if got := (&Config{MITMHosts: []string{"a.corp.example"}, Policy: c.Policy}).TerminatedOutside(nil); got == nil || len(got) != 0 {
		t.Fatalf("without a CA = %#v, want an empty non-nil list", got)
	}
}

// TestTerminatedOutsideMatchesConnect pins Config.TerminatedOutside to the decision the real *Proxy
// gives a CONNECT: a terminated host no entry names is listed exactly when it is a via_org host, or the
// proxy admits it or holds it for first-use approval, and not when it refuses it. The three-way result
// (gateConnect) is checked against the proxy's own decideConnect for every host.
func TestTerminatedOutsideMatchesConnect(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	ghGrant := &GitHubAPIGrant{Grants: map[string]uuid.UUID{"o": uuid.New()}}
	viaOrg := []ViaOrgRoute{{GrantID: uuid.New(), Host: "v.corp.example"}}
	hosts := []string{"artifacts.corp.example:8443", "plain.corp.example"}
	pol := func(mode types.FirstUseMode, allowAll bool, allowed, denied []string) types.RunPolicySpec {
		return types.RunPolicySpec{FirstUseApproval: mode, AllowAllEgress: allowAll, AllowedDomains: allowed, DeniedDomains: denied}
	}
	for _, tc := range []struct {
		name    string
		llm     bool
		portRul string
		policy  types.RunPolicySpec
		allowed []string
		want    []string
	}{
		{"llm hosts over an allowlist without them are refused", true, "",
			pol(types.FirstUseAlwaysDeny, false, []string{"pypi.org"}, nil), []string{"pypi.org"}, []string{"v.corp.example"}},
		{"llm hosts a first-use review would hold are listed", true, "",
			pol(types.FirstUseDenyWithReview, false, []string{"pypi.org"}, nil), []string{"pypi.org"},
			[]string{"api.anthropic.com", "api.github.com:443", "api.openai.com", "artifacts.corp.example:8443", "plain.corp.example", "v.corp.example"}},
		{"a door host outside the allowlist is refused, one on it is named", false, "",
			pol(types.FirstUseAlwaysDeny, false, []string{"api.github.com"}, nil), []string{"api.github.com"}, []string{"v.corp.example"}},
		{"under allow-all everything not denied is listed, a denied host is not", true, "",
			pol(types.FirstUseAlwaysDeny, true, nil, []string{"api.openai.com", "plain.corp.example"}), nil,
			[]string{"api.anthropic.com", "api.github.com:443", "artifacts.corp.example:8443", "v.corp.example"}},
		{"a via_org host that is also denied is still listed", false, "",
			pol(types.FirstUseAlwaysDeny, true, nil, []string{"v.corp.example"}), []string{"api.github.com", "plain.corp.example", "artifacts.corp.example"}, []string{"v.corp.example"}},
		{"a port-scoped entry names an any-port host", false, "",
			pol(types.FirstUseAlwaysDeny, false, []string{"plain.corp.example:8443"}, nil), []string{"plain.corp.example:8443"}, []string{"v.corp.example"}},
		{"a method restriction without CONNECT refuses all but via_org", true, "",
			types.RunPolicySpec{AllowAllEgress: true, AllowedMethods: []string{"GET"}}, nil, []string{"v.corp.example"}},
		{"the port rule closes a scoped port", false, PortRuleOn,
			pol(types.FirstUseAlwaysDeny, true, nil, nil), nil, []string{"api.github.com:443", "plain.corp.example", "v.corp.example"}},
		{"the port rule dry run still admits it", false, PortRuleDryRun,
			pol(types.FirstUseAlwaysDeny, true, nil, nil), nil, []string{"api.github.com:443", "artifacts.corp.example:8443", "plain.corp.example", "v.corp.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{MITMHosts: hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, ViaOrgRoutes: viaOrg, PortRule: tc.portRul, Policy: tc.policy,
				MITMCACertPEM: string(certPEM), MITMCAKeyPEM: string(keyPEM)}
			p := newProxy(Options{
				RunID: uuid.New(), Policy: CompilePolicy(tc.policy), Sink: &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
				Approval: newApprovalClient("http://127.0.0.1:1", newTokenSource("tok"), uuid.New(), http.DefaultClient), Resolver: publicResolver{}, CA: ca,
				MITMHosts: hosts, MITMLLM: tc.llm, GitHubAPI: ghGrant, ViaOrgRoutes: viaOrg,
			})
			p.portRule = effectivePortRule(cfg.VM, cfg.PortRule)
			got := cfg.TerminatedOutside(tc.allowed)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("TerminatedOutside(%v) = %#v, want %#v", tc.allowed, got, tc.want)
			}
			canAsk := p.policy.FirstUseMode().RaisesApproval()
			for _, tok := range cfg.TerminatedHosts() {
				host, ps, scoped := strings.Cut(tok, ":")
				port := 443
				if scoped {
					port, _ = strconv.Atoi(ps)
				}
				d, _, _ := p.decideConnect(context.Background(), "", "", host, port)
				real := map[egress.Decision]connectOutcome{egress.Allow: connectAdmit, egress.Pending: connectHold, egress.Deny: connectRefuse}[d]
				req := egress.Request{Host: host, Port: port, Method: http.MethodConnect}
				if g := gateConnect(context.Background(), req, p.evaluator, p.policy, p.portRule).outcome(canAsk); g != real {
					t.Errorf("%s:%d: the gate decides %v, the proxy's CONNECT decision is %v (%v)", host, port, g, real, d)
				}
				if slices.ContainsFunc(tc.allowed, func(e string) bool { x, _, _ := domainmatch.Classify(e); return x == host }) {
					if slices.Contains(got, tok) {
						t.Errorf("%s is named by an entry and is still listed", tok)
					}
					continue
				}
				_, isViaOrg := p.viaOrgRouteForHost(host)
				if want := isViaOrg || real != connectRefuse; slices.Contains(got, tok) != want {
					t.Errorf("%s: listed = %v, the proxy's own decision (%v, via_org %v) says %v", tok, slices.Contains(got, tok), d, isViaOrg, want)
				}
			}
		})
	}
}

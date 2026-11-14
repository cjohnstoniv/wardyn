// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// forgeRig is a proxy whose every dial lands on one local upstream and is
// counted, so a refusal can be shown to have sent nothing upstream.
type forgeRig struct {
	p     *Proxy
	sink  *bytes.Buffer
	dials *atomic.Int32
	url   string
}

func newForgeRig(t *testing.T, spec types.RunPolicySpec, wire func(p *Proxy)) *forgeRig {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("upstream")) }))
	t.Cleanup(up.Close)
	dials := &atomic.Int32{}
	dial := redirectDial(upstreamAddr(up))
	sink := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(spec),
		Sink:     &decisionSink{out: sink, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return dial(ctx, network, addr)
		},
	})
	if wire != nil {
		wire(p)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return &forgeRig{p: p, sink: sink, dials: dials, url: srv.URL}
}

// connect sends a CONNECT for hostport and reports whether the tunnel opened.
func (r *forgeRig) connect(t *testing.T, hostport string) (opened bool, resp string) {
	t.Helper()
	conn, resp := connectThrough(t, r.url, hostport)
	_ = conn.Close()
	return strings.HasPrefix(resp, "HTTP/1.1 200"), resp
}

// plain sends an absolute-form request on the plain lane.
func (r *forgeRig) plain(t *testing.T, rawurl string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, rawurl))
	return rec
}

// refusedConnect asserts the forge guard refused a CONNECT to hostport and
// that nothing was dialled.
func (r *forgeRig) refusedConnect(t *testing.T, hostport, host string) {
	t.Helper()
	before := r.dials.Load()
	opened, resp := r.connect(t, hostport)
	if opened || !strings.HasPrefix(resp, "HTTP/1.1 403") || !strings.Contains(resp, pushDenyAllForgeRefusal(host)) {
		t.Fatalf("CONNECT %s: response %q, want 403 carrying %q", hostport, resp, pushDenyAllForgeRefusal(host))
	}
	if !strings.Contains(r.sink.String(), `"rule_source":"`+ruleSourceGitPushDenyAllForge+`"`) {
		t.Errorf("CONNECT %s: decision log %q, want a %s row", hostport, r.sink.String(), ruleSourceGitPushDenyAllForge)
	}
	if n := r.dials.Load() - before; n != 0 {
		t.Errorf("CONNECT %s: a refused connection dialled upstream %d time(s)", hostport, n)
	}
}

func (r *forgeRig) openedConnect(t *testing.T, hostport string) {
	t.Helper()
	if opened, resp := r.connect(t, hostport); !opened {
		t.Fatalf("CONNECT %s: response %q, want the tunnel open", hostport, resp)
	}
}

func denyAllAllowing(domains ...string) types.RunPolicySpec {
	s := denyAllSpec()
	s.AllowedDomains = domains
	return s
}

// forgeHostsAllowedPlainly are the public code hosts the guard refuses, each
// allowed as a plain domain.
var forgeHostsAllowedPlainly = []string{"github.com", "uploads.github.com", "api.github.com", "gist.github.com",
	"ssh.github.com", "dev.azure.com", "ssh.dev.azure.com", "acme.visualstudio.com"}

// Under deny_all a public code host allowed as a plain domain is refused on
// both lanes and on any port, and nothing reaches upstream.
func TestPushDenyAllForge_PlainlyAllowedPublicHostRefused(t *testing.T) {
	for _, host := range forgeHostsAllowedPlainly {
		t.Run(host, func(t *testing.T) {
			r := newForgeRig(t, denyAllAllowing(host), nil)
			for _, port := range []string{"443", "8443", "22"} {
				r.refusedConnect(t, host+":"+port, host)
			}
			rec := r.plain(t, "http://"+host+"/acme/app.git/info/refs?service=git-receive-pack")
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), pushDenyAllForgeRefusal(host)) {
				t.Fatalf("plain lane: status %d body %q, want 403 with the refusal", rec.Code, rec.Body)
			}
			if n := r.dials.Load(); n != 0 {
				t.Errorf("plain lane reached upstream %d time(s)", n)
			}
		})
	}
}

// Another spelling of a known host is the same host: case and a trailing dot.
// An internationalised spelling that maps onto it is refused at step 0 first.
func TestPushDenyAllForge_HostSpellings(t *testing.T) {
	r := newForgeRig(t, types.RunPolicySpec{AllowAllEgress: true, PushRules: &types.PushRulesSpec{DenyAll: true}}, nil)
	r.refusedConnect(t, "GitHub.COM.:443", "github.com")
	r.refusedConnect(t, "UPLOADS.github.com..:443", "uploads.github.com")
	// A full-width spelling never reaches the forge guard: step 0 refuses every non-ASCII host first.
	for host, want := range map[string]string{"ｇｉｔｈｕｂ.com": ruleSourceNonASCIIHost, "GITHUB.com.": ruleSourceGitPushDenyAllForge, "Dev.Azure.Com": ruleSourceGitPushDenyAllForge} {
		d, _, log := r.p.evaluate(context.Background(), host, 443, http.MethodConnect, "")
		if d != egress.Deny || log == nil || log.RuleSource != want {
			t.Errorf("%q: decision %v %+v, want a %s deny", host, d, log, want)
		}
	}
}

// An allowed wildcard, or allow_all_egress, cannot reopen a refused host.
func TestPushDenyAllForge_WildcardAndAllowAllDoNotReopen(t *testing.T) {
	for name, spec := range map[string]types.RunPolicySpec{
		"*.github.com":     denyAllAllowing("*.github.com", "*.visualstudio.com", "*.dev.azure.com"),
		"allow_all_egress": {AllowAllEgress: true, PushRules: &types.PushRulesSpec{DenyAll: true}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newForgeRig(t, spec, nil)
			for _, host := range []string{"uploads.github.com", "api.github.com", "gist.github.com", "acme.visualstudio.com", "ssh.dev.azure.com"} {
				r.refusedConnect(t, host+":443", host)
			}
		})
	}
}

// A first-use approval is never asked for a refused host: the guard answers
// before the ask.
func TestPushDenyAllForge_NoApprovalAsked(t *testing.T) {
	spec := types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview, PushRules: &types.PushRulesSpec{DenyAll: true}}
	r := newForgeRig(t, spec, func(p *Proxy) {
		p.approval = newApprovalClient("http://127.0.0.1:1", newTokenSource("tok"), uuid.New(), &http.Client{})
	})
	d, _, log := r.p.evaluate(context.Background(), "uploads.github.com", 443, http.MethodConnect, "")
	if d != egress.Deny || log == nil || log.RuleSource != ruleSourceGitPushDenyAllForge {
		t.Fatalf("decision %v %+v, want the guard's deny before any approval", d, log)
	}
}

// A self-hosted code host dispatch names (a GitHub Enterprise row, say), or
// one a git_pat grant names, allowed plainly and served by no door, is refused.
func TestPushDenyAllForge_ConfiguredAndGrantHostsRefused(t *testing.T) {
	t.Run("configured GitHub Enterprise host", func(t *testing.T) {
		r := newForgeRig(t, denyAllAllowing("ghe.corp.example", "api.ghe.corp.example"), func(p *Proxy) {
			p.forgeHosts = forgeHostSet([]string{"GHE.corp.example.", "api.ghe.corp.example"})
		})
		r.refusedConnect(t, "ghe.corp.example:443", "ghe.corp.example")
		r.refusedConnect(t, "api.ghe.corp.example:8443", "api.ghe.corp.example")
	})
	t.Run("git_pat grant host on the broker lane", func(t *testing.T) {
		r := newForgeRig(t, denyAllAllowing("gitlab.corp.example"), func(p *Proxy) {
			p.patGrants = canonicalPATGrants(map[string]PATGrant{"gitlab.corp.example": {GrantID: uuid.New()}})
		})
		r.refusedConnect(t, "gitlab.corp.example:443", "gitlab.corp.example")
	})
	t.Run("git_pat grant host named by dispatch", func(t *testing.T) {
		r := newForgeRig(t, denyAllAllowing("bitbucket.corp.example"), func(p *Proxy) {
			p.forgeHosts = forgeHostSet([]string{"bitbucket.corp.example"})
		})
		r.refusedConnect(t, "bitbucket.corp.example:7999", "bitbucket.corp.example")
	})
	t.Run("push rule set entry host", func(t *testing.T) {
		r := newForgeRig(t, types.RunPolicySpec{AllowedDomains: []string{"ado.corp.example"}}, func(p *Proxy) {
			p.pushSets = []types.PushRuleSet{{Provider: string(types.GitProviderAzureDevOps), Host: "ado.corp.example", Org: "tfs",
				Rules: types.PushRulesSpec{DenyAll: true}}}
		})
		r.refusedConnect(t, "ado.corp.example:443", "ado.corp.example")
	})
}

// Without deny_all every one of those hosts is reached as before, and under
// deny_all a host that is not a code host, or codeload.github.com (archive
// downloads only), still is.
func TestPushDenyAllForge_UnchangedWithoutDenyAll(t *testing.T) {
	hosts := append(append([]string(nil), forgeHostsAllowedPlainly...), "ghe.corp.example", "gitlab.corp.example")
	wire := func(p *Proxy) {
		p.forgeHosts = forgeHostSet([]string{"ghe.corp.example"})
		p.patGrants = canonicalPATGrants(map[string]PATGrant{"gitlab.corp.example": {GrantID: uuid.New()}})
	}
	r := newForgeRig(t, types.RunPolicySpec{GitPushAnyBranch: true, AllowedDomains: append([]string{"*.github.com"}, hosts...),
		PushRules: &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}}, wire)
	for _, host := range hosts {
		r.openedConnect(t, host+":443")
		if rec := r.plain(t, "http://"+host+"/"); rec.Code != http.StatusOK {
			t.Errorf("plain %s: status %d body %q, want 200", host, rec.Code, rec.Body)
		}
	}
	if strings.Contains(r.sink.String(), ruleSourceGitPushDenyAllForge) {
		t.Errorf("the guard ran without deny_all: %s", r.sink.String())
	}
	r = newForgeRig(t, denyAllAllowing("codeload.github.com", "registry.npmjs.org", "github.com.evil.example"), wire)
	for _, host := range []string{"codeload.github.com", "registry.npmjs.org", "github.com.evil.example"} {
		r.openedConnect(t, host+":443")
	}
}

// The GitHub API door's host under deny_all is still the door's: the CONNECT
// passes the guard to be terminated, a read through the door passes, and a
// tunnel on another port is still the door's refusal. A sibling host the
// door does not serve is refused by the guard.
func TestPushDenyAllForge_GitHubAPIDoorUnchanged(t *testing.T) {
	up := newGHUpstream(t)
	spec := denyAllAllowing("api.github.com", "uploads.github.com")
	p, _ := newGHProxySpec(t, up, spec)
	d, _, log := p.evaluate(context.Background(), ghscope.APIHost, 443, http.MethodConnect, "")
	if d != egress.Allow {
		t.Fatalf("door host CONNECT: decision %v %+v, want allow (the door terminates it)", d, log)
	}
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues?state=open"), ghscope.APIHost, 443)
	if rec.Code != http.StatusOK {
		t.Fatalf("a read through the door: status %d body %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodConnect, "api.github.com:8443", nil)
	req.Host = "api.github.com:8443"
	rec = httptest.NewRecorder()
	p.handleConnect(rec, req)
	ghRefused(t, rec)
	if d, _, log := p.evaluate(context.Background(), "uploads.github.com", 443, http.MethodConnect, ""); d != egress.Deny ||
		log == nil || log.RuleSource != ruleSourceGitPushDenyAllForge {
		t.Fatalf("uploads.github.com beside the door: decision %v %+v, want the guard's deny", d, log)
	}
}

// The GitHub App broker lane under deny_all: the fetch through the broker
// passes and the push is refused by the broker, as before. A CONNECT to
// github.com is no broker route, so the guard refuses it.
func TestPushDenyAllForge_GitBrokerLaneUnchanged(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
		upstreamAddr(up.srv), denyAllAllowing("github.com"))
	base := "/wardyn/gh/octocat/hello-world"
	for _, req := range []*http.Request{
		mustLocalReq(t, http.MethodGet, base+"/info/refs?service=git-upload-pack", nil),
		mustLocalReq(t, http.MethodPost, base+"/git-upload-pack", strings.NewReader(pkt("want "+someOID+"\n")+"0000")),
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("fetch %s: status %d body %q", req.URL, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, base+"/git-receive-pack",
		strings.NewReader(pkt(zeroOID+" "+someOID+" refs/heads/main"+firstCaps)+"0000"+emptyPack())))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), pushDenyAllRefusal) {
		t.Fatalf("push: status %d body %q, want the broker's deny_all refusal", rec.Code, rec.Body)
	}
	if strings.Contains(sink.String(), ruleSourceGitPushDenyAllForge) {
		t.Errorf("the broker lane met the forge guard: %s", sink.String())
	}
	if d, _, log := p.evaluate(context.Background(), "github.com", 443, http.MethodConnect, ""); d != egress.Deny ||
		log == nil || log.RuleSource != ruleSourceGitPushDenyAllForge {
		t.Fatalf("CONNECT github.com: decision %v %+v, want the guard's deny", d, log)
	}
}

// A host denied by the policy keeps the policy's own refusal: the guard adds
// a refusal, it never replaces one.
func TestPushDenyAllForge_PolicyDenyKeepsItsRow(t *testing.T) {
	spec := denyAllAllowing("*.github.com")
	spec.DeniedDomains = []string{"github.com"}
	r := newForgeRig(t, spec, nil)
	d, _, log := r.p.evaluate(context.Background(), "github.com", 443, http.MethodConnect, "")
	if d != egress.Deny || log == nil || log.RuleSource != "policy:denied" {
		t.Fatalf("decision %v %+v, want policy:denied", d, log)
	}
}

func TestForgeHostKey(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub.com.":         "github.com",
		"github.com:443":      "github.com",
		" ghe.corp.example. ": "ghe.corp.example",
		"ｇｉｔｈｕｂ.com":          "github.com",
		"bücher.example":      "xn--bcher-kva.example",
	} {
		if got := forgeHostKey(in); got != want {
			t.Errorf("forgeHostKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The editor extension gallery sits under *.visualstudio.com but is not a code
// host: under deny_all it stays reachable, in any spelling, while an Azure
// DevOps organization's *.visualstudio.com address is refused.
func TestPushDenyAllForge_ExtensionGalleryStaysOpen(t *testing.T) {
	r := newForgeRig(t, denyAllAllowing("*.visualstudio.com"), nil)
	for _, hostport := range []string{"marketplace.visualstudio.com:443", "Marketplace.VisualStudio.com.:443"} {
		r.openedConnect(t, hostport)
	}
	if rec := r.plain(t, "http://marketplace.visualstudio.com/"); rec.Code != http.StatusOK {
		t.Errorf("plain marketplace.visualstudio.com: status %d body %q, want 200", rec.Code, rec.Body)
	}
	r.refusedConnect(t, "acme.visualstudio.com:443", "acme.visualstudio.com")
}

// A signed-in browser can commit through a code host's web pages, so the
// desktop sign-in listener refuses a known code host under deny_all too, while
// another allowed sign-in host still tunnels.
func TestPushDenyAllForge_SigninLaneRefused(t *testing.T) {
	rig := newSigninLaneRig(t, func(o *Options) {
		o.Policy = CompilePolicy(denyAllAllowing("site.test", "github.com", "dev.azure.com"))
	})
	for _, host := range []string{"github.com", "dev.azure.com"} {
		c, err := net.Dial("tcp", rig.signinAddr)
		if err != nil {
			t.Fatal(err)
		}
		// An opened tunnel would hold the body open: fail on the deadline instead.
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\nProxy-Authorization: "+
			signinAuth(rig.goodCred)+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("read CONNECT %s response: %v", host, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = c.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body)+resp.Header.Get(egressHeaderDetail), pushDenyAllForgeRefusal(host)) {
			t.Errorf("sign-in CONNECT %s = %d %q, want the deny_all code host refusal", host, resp.StatusCode, body)
		}
	}
	if !strings.Contains(rig.logs.String(), ruleSourceGitPushDenyAllForge) {
		t.Errorf("no %s decision row: %s", ruleSourceGitPushDenyAllForge, rig.logs.String())
	}
	if _, _, status := rig.connect(t, rig.signinAddr, "site.test:443", signinAuth(rig.goodCred)); status != http.StatusOK {
		t.Fatalf("sign-in CONNECT to an allowed host = %d, want 200", status)
	}
}

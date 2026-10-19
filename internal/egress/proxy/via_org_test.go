// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	viaOrgHost   = "example.com" // a name the test upstream's leaves are minted for
	viaOrgSecret = "Bearer ORG-SECRET-VALUE"
)

// orgUpstream is the internet the organisation dials: a TLS server that presents, for any name
// asked, a leaf signed by a test CA the engine trusts (Config.TrustedCAPEM). It records what reached it.
type orgUpstream struct {
	srv    *httptest.Server
	caPEM  []byte
	mu     sync.Mutex
	hits   int
	header http.Header
	path   string
}

func newOrgUpstream(t *testing.T) *orgUpstream {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	u := &orgUpstream{caPEM: certPEM}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		u.header, u.path = r.Header.Clone(), r.URL.Path
		u.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	u.srv.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return ca.leafFor(h.ServerName) }}
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *orgUpstream) seen() (int, http.Header, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits, u.header, u.path
}

// fakeOrg is the in-process control plane an engine's client reaches: it resolves the injection,
// mints the GitHub token, and records decisions and approvals.
type fakeOrg struct {
	mu                               sync.Mutex
	injections, mints, raised, polls int
	gated                            bool          // the GitHub grant needs an approval nobody gives
	value                            string        // the injected header value; viaOrgSecret when empty
	failGrant                        uuid.UUID     // this grant's injection resolve fails
	block                            chan struct{} // decision posts wait on it
	decisions                        strings.Builder
}

func (f *fakeOrg) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.mu.Lock()
	defer f.mu.Unlock()
	p := req.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/v1/internal/injection/") && f.failGrant != uuid.Nil && strings.HasSuffix(p, f.failGrant.String()):
		rec.WriteHeader(http.StatusInternalServerError)
	case strings.HasPrefix(p, "/api/v1/internal/injection/"):
		f.injections++
		v := f.value
		if v == "" {
			v = viaOrgSecret
		}
		_ = json.NewEncoder(rec).Encode(types.ResolvedInjection{Host: viaOrgHost, Header: "Authorization", Value: v, JTI: uuid.NewString()})
	case p == "/api/v1/internal/credentials/mint" && f.gated:
		f.mints++
		rec.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(rec, `{"approval_id":"`+uuid.NewString()+`"}`)
	case p == "/api/v1/internal/credentials/mint":
		f.mints++
		_, _ = io.WriteString(rec, `{"kind":"github_token","token":"gh-inst-token","jti":"j","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	case p == "/api/v1/internal/decisions":
		if f.block != nil {
			<-f.block
		}
		b, _ := io.ReadAll(req.Body)
		f.decisions.Write(b)
		rec.WriteHeader(http.StatusAccepted)
	case p == "/api/v1/internal/approvals" && req.Method == http.MethodPost:
		f.raised++
		rec.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rec).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	case strings.HasPrefix(p, "/api/v1/internal/approvals/"):
		f.polls++
		_ = json.NewEncoder(rec).Encode(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalPending})
	default:
		rec.WriteHeader(http.StatusNoContent)
	}
	return rec.Result(), nil
}

type viaOrgFixture struct {
	engine *ViaOrgEngine
	up     *orgUpstream
	org    *fakeOrg
	runID  uuid.UUID
}

// newViaOrgFixture builds an engine holding one header grant for viaOrgHost and a GitHub grant for
// octocat/hello-world, every dial landing on the test upstream; mutate edits the config first.
func newViaOrgFixture(t *testing.T, lookup func(string) ([]net.IP, error), mutate func(*Config)) *viaOrgFixture {
	t.Helper()
	return newViaOrgFixtureWith(t, &fakeOrg{}, lookup, mutate)
}

func newViaOrgFixtureWith(t *testing.T, org *fakeOrg, lookup func(string) ([]net.IP, error), mutate func(*Config)) *viaOrgFixture {
	t.Helper()
	up := newOrgUpstream(t)
	cfg := &Config{
		RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
		Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgHost, githubHost}},
		Injection: []InjectionConfig{{GrantID: uuid.New(),
			InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization", SecretName: "corp-secret"}}},
		MITMHosts:    []string{viaOrgHost},
		GitGrants:    map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
		TrustedCAPEM: string(up.caPEM),
	}
	if mutate != nil {
		mutate(cfg)
	}
	if lookup == nil {
		lookup = publicResolver{}.LookupIP
	}
	e, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{
		Client: &http.Client{Transport: org}, Dial: redirectDial(upstreamAddr(up.srv)), LookupIP: lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	return &viaOrgFixture{engine: e, up: up, org: org, runID: cfg.RunID}
}

func (f *viaOrgFixture) inject(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.Host = viaOrgHost
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.engine.ServeInjected(w, r, viaOrgHost)
	return w
}

func (f *viaOrgFixture) broker(t *testing.T, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.engine.ServeBroker(w, mustLocalReq(t, http.MethodPost, path, strings.NewReader(body)))
	return w
}

func (f *viaOrgFixture) decisions(t *testing.T) string {
	t.Helper()
	if err := f.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.org.mu.Lock()
	defer f.org.mu.Unlock()
	return f.org.decisions.String()
}

// TestViaOrg_ReRunsTheProxyChecks: every check of design §7.3 runs at the organisation, through the
// engine, before anything is dialed — and an allowed request carries the org's credential, never the
// sandbox's.
func TestViaOrg_ReRunsTheProxyChecks(t *testing.T) {
	t.Run("allowed request carries the org credential", func(t *testing.T) {
		f := newViaOrgFixture(t, nil, nil)
		if w := f.inject(http.MethodPost, "/v1/messages", map[string]string{"Authorization": "Bearer sandbox"}); w.Code != http.StatusOK {
			t.Fatalf("status = %d %s", w.Code, w.Body)
		}
		if hits, h, _ := f.up.seen(); hits != 1 || h.Get("Authorization") != viaOrgSecret {
			t.Fatalf("upstream hits=%d auth=%q, want the org credential", hits, h.Get("Authorization"))
		}
		// The destination's own allow row (handleConnect's, ahead of a terminated tunnel) is written
		// at the org: it is the row an approval's attribution rides.
		if d := f.decisions(t); !strings.Contains(d, `"decision":"allow"`) || !strings.Contains(d, `"rule_source":"policy:allowed"`) {
			t.Errorf("no org-side destination allow row: %s", d)
		}
	})
	t.Run("deny list", func(t *testing.T) {
		// A grant destination the run's policy denies: no engine serves it, so nothing is dialed.
		up := newOrgUpstream(t)
		cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK",
			Policy:    types.RunPolicySpec{AllowedDomains: []string{viaOrgHost}, DeniedDomains: []string{viaOrgHost}},
			Injection: []InjectionConfig{{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization"}}},
			MITMHosts: []string{viaOrgHost}, TrustedCAPEM: string(up.caPEM)}
		if _, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{Client: &http.Client{Transport: &fakeOrg{}},
			Dial: redirectDial(upstreamAddr(up.srv)), LookupIP: publicResolver{}.LookupIP}); err == nil {
			t.Fatal("an engine was built for a denied destination")
		}
		// And per request: the deny list is evaluated before the dial.
		f := newViaOrgFixture(t, nil, nil)
		denied := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{viaOrgHost}, DeniedDomains: []string{viaOrgHost}})
		f.engine.srv.proxy.policy, f.engine.srv.proxy.evaluator = denied, builtinEvaluator{p: denied}
		if w := f.inject(http.MethodGet, "/v1/x", nil); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		if hits, _, _ := f.up.seen(); hits != 0 {
			t.Fatalf("a denied host was dialed %d times", hits)
		}
	})
	t.Run("injection pin", func(t *testing.T) {
		f := newViaOrgFixture(t, nil, func(c *Config) { c.Injection[0].PinPath = "/v1/allowed" })
		f.inject(http.MethodGet, "/v1/other", map[string]string{"Authorization": "Bearer sandbox"})
		if hits, h, _ := f.up.seen(); hits != 1 || h.Get("Authorization") != "" {
			t.Fatalf("outside the pin: hits=%d auth=%q, want no credential at all", hits, h.Get("Authorization"))
		}
		f.inject(http.MethodGet, "/v1/allowed", nil)
		if _, h, _ := f.up.seen(); h.Get("Authorization") != viaOrgSecret {
			t.Fatalf("inside the pin: auth=%q", h.Get("Authorization"))
		}
	})
	t.Run("sandbox credentials stripped", func(t *testing.T) {
		f := newViaOrgFixture(t, nil, nil)
		f.inject(http.MethodGet, "/v1/x", map[string]string{"X-Api-Key": "sandbox-key", "Cookie": "s=1", "Authorization": "Bearer sandbox"})
		if _, h, _ := f.up.seen(); h.Get("X-Api-Key") != "" || h.Get("Cookie") != "" || h.Get("Authorization") != viaOrgSecret {
			t.Fatalf("upstream headers = %v", h)
		}
	})
	t.Run("IP guard", func(t *testing.T) {
		private := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.20.30.40")}, nil }
		f := newViaOrgFixture(t, private, nil)
		if w := f.inject(http.MethodGet, "/v1/x", nil); w.Code < 400 {
			t.Fatalf("status = %d, want a refusal", w.Code)
		}
		if hits, _, _ := f.up.seen(); hits != 0 {
			t.Fatalf("a private address was dialed %d times", hits)
		}
	})
	t.Run("ADO gate", func(t *testing.T) {
		f := newViaOrgFixture(t, nil, func(c *Config) {
			c.ADOGrant = &ADOGrantConfig{Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapCodeRead}, Hosts: []string{viaOrgHost}}
		})
		w := f.inject(http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", nil)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "CapabilityNotGrantedException") {
			t.Fatalf("status = %d %s, want the ADO gate's refusal", w.Code, w.Body)
		}
		if hits, _, _ := f.up.seen(); hits != 0 {
			t.Fatalf("the gate's refusal reached the upstream %d times", hits)
		}
	})
	t.Run("branch namespace", func(t *testing.T) {
		t.Setenv(envEnforceBranchNS, "1")
		f := newViaOrgFixture(t, nil, nil)
		body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
		if w := f.broker(t, "/wardyn/gh/octocat/hello-world.git/git-receive-pack", body); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d %s, want 403", w.Code, w.Body)
		}
		if hits, _, _ := f.up.seen(); hits != 0 || f.org.mints != 0 {
			t.Fatalf("an out-of-namespace push reached github %d times and minted %d", hits, f.org.mints)
		}
	})
	t.Run("push-content deny", func(t *testing.T) {
		f := newViaOrgFixture(t, nil, func(c *Config) {
			c.Policy.PushRules = &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}}
		})
		body := recordedPush(t, BranchNSPrefix(f.runID)+"work", map[string]string{".github/workflows/ci.yml": "on: push\n"})
		w := f.broker(t, "/wardyn/gh/octocat/hello-world/git-receive-pack", string(body))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), ".github/workflows/ci.yml") {
			t.Fatalf("status = %d %s, want the push rules' refusal", w.Code, w.Body)
		}
		if hits, _, _ := f.up.seen(); hits != 0 || f.org.mints != 0 {
			t.Fatalf("a denied push reached github %d times and minted %d", hits, f.org.mints)
		}
	})
	t.Run("approval hold", func(t *testing.T) {
		orig := gitApprovalPollInterval
		gitApprovalPollInterval = 5 * time.Millisecond
		t.Cleanup(func() { gitApprovalPollInterval = orig })
		t.Setenv(envGitApprovalTimeout, "200ms")
		f := newViaOrgFixture(t, nil, nil)
		f.org.gated = true
		w := httptest.NewRecorder()
		f.engine.ServeBroker(w, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
		if w.Code == http.StatusOK {
			t.Fatalf("an approval-gated grant was served before anyone approved it: %s", w.Body)
		}
		if hits, _, _ := f.up.seen(); hits != 0 || f.org.polls == 0 {
			t.Fatalf("hits=%d polls=%d, want the hold kept at the org and nothing dialed", hits, f.org.polls)
		}
	})
}

// TestViaOrg_NeverDialsCleartext: the organisation reaches a via_org destination over TLS only. A
// rule whose origin is cleartext is refused before any dial, and a destination that answers in
// cleartext never receives the request.
func TestViaOrg_NeverDialsCleartext(t *testing.T) {
	f := newViaOrgFixture(t, nil, func(c *Config) { c.MITMHosts = []string{"http://" + viaOrgHost} })
	if w := f.inject(http.MethodGet, "/v1/x", nil); w.Code != http.StatusForbidden {
		t.Fatalf("cleartext rule: status = %d, want 403", w.Code)
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("cleartext rule dialed %d times", hits)
	}

	var plainHits int
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits++ }))
	defer plain.Close()
	g := newViaOrgFixture(t, nil, nil)
	g.engine.srv.proxy.dial = redirectDial(upstreamAddr(plain))
	if w := g.inject(http.MethodGet, "/v1/x", nil); w.Code < 500 {
		t.Fatalf("a cleartext destination: status = %d, want a failed TLS dial", w.Code)
	}
	if plainHits != 0 {
		t.Fatalf("a cleartext server received %d requests", plainHits)
	}
}

// laptopFixture is a runner's proxy with one header route and one GitHub route, a control plane that
// records what is forwarded to it, and a local upstream for every other destination.
type laptopFixture struct {
	p             *Proxy
	sink          *strings.Builder
	cpSeen, lHits int
	cpReq         *http.Request
	cpAuthz       []string
	caPEM         []byte
	header, gh    uuid.UUID
}

func newLaptopFixture(t *testing.T) *laptopFixture {
	t.Helper()
	f := &laptopFixture{header: uuid.New(), gh: uuid.New(), sink: &strings.Builder{}}
	var lock sync.Mutex
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		f.cpSeen++
		f.cpReq = r.Clone(context.Background())
		f.cpAuthz = r.Header.Values(ViaOrgAuthorizationHeader)
		lock.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "from-org")
	}))
	t.Cleanup(cp.Close)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lock.Lock()
		f.lHits++
		lock.Unlock()
		_, _ = io.WriteString(w, "from-laptop")
	}))
	t.Cleanup(local.Close)
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	f.caPEM = certPEM
	cpAddr := upstreamAddr(cp)
	sink := &decisionSink{out: &lockedBuilder{b: f.sink}, ch: make(chan egress.DecisionLog, 64)}
	f.p = newProxy(Options{
		RunID: uuid.New(), Sink: sink, CA: ca, Resolver: publicResolver{},
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"other.example"}, FirstUseApproval: types.FirstUseWaitForReview}),
		ControlPlaneURL: "http://" + cpAddr, RunToken: newTokenSource("RUNTOK"),
		GitGrants:    map[string]uuid.UUID{"acme/app": f.gh},
		ViaOrgRoutes: []ViaOrgRoute{{GrantID: f.header, Host: "api.corp.example"}, {GrantID: f.gh, Host: githubHost}},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == cpAddr {
				return (&net.Dialer{}).DialContext(ctx, network, cpAddr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, upstreamAddr(local))
		},
	})
	return f
}

type lockedBuilder struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// TestLaptopProxy_ViaOrgForwardsAndHoldsNothing: a runner's proxy forwards a via_org destination's
// requests — plain, terminated CONNECT and /wardyn/gh/ alike — to the organisation with the run token,
// raising no hold and writing no decision; another destination is dialed from the laptop.
func TestLaptopProxy_ViaOrgForwardsAndHoldsNothing(t *testing.T) {
	f := newLaptopFixture(t)
	srv := httptest.NewServer(f.p)
	defer srv.Close()
	proxyURL := srv.URL

	// A plain request to the via_org host (not allowlisted, first-use wait_for_review: a hold if
	// this proxy decided it).
	req, _ := http.NewRequest(http.MethodPost, "http://api.corp.example/v1/messages?stream=1", strings.NewReader(`{"q":1}`))
	req.Header.Set("Authorization", "Bearer sandbox-own")
	resp := proxyDo(t, proxyURL, req)
	if body := readAll(t, resp); resp.StatusCode != http.StatusOK || body != "from-org" {
		t.Fatalf("plain: %d %q, want the organisation's answer", resp.StatusCode, body)
	}
	if f.cpReq.URL.Path != ViaOrgPathPrefix+f.header.String()+"/v1/messages" || f.cpReq.URL.RawQuery != "stream=1" ||
		f.cpReq.Header.Get("Authorization") != "Bearer RUNTOK" || f.cpReq.Header.Get(ViaOrgAuthorityHeader) != "api.corp.example" ||
		len(f.cpAuthz) != 1 || f.cpAuthz[0] != "Bearer sandbox-own" || f.cpReq.Method != http.MethodPost {
		t.Fatalf("forwarded %s %s?%s headers %v", f.cpReq.Method, f.cpReq.URL.Path, f.cpReq.URL.RawQuery, f.cpReq.Header)
	}

	// A terminated CONNECT: each inner request is forwarded, its own Host reported.
	conn := viaOrgMITMConn(t, proxyURL, "api.corp.example", f.caPEM)
	inner, _ := http.NewRequest(http.MethodGet, "https://api.corp.example/v1/models", nil)
	if err := inner.Write(conn); err != nil {
		t.Fatal(err)
	}
	iresp, err := http.ReadResponse(bufio.NewReader(conn), inner)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, iresp); body != "from-org" || f.cpReq.URL.Path != ViaOrgPathPrefix+f.header.String()+"/v1/models" || f.cpReq.Method != http.MethodGet {
		t.Fatalf("CONNECT: %q forwarded to %s %s", body, f.cpReq.Method, f.cpReq.URL.Path)
	}
	_ = conn.Close()

	// /wardyn/gh/ for a repository whose grant is via_org.
	gr := httptest.NewRequest(http.MethodGet, "/wardyn/gh/acme/app.git/info/refs?service=git-upload-pack", nil)
	gw := httptest.NewRecorder()
	f.p.ServeHTTP(gw, gr)
	if gw.Body.String() != "from-org" || f.cpReq.URL.Path != ViaOrgPathPrefix+f.gh.String()+"/wardyn/gh/acme/app.git/info/refs" {
		t.Fatalf("git broker: %q forwarded to %s", gw.Body, f.cpReq.URL.Path)
	}
	if f.cpSeen != 3 || f.lHits != 0 {
		t.Fatalf("org saw %d, laptop dialed %d, want 3 and 0", f.cpSeen, f.lHits)
	}
	_ = f.p.sink.close(context.Background())
	if f.sink.Len() != 0 {
		t.Fatalf("the laptop wrote decisions for via_org destinations: %s", f.sink.String())
	}

	// Another destination is this proxy's own: dialed from the laptop, with its decision.
	g := newLaptopFixture(t)
	other, _ := http.NewRequest(http.MethodGet, "http://other.example/", nil)
	if body := readAll(t, proxyDoHandler(t, g.p, other)); body != "from-laptop" || g.lHits != 1 || g.cpSeen != 0 {
		t.Fatalf("other destination: %q laptop=%d org=%d", body, g.lHits, g.cpSeen)
	}
	_ = g.p.sink.close(context.Background())
	if !strings.Contains(g.sink.String(), `"decision":"allow"`) {
		t.Fatalf("no laptop decision for its own destination: %s", g.sink.String())
	}
}

func proxyDo(t *testing.T, proxyURL string, req *http.Request) *http.Response {
	t.Helper()
	pu, _ := req.URL.Parse(proxyURL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func proxyDoHandler(t *testing.T, p *Proxy, req *http.Request) *http.Response {
	t.Helper()
	srv := httptest.NewServer(p)
	defer srv.Close()
	return proxyDo(t, srv.URL, req)
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// viaOrgMITMConn is agentMITMConn for any host.
func viaOrgMITMConn(t *testing.T, proxyURL, host string, caPEM []byte) *tls.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tc := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return tc
}

// TestViaOrg_EnginesDoNotShareASecretCorpus: each engine scans and masks with its own run's values.
// Engine A (run 1) resolves its credential and closes; engine B (run 2), inspecting forwarded bodies,
// must not refuse run 1's value as a secret of its own, and still refuses its own.
func TestViaOrg_EnginesDoNotShareASecretCorpus(t *testing.T) {
	procMask([]byte("SIDECAR-SHARED-ENTRY-0b3d")) // the shared registry: no engine may clear it
	holds := func(key uuid.UUID, v string) bool {
		return slices.ContainsFunc(procRegistry.Snapshot(key), func(b []byte) bool { return strings.Contains(string(b), v) })
	}
	a := newViaOrgFixtureWith(t, &fakeOrg{value: "Bearer RUN-ONE-ONLY-7d41c9"}, nil, nil)
	aKey := a.engine.srv.maskKey
	if aKey == uuid.Nil || !holds(aKey, "RUN-ONE-ONLY-7d41c9") || holds(uuid.Nil, "RUN-ONE-ONLY-7d41c9") {
		t.Fatalf("engine A's credential is not filed under its own key %s", aKey)
	}
	if err := a.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "engine A's key is evicted", func() bool { return !holds(aKey, "RUN-ONE-ONLY-7d41c9") })
	if !holds(uuid.Nil, "SIDECAR-SHARED-ENTRY-0b3d") {
		t.Fatal("closing engine A cleared the shared registry")
	}
	inspect := func(c *Config) {
		c.Policy.LLMInspection = &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, InspectForwardEgress: true}
	}
	b := newViaOrgFixtureWith(t, &fakeOrg{value: "Bearer RUN-TWO-ONLY-5e20aa"}, nil, inspect)
	send := func(body string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(body))
		r.Host = viaOrgHost
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		b.engine.ServeInjected(w, r, viaOrgHost)
		return w.Code
	}
	if code := send(`{"note":"RUN-TWO-ONLY-5e20aa"}`); code != http.StatusForbidden {
		t.Fatalf("engine B forwarded its own credential in a body: %d (the scan is not on)", code)
	}
	if code := send(`{"note":"RUN-ONE-ONLY-7d41c9"}`); code != http.StatusOK {
		t.Fatalf("engine B refused run 1's value: %d — its corpus carries another run's secrets", code)
	}
	if holds(b.engine.srv.maskKey, "RUN-ONE-ONLY") {
		t.Fatal("engine B's corpus holds run 1's value")
	}
	// Close and rebuild run 2's engine: the old key is released, the new engine has its own.
	bKey := b.engine.srv.maskKey
	if err := b.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := newViaOrgFixtureWith(t, &fakeOrg{value: "Bearer RUN-TWO-ONLY-5e20aa"}, nil, inspect)
	eventually(t, "engine B's key is evicted", func() bool { return !holds(bKey, "RUN-TWO-ONLY") })
	if c.engine.srv.maskKey == bKey || !holds(c.engine.srv.maskKey, "RUN-TWO-ONLY-5e20aa") {
		t.Fatal("the rebuilt engine did not start from its own corpus")
	}
	if !holds(uuid.Nil, "SIDECAR-SHARED-ENTRY-0b3d") {
		t.Fatal("an engine close cleared the shared registry")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heldAnywhere reports whether any registry key holds v.
func heldAnywhere(v string) bool {
	for _, key := range procRegistry.RunIDs() {
		for _, b := range procRegistry.Snapshot(key) {
			if strings.Contains(string(b), v) {
				return true
			}
		}
	}
	return false
}

// A build that fails after a first grant registered its credential leaves no key behind, however
// often it is retried, and never touches another engine's key.
func TestViaOrg_FailedBuildLeavesNoKey(t *testing.T) {
	live := newViaOrgFixtureWith(t, &fakeOrg{value: "Bearer LIVE-ENGINE-VALUE-91af"}, nil, nil)
	liveKey := live.engine.srv.maskKey
	second := uuid.New()
	build := func() error {
		up := newOrgUpstream(t)
		cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
			Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgHost, "second.example.com"}},
			Injection: []InjectionConfig{
				{GrantID: uuid.New(), InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization"}},
				{GrantID: second, InjectionRule: egress.InjectionRule{Host: "second.example.com", Header: "Authorization"}},
			},
			MITMHosts: []string{viaOrgHost, "second.example.com"}, TrustedCAPEM: string(up.caPEM)}
		_, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{
			Client: &http.Client{Transport: &fakeOrg{value: "Bearer FIRST-RULE-VALUE-2c5e", failGrant: second}},
			Dial:   redirectDial(upstreamAddr(up.srv)), LookupIP: publicResolver{}.LookupIP,
		})
		return err
	}
	keys := len(procRegistry.RunIDs())
	for i := range 5 {
		if err := build(); err == nil {
			t.Fatalf("build %d succeeded with a failing second grant", i)
		}
		if heldAnywhere("FIRST-RULE-VALUE-2c5e") {
			t.Fatalf("build %d left the first grant's credential in the registry", i)
		}
	}
	if n := len(procRegistry.RunIDs()); n != keys {
		t.Fatalf("five failed builds changed the registry's keys from %d to %d", keys, n)
	}
	if !slices.ContainsFunc(procRegistry.Snapshot(liveKey), func(b []byte) bool { return strings.Contains(string(b), "LIVE-ENGINE-VALUE-91af") }) {
		t.Fatal("a failed build evicted another engine's key")
	}
}

// A close that gives up waiting for the sink keeps the engine's masking corpus until the sink has
// posted every queued row: no queued row is posted without the key that masks it.
func TestViaOrg_KeyOutlivesQueuedDecisionRows(t *testing.T) {
	org := &fakeOrg{value: "Bearer QUEUED-ROWS-VALUE-6b0d"}
	f := newViaOrgFixtureWith(t, org, nil, nil)
	key := f.engine.srv.maskKey
	org.block = make(chan struct{})
	if w := f.inject(http.MethodGet, "/v1/x", nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.engine.Close(ctx); err == nil {
		t.Fatal("the close did not give up on its blocked sink")
	}
	time.Sleep(50 * time.Millisecond)
	if !slices.ContainsFunc(procRegistry.Snapshot(key), func(b []byte) bool { return strings.Contains(string(b), "QUEUED-ROWS-VALUE-6b0d") }) {
		t.Fatal("the key was evicted while decision rows were still queued")
	}
	close(org.block)
	eventually(t, "the key is evicted once the queue has posted", func() bool { return len(procRegistry.Snapshot(key)) == 0 })
}

// A closed engine serves nothing: its sink and masking corpus are gone.
func TestViaOrg_ClosedEngineServesNothing(t *testing.T) {
	f := newViaOrgFixture(t, nil, nil)
	if err := f.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.inject(http.MethodGet, "/v1/x", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a closed engine answered %d", w.Code)
	}
	w := httptest.NewRecorder()
	f.engine.ServeBroker(w, mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))
	if hits, _, _ := f.up.seen(); w.Code != http.StatusServiceUnavailable || hits != 0 || f.org.mints != 0 {
		t.Fatalf("a closed engine's broker answered %d, dialed %d, minted %d", w.Code, hits, f.org.mints)
	}
}

// A forwarded broker request reaches the engine through the control plane's
// listener, which sets no read timeout: its body is read under the same bound
// a terminated tunnel's is, so a body that stops arriving ends the request.
func TestViaOrg_BrokerBodyReadIsBounded(t *testing.T) {
	prev := mitmReadTimeout
	mitmReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mitmReadTimeout = prev })
	f := newViaOrgFixture(t, nil, nil)
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		f.engine.ServeBroker(w, r)
	}))
	defer srv.Close()
	body, stalled := io.Pipe()
	defer stalled.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/wardyn/gh/octocat/hello-world/git-receive-pack", body)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if resp, err := srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	if _, err := stalled.Write([]byte("00")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("a broker request whose body stopped arriving was still being read")
	}
	if hits, _, _ := f.up.seen(); hits != 0 {
		t.Fatalf("a push whose body never finished reached github %d times", hits)
	}
}

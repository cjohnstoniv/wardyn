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
	gated                            bool // the GitHub grant needs an approval nobody gives
	decisions                        strings.Builder
}

func (f *fakeOrg) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.mu.Lock()
	defer f.mu.Unlock()
	p := req.URL.Path
	switch {
	case strings.HasPrefix(p, "/api/v1/internal/injection/"):
		f.injections++
		_ = json.NewEncoder(rec).Encode(types.ResolvedInjection{Host: viaOrgHost, Header: "Authorization", Value: viaOrgSecret, JTI: uuid.NewString()})
	case p == "/api/v1/internal/credentials/mint" && f.gated:
		f.mints++
		rec.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(rec, `{"approval_id":"`+uuid.NewString()+`"}`)
	case p == "/api/v1/internal/credentials/mint":
		f.mints++
		_, _ = io.WriteString(rec, `{"kind":"github_token","token":"gh-inst-token","jti":"j","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
	case p == "/api/v1/internal/decisions":
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
	up, org := newOrgUpstream(t), &fakeOrg{}
	cfg := &Config{
		RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK",
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
		if d := f.decisions(t); !strings.Contains(d, `"decision":"allow"`) {
			t.Errorf("no org-side allow row: %s", d)
		}
	})
	t.Run("deny list", func(t *testing.T) {
		// A grant destination the run's policy denies: no engine serves it, so nothing is dialed.
		up := newOrgUpstream(t)
		cfg := &Config{RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK",
			Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgHost}, DeniedDomains: []string{viaOrgHost}},
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

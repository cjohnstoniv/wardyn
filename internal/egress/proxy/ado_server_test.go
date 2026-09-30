// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Azure DevOps Server with the person's own token (#1430): the run's grant
// covers a Server host, the control plane resolves the token the person pasted
// as Authorization: Basic base64(":"+PAT), and git reaches the Server through
// the same /wardyn/git/ door as Azure DevOps Services — with no git_pat grant
// in this proxy at all (the PAT broker off), so the proxy's injector is the
// only thing that ever holds the token. REST to a Server host is refused: the
// capability catalogue classifies no Server route.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	adoServerHost = "ado.corp.example"
	adoServerOrg  = "tfs/DefaultCollection"
	adoServerRepo = "https://" + adoServerHost + "/tfs/DefaultCollection/proj/_git/app"
	adoServerJTI  = "own-pat"
)

// ownPATCP answers every injection resolve the way wardynd resolves an own_pat
// grant: the person's one token, the same value whatever stale_jti says.
type ownPATCP struct {
	srv *httptest.Server
	pat string

	mu      sync.Mutex
	queries []string // every resolve's stale_jti, "" for none
}

func newOwnPATCP(t *testing.T, pat string) *ownPATCP {
	t.Helper()
	cp := &ownPATCP{pat: pat}
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.queries = append(cp.queries, r.URL.Query().Get("stale_jti"))
		cp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cp.resolved())
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *ownPATCP) resolved() types.ResolvedInjection {
	return types.ResolvedInjection{
		Header: "Authorization", Value: adoBasicPAT(cp.pat), JTI: adoServerJTI,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
	}
}

func (cp *ownPATCP) staleQueries() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var out []string
	for _, q := range cp.queries {
		if q != "" {
			out = append(out, q)
		}
	}
	return out
}

// stripTFS stands in for a Server's /tfs virtual directory: adofake serves
// /{org}/{project}/_git/{repo}, so the collection plays the organisation.
func stripTFS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/tfs")
		r.URL.RawPath = ""
		next.ServeHTTP(w, r)
	})
}

type adoServerHarness struct {
	*adoGitHarness
	front *adoFront
	cp    *ownPATCP
}

// newADOServerHarness is the git harness with the run's grant on one Server
// host, pinned to tfs/DefaultCollection, and no Azure DevOps Services host.
func newADOServerHarness(t *testing.T, caps ...adoscope.Capability) *adoServerHarness {
	t.Helper()
	front := &adoFront{}
	var cp *ownPATCP
	h := newADOGitHarnessFront(t, func(p *Proxy, pat string) {
		cp = newOwnPATCP(t, pat)
		e := &injEntry{grantID: uuid.New(), requireTLS: true}
		e.install(cp.resolved(), time.Now()) // what the boot resolve installs
		inj := p.inject
		inj.byHost = map[string]*injEntry{adoServerHost: e}
		inj.base, inj.token, inj.client = cp.srv.URL, newTokenSource("RUNTOK"), cp.srv.Client()
		p.adoGrants = adoGrantsByHost{adoServerHost: {Organization: adoServerOrg, Capabilities: caps}}
	}, func(next http.Handler) http.Handler { return front.wrap(stripTFS(next)) }, caps...)
	h.brokerHosts(t, adoServerHost, "other.corp.example")
	return &adoServerHarness{adoGitHarness: h, front: front, cp: cp}
}

// brokerHosts adds hosts to the git rewrite through the real agent-run-lib.sh,
// as a WARDYN_GIT_PAT_BROKER_HOSTS naming them would.
func (h *adoGitHarness) brokerHosts(t *testing.T, hosts ...string) {
	t.Helper()
	_, self, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(self), "..", "..", "..", "deploy", "images", "common", "agent-run-lib.sh")
	cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; configure_git_pat_broker_insteadof`, "bash", lib)
	cmd.Env = append(h.env(), "WARDYN_PROXY_URL="+h.proxy+"/", "WARDYN_GIT_PAT_BROKER_HOSTS="+strings.Join(hosts, " "))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure_git_pat_broker_insteadof: %v\n%s", err, out)
	}
}

// sandboxHoldsNoPAT fails if any file under the sandbox's home or work tree
// carries the PAT in either rendering.
func (h *adoServerHarness) sandboxHoldsNoPAT(t *testing.T) {
	t.Helper()
	wire := base64.StdEncoding.EncodeToString([]byte(":" + h.bearer))
	for _, root := range []string{h.home, h.work} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, rerr := os.ReadFile(path)
			if rerr == nil && (strings.Contains(string(b), h.bearer) || strings.Contains(string(b), wire)) {
				t.Errorf("the sandbox holds the PAT in %s", path)
			}
			return nil
		})
	}
}

// noPATInLogs asserts neither rendering of the PAT is in the decision log or slog.
func (h *adoServerHarness) noPATInLogs(t *testing.T) string {
	t.Helper()
	log := h.finish(t)
	wire := base64.StdEncoding.EncodeToString([]byte(":" + h.bearer))
	for name, s := range map[string]string{"decision log": log, "slog": h.logs.String()} {
		if strings.Contains(s, wire) {
			t.Errorf("%s carries the base64 PAT:\n%s", name, s)
		}
	}
	return log
}

// A clone from the Server carries the person's own token as Basic and nothing
// the sandbox put on the request; the PAT is masked in every rendering; and
// with no git_pat grant in the proxy (the PAT broker off) the token is in no
// file the sandbox can read, no git output and no log line.
func TestADOServerGit_OwnPATCloneIsInjectedAndNeverInTheSandbox(t *testing.T) {
	h := newADOServerHarness(t, adoscope.CapCodeRead)
	if len(h.p.patGrants) != 0 {
		t.Fatal("precondition: this proxy brokers no git_pat grant (the PAT broker off)")
	}
	dir := h.cloneWithSandboxBasic(t, adoServerRepo)
	assertPATMasked(t, h.bearer)

	want := adoBasicPAT(h.bearer)
	reqs := h.fake.Requests()
	if len(reqs) < 2 {
		t.Fatalf("the Server saw %d requests, want the advertisement and the upload-pack", len(reqs))
	}
	for _, r := range reqs {
		if r.Headers.Get("Authorization") != want || r.Token != h.bearer || !r.Authorized ||
			r.Headers.Get("Cookie") != "" || r.Headers.Get("Private-Token") != "" {
			t.Errorf("%s %s reached the Server with Authorization %q, Cookie %q, Private-Token %q; want the person's Basic PAT only",
				r.Method, r.Path, r.Headers.Get("Authorization"), r.Headers.Get("Cookie"), r.Headers.Get("Private-Token"))
		}
	}
	for _, q := range h.front.requests("") {
		if !strings.HasPrefix(q.path, "/tfs/DefaultCollection/proj/_git/app/") {
			t.Errorf("the Server saw %s, want the collection's own repository path", q.path)
		}
	}
	if out, err := h.git(t, "-C", dir, "remote", "-v"); err != nil || strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(":"+h.bearer))) {
		t.Errorf("git remote -v: %v\n%s", err, out)
	}
	h.sandboxHoldsNoPAT(t)
	if log := h.noPATInLogs(t); !strings.Contains(log, `"`+ruleSourceADOGit+`"`) {
		t.Errorf("no %s allow row:\n%s", ruleSourceADOGit, log)
	}
}

// The pin is the whole collection path: another collection, the collection
// left out, or another Server host is refused before anything is sent.
func TestADOServerGit_AnotherCollectionOrHostIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, url, want string }{
		{"another collection", "https://" + adoServerHost + "/tfs/OtherCollection/proj/_git/app",
			`granted the "tfs/DefaultCollection" Azure DevOps organisation only`},
		{"the collection left out", "https://" + adoServerHost + "/tfs/proj/_git/app",
			`granted the "tfs/DefaultCollection" Azure DevOps organisation only`},
		{"the virtual directory left out", "https://" + adoServerHost + "/DefaultCollection/proj/_git/app",
			`granted the "tfs/DefaultCollection" Azure DevOps organisation only`},
		{"another host", "https://other.corp.example/tfs/DefaultCollection/proj/_git/app", "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newADOServerHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
			out, err := h.git(t, "clone", tc.url, "app")
			if err == nil || !strings.Contains(out, tc.want) {
				t.Errorf("clone was not refused with %q (err %v):\n%s", tc.want, err, out)
			}
			if n := len(h.front.requests("")); n != 0 {
				t.Errorf("the Server saw %d requests, want 0", n)
			}
			h.noPATInLogs(t)
		})
	}
}

// Which Server paths the git door forwards: a smart-HTTP endpoint of
// <collection>/<project>/_git/<repository> or <collection>/_git/<repository>.
// Everything else fails closed.
func TestADOServerGitPath(t *testing.T) {
	for _, tc := range []struct {
		path, collection string
		ok               bool
	}{
		{"/tfs/DefaultCollection/proj/_git/app/info/refs", adoServerOrg, true},
		{"/tfs/DefaultCollection/proj/_git/app/git-upload-pack", adoServerOrg, true},
		{"/tfs/DefaultCollection/proj/_git/app/git-receive-pack", adoServerOrg, true},
		{"/tfs/DefaultCollection/_git/proj/info/refs", adoServerOrg, true},
		{"/TFS/defaultcollection/Proj/_git/App/info/refs", adoServerOrg, true},
		{"/DefaultCollection/proj/_git/app/info/refs", "DefaultCollection", true},
		{"/DefaultCollection/proj/_git/app/info/refs", "/DefaultCollection/", true},

		{"/tfs/DefaultCollection/proj/app/info/refs", adoServerOrg, false},
		{"/tfs/DefaultCollection/proj/sub/_git/app/info/refs", adoServerOrg, false},
		{"/tfs/DefaultCollection/proj/_git/app/extra/info/refs", adoServerOrg, false},
		{"/tfs/DefaultCollection/proj/_git/info/refs", adoServerOrg, false},
		{"/tfs/DefaultCollection/proj/_git/app/HEAD", adoServerOrg, false},
		{"/tfs/DefaultCollection/proj/_git/app/objects/info/packs", adoServerOrg, false},
		{"/tfs/Other/proj/_git/app/info/refs", adoServerOrg, false},
		{"/tfs/proj/_git/app/info/refs", adoServerOrg, false},
		{"/a/b/c/proj/_git/app/info/refs", "a/b/c", false},
		{"/proj/_git/app/info/refs", "", false},
		{"/tfs/DefaultCollection/proj/_git/app/info/refs", "tfs//DefaultCollection", false},
	} {
		req := httptest.NewRequest(http.MethodGet, routePATBroker+adoServerHost+tc.path, nil)
		keys, ok := adoGitKeys(req)
		if !ok {
			t.Fatalf("adoGitKeys(%s) refused", tc.path)
		}
		if got := adoServerGitPath(keys, tc.collection); got != tc.ok {
			t.Errorf("adoServerGitPath(%s, %q) = %v, want %v", tc.path, tc.collection, got, tc.ok)
		}
	}
}

// A path inside the collection that is no repository's smart-HTTP endpoint is
// refused at the door and never sent under the person's token.
func TestADOServerGit_UnclassifiablePathIsRefused(t *testing.T) {
	h := newADOServerHarness(t, adoscope.CapCodeRead)
	resp, err := http.Get(h.proxy + routePATBroker + adoServerHost + "/tfs/DefaultCollection/proj/app/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "reaches git only at tfs/DefaultCollection/<project>/_git/<repository>") {
		t.Errorf("status %d body %q, want the Server path refusal", resp.StatusCode, body)
	}
	if n := len(h.front.requests("")); n != 0 {
		t.Errorf("the Server saw %d requests, want 0", n)
	}
	if log := h.noPATInLogs(t); !strings.Contains(log, `"`+ruleSourceADOGitDenied+`"`) {
		t.Errorf("no %s row:\n%s", ruleSourceADOGitDenied, log)
	}
}

// A push to the Server is held to the run's capabilities and its own branch,
// exactly as on Azure DevOps Services, and a granted one carries the PAT.
func TestADOServerGit_PushIsHeldToTheRunsCapabilities(t *testing.T) {
	ro := newADOServerHarness(t, adoscope.CapCodeRead)
	dir := ro.clone(t, adoServerRepo)
	out, err := ro.push(t, dir, ro.runBranch())
	mustBeGitRefusal(t, out, err, "this run was not granted it")
	if n := ro.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the refused pack reached the Server %d times", n)
	}
	ro.noPATInLogs(t)

	rw := newADOServerHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
	dir = rw.clone(t, adoServerRepo)
	out, err = rw.push(t, dir, "main")
	mustBeGitRefusal(t, out, err, "this run may push only to its own branch")
	if n := rw.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the off-branch pack reached the Server %d times", n)
	}
	if out, err := rw.push(t, dir, rw.runBranch()); err != nil {
		t.Fatalf("push on the run's branch with code_write: %v\n%s", err, out)
	}
	if n := rw.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 1 {
		t.Errorf("the Server saw %d pack uploads, want 1", n)
	}
	for _, r := range rw.fake.Requests() {
		if r.Headers.Get("Authorization") != adoBasicPAT(rw.bearer) {
			t.Errorf("%s %s carried Authorization %q, want the person's Basic PAT", r.Method, r.Path, r.Headers.Get("Authorization"))
		}
	}
	rw.noPATInLogs(t)
}

// The Server refusing the person's own token: the proxy drops the header and
// re-resolves once, the control plane has nothing newer (an own_pat resolve is
// the same token), so the request is NOT retried — git gets the refusal, the
// Server saw one request, and no reresolved row is written.
func TestADOServerGit_RefusedOwnPATIsNotRetried(t *testing.T) {
	h := newADOServerHarness(t, adoscope.CapCodeRead)
	h.front.setRefuse(refuseToken(h.bearer, http.StatusUnauthorized))
	out, err := h.git(t, "clone", adoServerRepo, "app")
	mustBeGitRefusal(t, out, err, "refused this run's Azure DevOps sign-in (HTTP 401)")
	if got := h.front.requests("/info/refs"); len(got) != 1 {
		t.Errorf("the Server saw %d advertisement requests, want 1 (no retry): %v", len(got), tokensOf(got))
	}
	if got := h.cp.staleQueries(); len(got) != 1 || got[0] != adoServerJTI {
		t.Errorf("stale re-resolves = %v, want one naming %q", got, adoServerJTI)
	}

	// The Server takes the token again: the re-installed header is the same PAT.
	h.front.setRefuse(nil)
	h.clone(t, adoServerRepo)
	for _, q := range h.front.requests("") {
		if q.token != h.bearer {
			t.Errorf("%s carried %q, want the person's PAT", q.path, q.token)
		}
	}
	if log := h.noPATInLogs(t); strings.Contains(log, logGitReresolved) {
		t.Errorf("a %s row was written for a request that was not retried:\n%s", logGitReresolved, log)
	}
}

// newADOServerRESTProxy is a proxy whose Azure DevOps grant covers the Server
// host, with the person's PAT injectable there, allowing allowed and dialling
// every host at upstream.
func newADOServerRESTProxy(t *testing.T, upstream, pat string, allowed ...string) (*Proxy, func() string) {
	t.Helper()
	inj := &injector{byHost: map[string]*injEntry{adoServerHost: {
		grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: adoBasicPAT(pat), jti: adoServerJTI},
	}}}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: allowed}),
		Injector:  inj,
		Sink:      &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:  publicResolver{},
		Dial:      redirectDial(upstream),
		RunToken:  newTokenSource("RUNTOK"),
		ADOGrants: adoGrantsByHost{adoServerHost: {Organization: adoServerOrg, Capabilities: adoscope.GrantableCapabilities()}},
	})
	return p, func() string {
		_ = p.sink.close(context.Background())
		return buf.String()
	}
}

// REST to a Server host is never forwarded, whatever the run holds: on a
// terminated connection the gate can't classify a Server route and refuses; on
// the plain lane every request is refused; and a CONNECT this proxy won't
// terminate is refused rather than tunnelled past the gate.
func TestADOServerREST_IsRefused(t *testing.T) {
	pat := "pat-" + uuid.NewString()
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(pat, allADOScopes...)
	upstream := strings.TrimPrefix(fake.URL(), "http://")

	t.Run("terminated", func(t *testing.T) {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/tfs/DefaultCollection/_apis/projects?api-version=7.1", ""},
			{http.MethodGet, "/tfs/DefaultCollection/proj/_apis/git/repositories?api-version=7.1", ""},
			{http.MethodOptions, "/tfs/DefaultCollection/_apis", ""},
			{http.MethodPost, "/tfs/DefaultCollection/proj/_apis/git/repositories/app/pushes?api-version=7.1", `{"refUpdates":[]}`},
			{http.MethodGet, "/tfs/DefaultCollection/proj/_git/app/info/refs?service=git-upload-pack", ""},
		} {
			p, log := newADOServerRESTProxy(t, upstream, pat)
			p.mitmHosts = map[string]bool{adoServerHost: true}
			p.mitmPorts = map[string]int{adoServerHost: 443}
			p.mitmPlaintext = map[string]bool{plaintextKey(adoServerHost, 443): true}
			raw := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: " + adoServerHost + "\r\n"
			if tc.body != "" {
				raw += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(tc.body)) + "\r\n"
			}
			req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, req, adoServerHost, 443)
			if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), pat) {
				t.Errorf("%s %s: status %d body %s, want a 403 refusal", tc.method, tc.path, rec.Code, rec.Body.String())
			}
			if l := log(); !strings.Contains(l, `"`+ruleSourceADODenied+`"`) || strings.Contains(l, pat) {
				t.Errorf("%s %s: decision log is not a clean %s: %s", tc.method, tc.path, ruleSourceADODenied, l)
			}
		}
		if n := len(fake.Requests()); n != 0 {
			t.Errorf("the Server saw %d REST requests, want 0: %+v", n, fake.Requests())
		}
	})

	t.Run("plain lane", func(t *testing.T) {
		p, log := newADOServerRESTProxy(t, upstream, pat)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+adoServerHost+"/tfs/DefaultCollection/_apis/projects", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("status %d body %s, want 403", rec.Code, rec.Body.String())
		}
		if l := log(); !strings.Contains(l, `"`+ruleSourceADODenied+`"`) {
			t.Errorf("no %s row: %s", ruleSourceADODenied, l)
		}
		if n := len(fake.Requests()); n != 0 {
			t.Errorf("the Server saw %d requests, want 0", n)
		}
	})

	t.Run("connect not terminated", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		dialed := make(chan struct{}, 1)
		go func() {
			if c, err := ln.Accept(); err == nil {
				dialed <- struct{}{}
				_ = c.Close()
			}
		}()
		p, log := newADOServerRESTProxy(t, ln.Addr().String(), pat, adoServerHost+":443")
		srv := httptest.NewServer(p)
		t.Cleanup(srv.Close)
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "CONNECT "+adoServerHost+":443 HTTP/1.1\r\nHost: "+adoServerHost+":443\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("read CONNECT response: %v", err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("CONNECT status = %d, want 403", resp.StatusCode)
		}
		select {
		case <-dialed:
			t.Error("the proxy opened a tunnel to the Server")
		case <-time.After(100 * time.Millisecond):
		}
		srv.Close()
		if l := log(); !strings.Contains(l, `"`+ruleSourceADODenied+`"`) {
			t.Errorf("no %s row: %s", ruleSourceADODenied, l)
		}
	})
}

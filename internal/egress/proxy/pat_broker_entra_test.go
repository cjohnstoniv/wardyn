// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// adoGitBrokerHosts is what dispatch writes into WARDYN_GIT_PAT_BROKER_HOSTS
// for organisation "acme" (api.adoEntraGitHosts; pinned there too).
const adoGitBrokerHosts = "acme.visualstudio.com acme@dev.azure.com dev.azure.com"

// adoGitHarness is a real git client, rewritten by the real agent-run-lib.sh
// onto a real proxy's /wardyn/git/ broker, whose outbound leg reaches an
// adofake (git http-backend behind it) through a TLS front. The fake's token
// carries read AND write scope, as a real Entra token does (F-LIVE-1), so any
// refusal of a request the proxy forwards could only be the proxy's.
type adoGitHarness struct {
	p      *Proxy
	fake   *adofake.Server
	bearer string
	bare   string
	home   string
	work   string
	runID  uuid.UUID
	proxy  string // the proxy listener's URL, as WARDYN_PROXY_URL
	sink   *bytes.Buffer
	logs   *bytes.Buffer
}

func newADOGitHarness(t *testing.T, caps ...adoscope.Capability) *adoGitHarness {
	t.Helper()
	return newADOGitHarnessWith(t, nil, caps...)
}

// newADOGitHarnessWith runs setup against the proxy BEFORE its server starts
// serving, so anything setup writes happens-before every request — git reaches
// the proxy from a subprocess, which the race detector cannot order otherwise.
func newADOGitHarnessWith(t *testing.T, setup func(p *Proxy, token string), caps ...adoscope.Capability) *adoGitHarness {
	t.Helper()
	return newADOGitHarnessFront(t, setup, nil, caps...)
}

// newADOGitHarnessFront is newADOGitHarnessWith with front, when non-nil, wrapped around the TLS
// front's reverse proxy to the fake: it sees every request the proxy forwards.
func newADOGitHarnessFront(t *testing.T, setup func(p *Proxy, token string), front func(http.Handler) http.Handler, caps ...adoscope.Capability) *adoGitHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// Unique per harness, so the process-global mask registry another test
	// populated can never hide a missing registration here.
	token := "entra-bearer-" + uuid.NewString()
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(token, adofake.ScopeCodeRead, adofake.ScopeCodeWrite)
	bare := adofake.NewFixtureRepo(t)
	fake.RegisterRepo("acme", "proj", "app", bare)
	fake.RegisterRepo("DefaultCollection", "proj", "app", bare)
	fake.RegisterRepo("evil", "loot", "app", adofake.NewFixtureRepo(t))
	fu, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatal(err)
	}
	var toFake http.Handler = httputil.NewSingleHostReverseProxy(fu)
	if front != nil {
		toFake = front(toFake)
	}
	tlsFront := httptest.NewTLSServer(toFake)
	t.Cleanup(tlsFront.Close)

	logs := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	bearer := injectedHeader{name: "Authorization", value: "Bearer " + token}
	inj := &injector{byHost: map[string]*injEntry{
		"dev.azure.com":         {grantID: uuid.New(), header: bearer, requireTLS: true},
		"acme.visualstudio.com": {grantID: uuid.New(), header: bearer, requireTLS: true},
	}}
	grant := ADOGrant{Organization: "acme", Capabilities: caps}
	runID := uuid.New()
	sink := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           runID,
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Injector:        inj,
		Sink:            &decisionSink{out: sink, ch: make(chan egress.DecisionLog, 256)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(tlsFront)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ADOGrants:       adoGrantsByHost{"dev.azure.com": grant, "acme.visualstudio.com": grant},
	})
	if setup != nil {
		setup(p, token)
	}
	if inj.base == "" { // setup wired no control plane of its own
		echoControlPlane(t, inj)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	h := &adoGitHarness{p: p, fake: fake, bearer: token, bare: bare, home: t.TempDir(), work: t.TempDir(), runID: runID, proxy: srv.URL, sink: sink, logs: logs}
	// The REAL rewrite: agent-run-lib.sh's configure_git_pat_broker_insteadof,
	// fed exactly what dispatch writes.
	_, self, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(self), "..", "..", "..", "deploy", "images", "common", "agent-run-lib.sh")
	cmd := exec.Command("bash", "-c", `set -euo pipefail; source "$1"; configure_git_pat_broker_insteadof`, "bash", lib)
	cmd.Env = append(h.env(), "WARDYN_PROXY_URL="+srv.URL+"/", "WARDYN_GIT_PAT_BROKER_HOSTS="+adoGitBrokerHosts)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configure_git_pat_broker_insteadof: %v\n%s", err, out)
	}
	return h
}

func (h *adoGitHarness) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "GIT_"), strings.EqualFold(k, "http_proxy"), strings.EqualFold(k, "https_proxy"),
			strings.EqualFold(k, "all_proxy"), strings.EqualFold(k, "no_proxy"), k == "HOME", k == "SSH_ASKPASS":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+h.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

// git runs git in the harness's work dir and returns its combined output.
func (h *adoGitHarness) git(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = h.work
	cmd.Env = h.env()
	out, err := cmd.CombinedOutput()
	s := string(out)
	if strings.Contains(s, h.bearer) {
		t.Errorf("git output carries the bearer:\n%s", s)
	}
	return s, err
}

// clone clones url into work/app with a smuggled sandbox credential on every
// request, and returns the clone's directory.
func (h *adoGitHarness) clone(t *testing.T, cloneURL string) string {
	t.Helper()
	if out, err := h.git(t, "-c", "http.extraHeader=Authorization: Bearer SANDBOX-SMUGGLED",
		"-c", "http.extraHeader=Private-Token: SANDBOX-SMUGGLED", "clone", cloneURL, "app"); err != nil {
		t.Fatalf("git clone %s: %v\n%s", cloneURL, err, out)
	}
	return filepath.Join(h.work, "app")
}

// commit makes a new commit in dir on branch.
func (h *adoGitHarness) commit(t *testing.T, dir, branch string) {
	t.Helper()
	for _, args := range [][]string{
		{"-C", dir, "checkout", "-B", branch},
		{"-C", dir, "commit", "--allow-empty", "-m", "run work"},
	} {
		if out, err := h.git(t, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func (h *adoGitHarness) runBranch() string { return "wardyn/" + h.runID.String() + "/work" }

// finish flushes the decision sink and asserts the bearer is in no log line.
func (h *adoGitHarness) finish(t *testing.T) string {
	t.Helper()
	_ = h.p.sink.close(context.Background())
	for name, s := range map[string]string{"decision log": h.sink.String(), "slog": h.logs.String()} {
		if strings.Contains(s, h.bearer) {
			t.Errorf("%s carries the bearer:\n%s", name, s)
		}
	}
	return h.sink.String()
}

// mustBeGitRefusal asserts git printed the reason and not a credential prompt.
func mustBeGitRefusal(t *testing.T, out string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("git succeeded, want a refusal:\n%s", out)
	}
	if strings.Contains(out, "could not read Username") || strings.Contains(out, "Authentication failed") {
		t.Errorf("git read the refusal as a credential challenge:\n%s", out)
	}
	if !strings.Contains(out, want) {
		t.Errorf("git output does not carry the reason %q:\n%s", want, out)
	}
}

// A real clone through the broker carries the person's bearer, and nothing the
// sandbox put on the request; the Clone-button spelling (<org>@dev.azure.com)
// lands on the same broker path; and the bearer is mask-registered by the
// broker itself before anything can log it.
func TestADOGitBroker_CloneCarriesTheBearer(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	row := []byte(`{"rule_source":"brokered:ado-git","error":"` + h.bearer + `"}`)
	if !bytes.Contains(maskDecisionBytes(row), []byte(h.bearer)) {
		t.Fatal("precondition: the bearer is masked before the broker ever used it")
	}
	h.clone(t, "https://acme@dev.azure.com/acme/proj/_git/app")
	if bytes.Contains(maskDecisionBytes(row), []byte(h.bearer)) {
		t.Error("after a brokered clone the bearer is not in the mask registry")
	}

	reqs := h.fake.Requests()
	if len(reqs) < 2 {
		t.Fatalf("the fake saw %d requests, want the advertisement and the upload-pack", len(reqs))
	}
	for _, r := range reqs {
		if r.Token != h.bearer || !r.Authorized || r.Headers.Get("Private-Token") != "" {
			t.Errorf("%s %s reached Azure DevOps with token %q, Private-Token %q; want the brokered bearer only",
				r.Method, r.Path, r.Token, r.Headers.Get("Private-Token"))
		}
	}
	if log := h.finish(t); !strings.Contains(log, ruleSourceADOGit) {
		t.Errorf("no %s allow row:\n%s", ruleSourceADOGit, log)
	}
}

// The legacy <org>.visualstudio.com spelling routes through the same broker,
// pinned by its host label.
func TestADOGitBroker_LegacyHostClone(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	h.clone(t, "https://acme.visualstudio.com/DefaultCollection/proj/_git/app")
	h.finish(t)
}

// A push on the run's own branch succeeds with code_write.
func TestADOGitBroker_PushWithCodeWrite(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	h.commit(t, dir, h.runBranch())
	if out, err := h.git(t, "-C", dir, "push", "origin", h.runBranch()); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", h.bare, "rev-parse", "--verify", "refs/heads/"+h.runBranch()).CombinedOutput(); err != nil {
		t.Fatalf("the pushed branch is not in the repository: %v\n%s", err, out)
	}
	h.finish(t)
}

// WITHOUT code_write THE PUSH IS REFUSED ON THE PACK POST — the advertisement
// is a read and goes through — and git prints why, not a credential prompt.
func TestADOGitBroker_PushWithoutCodeWriteIsRefusedInGitsTerms(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	h.commit(t, dir, h.runBranch())
	out, err := h.git(t, "-C", dir, "push", "origin", h.runBranch())
	mustBeGitRefusal(t, out, err, "this run was not granted it")
	if !strings.Contains(out, "remote rejected") || !strings.Contains(out, string(adoscope.CapCodeWrite)) {
		t.Errorf("git did not report the rejected ref and the missing capability:\n%s", out)
	}

	advertised := false
	for _, r := range h.fake.Requests() {
		if r.Endpoint == adofake.EndpointGitReceivePack {
			t.Errorf("the refused pack reached Azure DevOps: %+v", r)
		}
		if r.Endpoint == adofake.EndpointGitAdvertise && strings.Contains(r.Query, "git-receive-pack") {
			advertised = true
		}
	}
	if !advertised {
		t.Error("the receive-pack advertisement was not forwarded under read (F-LIVE-8: it is a read)")
	}
	if log := h.finish(t); !strings.Contains(log, ruleSourceADOGitDenied) {
		t.Errorf("no %s row:\n%s", ruleSourceADOGitDenied, log)
	}
}

// A push outside the run's branch namespace is refused as the run's own-branch
// rule, whatever capabilities the run holds — policy_bypass included — and git
// is told where the run may push, not about a branch policy nobody consulted.
func TestADOGitBroker_NonRunRefPushIsRefusedWhateverTheCapabilities(t *testing.T) {
	h := newADOGitHarness(t, adoscope.GrantableCapabilities()...)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	h.commit(t, dir, "main")
	out, err := h.git(t, "-C", dir, "push", "origin", "main")
	mustBeGitRefusal(t, out, err, "this run may push only to its own branch (wardyn/"+h.runID.String()+"/…)")
	if !strings.Contains(out, "git_push_any_branch: true") || strings.Contains(strings.ToLower(out), "branch polic") {
		t.Errorf("refusal does not name the switch, or names a branch policy:\n%s", out)
	}
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the refused pack reached Azure DevOps %d times", n)
	}
	h.finish(t)
}

// With git_push_any_branch a push to main needs code_write only: it lands, and
// the forward is marked as the App lane marks an unconfined push. Without
// code_write it is still refused, as code_write.
func TestADOGitBroker_AnyBranchPushNeedsCodeWrite(t *testing.T) {
	anyBranch := func(p *Proxy, _ string) { p.policy = CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: true}) }
	h := newADOGitHarnessWith(t, anyBranch, adoscope.CapCodeRead, adoscope.CapCodeWrite)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	if out, err := h.push(t, dir, "main"); err != nil {
		t.Fatalf("any-branch push to main under code_write: %v\n%s", err, out)
	}
	local, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	remote, err := exec.Command("git", "-C", h.bare, "rev-parse", "refs/heads/main").Output()
	if err != nil || string(remote) != string(local) {
		t.Fatalf("main in the repository = %q (%v), want the pushed %q", remote, err, local)
	}
	if log := h.finish(t); !strings.Contains(log, `"`+ruleSourceGitNSOff+`"`) {
		t.Errorf("the any-branch push has no %s row:\n%s", ruleSourceGitNSOff, log)
	}

	h = newADOGitHarnessWith(t, anyBranch, adoscope.CapCodeRead)
	dir = h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err := h.push(t, dir, "main")
	mustBeGitRefusal(t, out, err, "("+string(adoscope.CapCodeWrite)+")")
	h.finish(t)
}

// Another organisation is refused before anything reaches Azure DevOps — the
// bearer carries no organisation claim, so the URL pin is the whole binding.
func TestADOGitBroker_AnotherOrganisationIsRefused(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
	out, err := h.git(t, "clone", "https://dev.azure.com/evil/loot/_git/app", "loot")
	mustBeGitRefusal(t, out, err, `granted the "acme" Azure DevOps organisation only`)
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("the fake saw %d requests for another organisation, want 0", n)
	}
	h.finish(t)
}

// AZURE DEVOPS' OWN 401 NEVER REACHES git AS ONE: relayed, git would prompt for
// a username and the person would never learn their sign-in was refused.
func TestADOGitBroker_UpstreamUnauthorizedIsNotACredentialPrompt(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	h.fake.RegisterToken(h.bearer) // the token now carries no scope at all
	out, err := h.git(t, "clone", "https://dev.azure.com/acme/proj/_git/app", "app")
	mustBeGitRefusal(t, out, err, "refused this run's Azure DevOps sign-in")
	h.finish(t)
}

// THE STORED-PAT LANE IS UNCHANGED beside an Entra grant: a host the Entra
// grant does not cover keeps Basic auth from the brokered PAT, byte for byte.
func TestADOGitBroker_StoredPATLaneUnchanged(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, _ := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
	p.adoGrants = adoGrantsByHost{"dev.azure.com": {Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapCodeRead}}}

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:T")); up.gitAuth != want {
		t.Fatalf("upstream Authorization = %q, want %q", up.gitAuth, want)
	}
}

// withHold points the harness's injector at a scripted control plane and
// approval reader, so a push beyond the grant is held instead of refused.
func (h *adoGitHarness) withHold(t *testing.T, cp *capControlPlane, reader approvalReader) {
	t.Helper()
	fastPolls(t, 5*time.Millisecond)
	tok := &tokenSource{}
	tok.Set("run-token")
	inj := h.p.inject
	inj.base, inj.token, inj.client = cp.srv.URL, tok, cp.srv.Client()
	inj.reauth, inj.approvals = newReauthCoordinator(), reader
	t.Cleanup(inj.reauth.stop)
	// An approval answers with this harness's own registered credential.
	cp.mu.Lock()
	cp.value = inj.byHost["dev.azure.com"].header.value
	cp.mu.Unlock()
}

// push commits on branch in dir and pushes it.
func (h *adoGitHarness) push(t *testing.T, dir, branch string) (string, error) {
	t.Helper()
	h.commit(t, dir, branch)
	return h.git(t, "-C", dir, "push", "origin", branch)
}

// countEndpoint is how many requests the fake answered for e whose query
// contains q.
func (h *adoGitHarness) countEndpoint(e adofake.Endpoint, q string) int {
	n := 0
	for _, r := range h.fake.Requests() {
		if r.Endpoint == e && strings.Contains(r.Query, q) {
			n++
		}
	}
	return n
}

// A PUSH BEYOND THE GRANT IS HELD, and one `once` approval covers the whole
// push: the advertisement is a read and asks nothing, the pack upload asks and
// spends it. The next push asks again.
func TestADOGitBroker_HeldPushApprovedOnceThenAsksAgain(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	if out, err := h.push(t, dir, h.runBranch()); err != nil {
		t.Fatalf("held push, approved once: %v\n%s", err, out)
	}
	if _, raised := cp.snapshot(); len(raised) != 1 {
		t.Fatalf("one push raised %d approvals, want 1 — the advertisement must not ask or spend", len(raised))
	}
	if a, p := h.countEndpoint(adofake.EndpointGitAdvertise, "git-receive-pack"), h.countEndpoint(adofake.EndpointGitReceivePack, ""); a != 1 || p != 1 {
		t.Fatalf("Azure DevOps saw %d receive-pack advertisements and %d pack uploads, want 1 and 1", a, p)
	}
	if asks, _ := cp.snapshot(); asks[0].Get("capability") != string(adoscope.CapCodeWrite) || asks[0].Get("ref_class") != "" || asks[0].Get("repo") != "app" {
		t.Errorf("the ask = %v, want code_write for repo app and no protected ref class", asks[0])
	}

	if out, err := h.push(t, dir, h.runBranch()); err != nil {
		t.Fatalf("second push: %v\n%s", err, out)
	}
	if _, raised := cp.snapshot(); len(raised) != 2 {
		t.Errorf("the second push raised %d approvals in total, want 2 — a once approval covers one push", len(raised))
	}
	h.finish(t)
}

// THE APPROVAL'S PAT REACHES THE PUSH: the run holds a read-only PAT, the approval
// mints a union PAT, and the one pack upload carries it — so one once approval is
// enough, not a failed 401 push that spends it.
func TestADOGitBroker_HeldPushGoesOutUnderTheApprovalsPAT(t *testing.T) {
	patB := "pat-b-" + uuid.NewString()
	// Azure DevOps refuses the pack upload of any PAT but the approval's.
	onlyB := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/git-receive-pack") && r.Method == http.MethodPost &&
				r.Header.Get("Authorization") != "Bearer "+patB {
				http.Error(w, "TF400813", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := newADOGitHarnessFront(t, nil, onlyB, adoscope.CapCodeRead)
	h.fake.RegisterToken(patB, adofake.ScopeCodeRead, adofake.ScopeCodeWrite)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	cp.mu.Lock()
	cp.value = "Bearer " + patB
	cp.mu.Unlock()
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	if out, err := h.push(t, dir, h.runBranch()); err != nil {
		t.Fatalf("held push, approved once: %v\n%s", err, out)
	}
	if _, raised := cp.snapshot(); len(raised) != 1 {
		t.Errorf("the push raised %d approvals, want 1", len(raised))
	}
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 1 {
		t.Errorf("Azure DevOps saw %d pack uploads, want 1", n)
	}
	h.finish(t)
}

// A denied push is refused in git's receive-pack terms and git exits non-zero.
func TestADOGitBroker_HeldPushDenied(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalDenied)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	out, err := h.push(t, dir, h.runBranch())
	mustBeGitRefusal(t, out, err, "it was not approved")
	if !strings.Contains(out, "remote rejected") {
		t.Errorf("git did not report the rejected ref:\n%s", out)
	}
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("a denied pack reached Azure DevOps %d times", n)
	}
	h.finish(t)
}

// Approved for the run: later pushes go through with no new approval.
func TestADOGitBroker_HeldPushApprovedForTheRun(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	cp := newCapControlPlane(t)
	cp.forRun = true
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalApproved)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	for i := 0; i < 3; i++ {
		if out, err := h.push(t, dir, h.runBranch()); err != nil {
			t.Fatalf("push %d: %v\n%s", i, err, out)
		}
	}
	if _, raised := cp.snapshot(); len(raised) != 1 {
		t.Errorf("three pushes raised %d approvals, want 1 — the run was not widened", len(raised))
	}
	h.finish(t)
}

// A ref outside the run's branch namespace is never an ask with the switch
// off; with it on, a run without code_write is asked for code_write, and the
// ask says the ref lies outside the run's own branch.
func TestADOGitBroker_HeldNonRunRef(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalDenied)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err := h.push(t, dir, "main")
	mustBeGitRefusal(t, out, err, "this run may push only to its own branch")
	if asks, _ := cp.snapshot(); len(asks) != 0 {
		t.Errorf("asks = %v, want none: the own-branch rule is not liftable", asks)
	}
	h.finish(t)

	anyBranch := func(p *Proxy, _ string) { p.policy = CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: true}) }
	h = newADOGitHarnessWith(t, anyBranch, adoscope.CapCodeRead)
	cp = newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalDenied)})
	dir = h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err = h.push(t, dir, "main")
	mustBeGitRefusal(t, out, err, "it was not approved")
	asks, _ := cp.snapshot()
	if len(asks) == 0 || asks[0].Get("capability") != string(adoscope.CapCodeWrite) || asks[0].Get("ref_class") != "outside_run_namespace" {
		t.Errorf("asks = %v, want code_write for a ref outside the run's own branch", asks)
	}
	h.finish(t)
}

// A pack larger than http.postBuffer makes git send an empty probe POST to
// git-receive-pack before the real one (remote-curl's probe_rpc). The probe
// moves no ref, so it neither asks nor spends: one approval still covers the
// push.
func TestADOGitBroker_HeldLargePushProbeDoesNotSpend(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	blob := make([]byte, 256<<10) // incompressible, so the pack exceeds the buffer
	_, _ = rand.Read(blob)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.git(t, "-C", dir, "add", "big.bin"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	h.commit(t, dir, h.runBranch())
	// The push itself ends in a 400 here, and that is the FAKE: net/http/cgi
	// refuses a chunked request body, and git streams a pack above
	// http.postBuffer chunked. What this pins is upstream of that — both POSTs
	// were forwarded, under one approval.
	_, _ = h.git(t, "-C", dir, "-c", "http.postBuffer=65536", "push", "origin", h.runBranch())
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 2 {
		t.Fatalf("Azure DevOps saw %d pack POSTs, want 2 (the probe and the pack) — the probe did not happen", n)
	}
	if _, raised := cp.snapshot(); len(raised) != 1 {
		t.Errorf("one large push raised %d approvals, want 1 — the probe spent the approval", len(raised))
	}
	h.finish(t)
}

// A NUL on a later command line is refused before anything reaches Azure
// DevOps, in receive-pack terms: "<old> <new> refs/heads/wardyn/<run>/x\0
// refs/heads/main" on line 2 must not pass as the run's own ref.
func TestADOGitBroker_NULOnALaterCommandIsRefused(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapCodeRead, adoscope.CapCodeWrite)
	run := BranchNSPrefix(h.runID)
	section := pkt(someOID+" "+otherOID+" "+run+"a"+firstCaps) +
		pkt(someOID+" "+otherOID+" "+run+"b\x00refs/heads/main\n") + "0000"
	req := mustLocalReq(t, http.MethodPost, "/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack",
		strings.NewReader(section+"PACK"))
	rec := httptest.NewRecorder()
	h.p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-git-receive-pack-result" {
		t.Fatalf("status %d type %q, want a receive-pack result", rec.Code, rec.Header().Get("Content-Type"))
	}
	if body := rec.Body.String(); !strings.Contains(body, "a NUL is allowed only on the first command") || !strings.Contains(body, "unpack refused by Wardyn") {
		t.Errorf("refusal body %q does not carry the reason and the unpack status", body)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("Azure DevOps saw %d requests, want 0", n)
	}
	h.finish(t)
}

// A CLONE needs code_read: a run holding only another area's read is held, and
// the ask names code_read.
func TestADOGitBroker_CloneWithoutCodeReadIsHeldAsCodeRead(t *testing.T) {
	cp := newCapControlPlane(t)
	cp.forRun = true
	fastPolls(t, 5*time.Millisecond)
	// The hold is wired before the server serves: the clone is the first
	// request, and git reaches the proxy from a subprocess the race detector
	// cannot order after a later write.
	h := newADOGitHarnessWith(t, func(p *Proxy, token string) {
		cp.value = "Bearer " + token
		tok := &tokenSource{}
		tok.Set("run-token")
		inj := p.inject
		inj.base, inj.token, inj.client = cp.srv.URL, tok, cp.srv.Client()
		inj.reauth, inj.approvals = newReauthCoordinator(), &fakeApprovalReader{steps: steps(types.ApprovalApproved)}
		t.Cleanup(inj.reauth.stop)
	}, adoscope.CapWorkRead)
	h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	if asks, raised := cp.snapshot(); len(raised) != 1 || asks[0].Get("capability") != string(adoscope.CapCodeRead) {
		t.Fatalf("control plane saw %v, want one ask for code_read", asks)
	}
	h.finish(t)
}

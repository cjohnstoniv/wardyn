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
	sink   *bytes.Buffer
	logs   *bytes.Buffer
}

func newADOGitHarness(t *testing.T, caps ...adoscope.Capability) *adoGitHarness {
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
	front := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(fu))
	t.Cleanup(front.Close)

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
		Dial:            redirectDial(upstreamAddr(front)),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ADOGrants:       adoGrantMap{"dev.azure.com": grant, "acme.visualstudio.com": grant},
	})
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)

	h := &adoGitHarness{p: p, fake: fake, bearer: token, bare: bare, home: t.TempDir(), work: t.TempDir(), runID: runID, sink: sink, logs: logs}
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

// A REAL CLONE THROUGH THE BROKER CARRIES THE PERSON'S BEARER, and nothing the
// sandbox put on the request; the Clone-button spelling (<org>@dev.azure.com)
// lands on the same broker path; and the bearer is mask-registered by the
// broker itself before anything can log it.
func TestADOGitBroker_CloneCarriesTheBearer(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
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
	h := newADOGitHarness(t, adoscope.CapRead)
	h.clone(t, "https://acme.visualstudio.com/DefaultCollection/proj/_git/app")
	h.finish(t)
}

// A PUSH ON THE RUN'S OWN BRANCH SUCCEEDS WITH code_write.
func TestADOGitBroker_PushWithCodeWrite(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
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
	h := newADOGitHarness(t, adoscope.CapRead)
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

// A PUSH OUTSIDE THE RUN'S BRANCH NAMESPACE MOVES A PROTECTED REF — every ref
// is protected until a grant says otherwise, as on the REST gate — and needs
// policy_bypass.
func TestADOGitBroker_ProtectedRefNeedsPolicyBypass(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	h.commit(t, dir, "main")
	out, err := h.git(t, "-C", dir, "push", "origin", "main")
	mustBeGitRefusal(t, out, err, string(adoscope.CapPolicyBypass))
	h.finish(t)

	h = newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPolicyBypass)
	dir = h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	h.commit(t, dir, "main")
	if out, err := h.git(t, "-C", dir, "push", "origin", "main"); err != nil {
		t.Fatalf("push to main with policy_bypass: %v\n%s", err, out)
	}
	h.finish(t)
}

// ANOTHER ORGANISATION IS REFUSED before anything reaches Azure DevOps — the
// bearer carries no organisation claim, so the URL pin is the whole binding.
func TestADOGitBroker_AnotherOrganisationIsRefused(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
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
	h := newADOGitHarness(t, adoscope.CapRead)
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
	p.adoGrants = adoGrantMap{"dev.azure.com": {Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapRead}}}

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
	h := newADOGitHarness(t, adoscope.CapRead)
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

// A DENIED PUSH is refused in git's receive-pack terms and git exits non-zero.
func TestADOGitBroker_HeldPushDenied(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
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

// APPROVED FOR THE RUN: later pushes go through with no new approval.
func TestADOGitBroker_HeldPushApprovedForTheRun(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
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

// A REF OUTSIDE THE RUN'S BRANCH NAMESPACE asks for policy_bypass, as a
// protected ref, not code_write.
func TestADOGitBroker_HeldNonRunRefAsksPolicyBypass(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalDenied)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")

	out, err := h.push(t, dir, "main")
	mustBeGitRefusal(t, out, err, "it was not approved")
	asks, _ := cp.snapshot()
	if len(asks) == 0 || asks[0].Get("capability") != string(adoscope.CapPolicyBypass) || asks[0].Get("ref_class") != "protected" {
		t.Errorf("asks = %v, want policy_bypass for a protected ref", asks)
	}
	h.finish(t)
}

// A PACK LARGER THAN http.postBuffer makes git send an empty probe POST to
// git-receive-pack before the real one (remote-curl's probe_rpc). The probe
// moves no ref, so it neither asks nor spends: one approval still covers the
// push.
func TestADOGitBroker_HeldLargePushProbeDoesNotSpend(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead)
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

// A NUL ON A LATER COMMAND LINE is refused before anything reaches Azure
// DevOps, in receive-pack terms: "<old> <new> refs/heads/wardyn/<run>/x\0
// refs/heads/main" on line 2 must not pass as the run's own ref.
func TestADOGitBroker_NULOnALaterCommandIsRefused(t *testing.T) {
	h := newADOGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
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

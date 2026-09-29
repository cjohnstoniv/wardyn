// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	adoHost  = "dev.azure.com"
	adoToken = "entra-bearer-for-acme"
)

// adoHarness is a proxy terminating dev.azure.com with the bearer injected, its
// upstream leg pointed at an adofake whose token carries EVERY scope — so
// whatever the gate forwards, the fake would answer. A refusal can only be the
// gate's.
type adoHarness struct {
	p    *Proxy
	fake *adofake.Server
	log  func() string
}

func newADOHarness(t *testing.T, caps ...adoscope.Capability) *adoHarness {
	t.Helper()
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(adoToken, adofake.ScopeCodeRead, adofake.ScopeCodeWrite, adofake.ScopeWorkRead,
		adofake.ScopeWorkWrite, adofake.ScopeProjectRead, adofake.ScopeTokens)
	fake.AddProject("acme", "", "proj")
	fake.AddProject("evil", "", "loot")

	inj := &injector{byHost: map[string]*injEntry{adoHost: {
		grantID: uuid.New(),
		header:  injectedHeader{name: "Authorization", value: "Bearer " + adoToken},
	}}}
	p, buf := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", strings.TrimPrefix(fake.URL(), "http://"), inj, nil)
	p.mitmHosts = map[string]bool{adoHost: true}
	p.mitmPorts = map[string]int{adoHost: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(adoHost, 443): true}
	p.adoGrants = adoGrantsByHost{adoHost: {Organization: "acme", Capabilities: caps}}
	return &adoHarness{p: p, fake: fake, log: func() string {
		_ = p.sink.close(context.Background())
		return buf.String()
	}}
}

// do sends one request through the MITM with the raw target exactly as given.
func (h *adoHarness) do(t *testing.T, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + adoHost + "\r\n"
	for k, v := range hdr {
		raw += k + ": " + v + "\r\n"
	}
	if body != "" {
		raw += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + body)))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, adoHost, 443)
	return rec
}

// mustRefuse asserts the gate's 403, the denied rule source, and that the fake
// never saw the request.
func (h *adoHarness) mustRefuse(t *testing.T, rec *httptest.ResponseRecorder, wantMsg string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403. body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"typeKey":"CapabilityNotGrantedException"`) {
		t.Errorf("refusal is not in Azure DevOps' shape: %s", rec.Body.String())
	}
	if wantMsg != "" && !strings.Contains(rec.Body.String(), wantMsg) {
		t.Errorf("refusal body %s does not say %q", rec.Body.String(), wantMsg)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("the upstream saw %d request(s) the gate refused: %+v", n, h.fake.Requests())
	}
	if log := h.log(); !strings.Contains(log, `"`+ruleSourceADODenied+`"`) {
		t.Errorf("decision log does not name %s: %s", ruleSourceADODenied, log)
	}
}

func TestADOGate_AllowedReadPassesWithTheInjectedBearer(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead)
	rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", map[string]string{"Authorization": "Bearer sandbox-placeholder"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	reqs := h.fake.Requests()
	if len(reqs) != 1 || reqs[0].Token != adoToken {
		t.Fatalf("upstream saw %+v, want one request carrying the injected bearer", reqs)
	}
	if log := h.log(); !strings.Contains(log, `"`+ruleSourceADO+`"`) || strings.Contains(log, ruleSourceADODenied) {
		t.Errorf("allowed read not logged as %s: %s", ruleSourceADO, log)
	}
}

func TestADOGate_WriteBeyondTheGrantIsRefused(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead)
	rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", `[{"op":"add","path":"/fields/System.Title","value":"x"}]`, nil)
	h.mustRefuse(t, rec, "this run was not granted it")

	// The same write under a grant that holds it is forwarded: the refusal
	// above is the capability check, not the route.
	ok := newADOHarness(t, adoscope.CapRead, adoscope.CapWorkWrite)
	if rec := ok.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1", `[{"op":"add","path":"/fields/System.Title","value":"x"}]`, nil); rec.Code != http.StatusOK {
		t.Fatalf("granted write: status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestADOGate_AnotherOrganisationIsRefused(t *testing.T) {
	for _, target := range []string{"/evil/_apis/projects", "/EVIL/_apis/projects", "/%61cme/_apis/projects"} {
		t.Run(target, func(t *testing.T) {
			h := newADOHarness(t, adoscope.GrantableCapabilities()...)
			h.mustRefuse(t, h.do(t, http.MethodGet, target, "", nil), `granted the \"acme\" organisation only`)
		})
	}
}

func TestADOGate_TokenAreaIsRefused(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			h := newADOHarness(t, adoscope.GrantableCapabilities()...)
			body := ""
			if m == http.MethodPost {
				body = `{"displayName":"x","scope":"app_token"}`
			}
			h.mustRefuse(t, h.do(t, m, "/acme/_apis/tokens/pats?api-version=7.1-preview.1", body, nil), "No run is granted this")
		})
	}
}

func TestADOGate_MethodOverrideCannotSmuggleAWrite(t *testing.T) {
	cases := []struct {
		name, method, target, override, body string
	}{
		{"GET raised to PATCH", http.MethodGet, "/acme/proj/_apis/wit/workitems/1", "PATCH", ""},
		{"POSTed write lowered to GET", http.MethodPost, "/acme/proj/_apis/wit/workitems/1", "GET", `[]`},
		{"POSTed push lowered to GET", http.MethodPost, "/acme/proj/_apis/git/repositories/r/pushes", "GET", `{"refUpdates":[{"name":"refs/heads/main"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newADOHarness(t, adoscope.CapRead)
			h.mustRefuse(t, h.do(t, c.method, c.target, c.body, map[string]string{"x-http-method-override": c.override}), "")
		})
	}
}

// The classifier's evasion cases, end to end: every one is refused even when
// the run holds every grantable capability, so only the evasion guard can be
// what refuses it.
func TestADOGate_PathEvasionsAreRefused(t *testing.T) {
	for _, target := range []string{
		`/acme/_apis\tokens/pats`,
		`/acme\..\evil/_apis/projects`,
		`/acme/_apis/git/..\..\tokens/pats`,
		"/acme/_apis/git/../tokens/pats",
		"/acme/../evil/_apis/projects",
		"/acme/%2e%2e/evil/_apis/projects",
		"/acme/%2E%2E/evil/_apis/projects",
		"/acme/./_apis/tokens/pats",
		"/acme/_apis/wit%2F..%2Fhooks/subscriptions",
		"/acme/_apis/wit%5C..%5Chooks/subscriptions",
	} {
		t.Run(target, func(t *testing.T) {
			h := newADOHarness(t, adoscope.GrantableCapabilities()...)
			h.mustRefuse(t, h.do(t, http.MethodGet, target, "", nil), "")
		})
	}
}

func TestADOGate_GitSmartHTTPOnTheInterceptedConnectionIsRefused(t *testing.T) {
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/acme/proj/_git/repo/info/refs?service=git-receive-pack"},
		{http.MethodPost, "/acme/proj/_git/repo/git-receive-pack"},
		{http.MethodPost, "/acme/proj/_git/repo/git-upload-pack"},
	} {
		t.Run(c.target, func(t *testing.T) {
			h := newADOHarness(t, adoscope.GrantableCapabilities()...)
			h.mustRefuse(t, h.do(t, c.method, c.target, "", nil), "git broker")
		})
	}
}

func TestADOGate_BodyThePeekCannotSeeIsRefused(t *testing.T) {
	// A pull-request update is a body route: its capability depends on
	// completionOptions.bypassPolicy, so a body the peek cannot see whole is
	// refused rather than classified.
	const pr = "/acme/proj/_apis/git/repositories/app/pullrequests/5"
	h := newADOHarness(t, adoscope.GrantableCapabilities()...)
	h.mustRefuse(t, h.do(t, http.MethodPatch, pr, `{}`,
		map[string]string{"Content-Encoding": "gzip"}), "encoded body")

	h = newADOHarness(t, adoscope.GrantableCapabilities()...)
	req := httptest.NewRequest(http.MethodPatch, pr, strings.NewReader("{}"))
	req.ContentLength = adoscope.MaxBodyPeek + 1
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, adoHost, 443)
	h.mustRefuse(t, rec, "too large")
}

// The refusal body is pinned byte for byte: tools key on this shape.
func TestADOGate_RefusalBodyGolden(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead)
	rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1", `[]`, nil)
	const golden = `{"$id":"1","innerException":null,"message":"Wardyn refused this Azure DevOps request: it needs \"Create and update work items\" (work_write), and this run was not granted it.","typeName":"Wardyn.Egress.CapabilityNotGrantedException, Wardyn","typeKey":"CapabilityNotGrantedException","errorCode":0,"eventId":3000}` + "\n"
	if got := rec.Body.String(); got != golden {
		t.Errorf("refusal body drifted:\n got %s\nwant %s", got, golden)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// A host the grant does not cover is not gated at all.
func TestADOGate_UncoveredHostStandsAside(t *testing.T) {
	p := &Proxy{adoGrants: adoGrantsByHost{}}
	if got := p.gateADO(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "example.com", 443, "artifact:mitm"); got != "artifact:mitm" {
		t.Errorf("gateADO on an uncovered host = %q, want the source unchanged", got)
	}
}

// One rule per ref across both doors: a REST push to the run's own branch
// namespace needs code_write, exactly as a git push through the broker does
// (adoRunRefProtected).
func TestADOGate_RunNamespacePushNeedsCodeWrite(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	body := `{"refUpdates":[{"name":"` + BranchNSPrefix(h.p.runID) + `work","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
	rec := h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1", body, nil)
	if rec.Code/100 != 2 {
		t.Fatalf("run-namespace push under code_write: status %d body %s, want it forwarded", rec.Code, rec.Body.String())
	}
}

// A REST push to any other ref is refused as the run's own-branch rule —
// whatever capabilities the run holds, policy_bypass included — and never
// worded as a branch policy nobody consulted.
func TestADOGate_NonRunRefPushIsRefusedWhateverTheCapabilities(t *testing.T) {
	h := newADOHarness(t, adoscope.GrantableCapabilities()...)
	body := `{"refUpdates":[{"name":"refs/heads/main","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
	rec := h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1", body, nil)
	h.mustRefuse(t, rec, "this run may push only to its own branch (wardyn/"+h.p.runID.String()+"/…)")
	if !strings.Contains(rec.Body.String(), "git_push_any_branch: true") || strings.Contains(strings.ToLower(rec.Body.String()), "branch polic") {
		t.Errorf("refusal does not name the switch, or names a branch policy: %s", rec.Body.String())
	}
}

// adoRunBranchRule across the switch: the run's own namespace always passes;
// any other ref is refused unless the policy sets git_push_any_branch.
func TestADORunBranchRule_AnyBranchSwitch(t *testing.T) {
	for _, anyBranch := range []bool{false, true} {
		p := &Proxy{runID: uuid.New(), policy: CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: anyBranch})}
		if msg := p.adoRunBranchRule([]string{BranchNSPrefix(p.runID) + "work"}); msg != "" {
			t.Errorf("any-branch=%v: the run's own namespace is refused: %s", anyBranch, msg)
		}
		for _, ref := range []string{"refs/heads/main", "refs/tags/v1", BranchNSPrefix(p.runID), BranchNSPrefix(uuid.New()) + "work"} {
			if msg := p.adoRunBranchRule([]string{BranchNSPrefix(p.runID) + "work", ref}); (msg == "") != anyBranch {
				t.Errorf("any-branch=%v: adoRunBranchRule(%q) = %q", anyBranch, ref, msg)
			}
		}
	}
}

// With git_push_any_branch a REST push to main needs code_write only and is
// forwarded; without code_write it is still refused, as code_write.
func TestADOGate_AnyBranchPushNeedsCodeWrite(t *testing.T) {
	body := `{"refUpdates":[{"name":"refs/heads/main","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
	const target = "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1"
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	h.p.policy = CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: true})
	if rec := h.do(t, http.MethodPost, target, body, nil); rec.Code/100 != 2 {
		t.Fatalf("any-branch push to main under code_write: status %d body %s, want it forwarded", rec.Code, rec.Body.String())
	}

	ro := newADOHarness(t, adoscope.CapRead)
	ro.p.policy = CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: true})
	ro.mustRefuse(t, ro.do(t, http.MethodPost, target, body, nil), "("+string(adoscope.CapCodeWrite)+")")
}

// The switch widens where a push may land, never a real policy bypass: a pull
// request completed with bypassPolicy still needs policy_bypass, spelled as one.
func TestADOGate_AnyBranchLeavesPRBypassPolicyBypass(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR)
	h.p.policy = CompilePolicy(types.RunPolicySpec{GitPushAnyBranch: true})
	rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/git/repositories/app/pullrequests/1?api-version=7.1",
		`{"status":"completed","completionOptions":{"bypassPolicy":true}}`, nil)
	h.mustRefuse(t, rec, "("+string(adoscope.CapPolicyBypass)+")")
	if !strings.Contains(rec.Body.String(), adoscope.Label(adoscope.CapPolicyBypass)) {
		t.Errorf("a real bypass is not refused as one: %s", rec.Body.String())
	}
}

// Update Ref names its branch in ?filter=: the run's own-branch rule reads
// it there, whatever ref the body names; with no filter the ref cannot be
// checked and the request is refused as that.
func TestADOGate_RefPatchReadsTheFilter(t *testing.T) {
	h := newADOHarness(t, adoscope.GrantableCapabilities()...)
	const refs = "/acme/proj/_apis/git/repositories/app/refs"
	body := `{"isLocked":true}`
	h.mustRefuse(t, h.do(t, http.MethodPatch, refs+"?filter=heads/main&api-version=7.1", body, nil), "may push only to its own branch")
	h = newADOHarness(t, adoscope.GrantableCapabilities()...)
	h.mustRefuse(t, h.do(t, http.MethodPatch, refs+"?api-version=7.1", body, nil), "names no ref Wardyn can check")
	h = newADOHarness(t, adoscope.GrantableCapabilities()...)
	h.mustRefuse(t, h.do(t, http.MethodPatch, refs+"?filter=heads/wardyn/x&filter=heads/main", body, nil), "names no ref Wardyn can check")

	ok := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	rec := ok.do(t, http.MethodPatch, refs+"?filter=heads/wardyn/"+ok.p.runID.String()+"/work&api-version=7.1", body, nil)
	if log := ok.log(); strings.Contains(log, ruleSourceADODenied) || !strings.Contains(log, `"`+ruleSourceADO+`"`) {
		t.Fatalf("a lock on the run's own branch under code_write was not forwarded (status %d): %s", rec.Code, log)
	}
}

// Annotated tags and REST cherry-picks/reverts create a ref, so they are held
// to the same rule: a tag, or a generated branch outside the run's own branch,
// is refused whatever the run holds; a generated branch inside it is code_write.
func TestADOGate_TagsAndGeneratedBranchesFollowTheRunBranchRule(t *testing.T) {
	const repo = "/acme/proj/_apis/git/repositories/app/"
	for _, tc := range []struct{ name, path, body string }{
		{"annotated tag", repo + "annotatedtags", `{"name":"v1","taggedObject":{"objectId":"` + zeroOID + `"},"message":"m"}`},
		{"cherry-pick onto main", repo + "cherrypicks", `{"generatedRefName":"refs/heads/main-pick","ontoRefName":"refs/heads/main"}`},
		{"revert onto main", repo + "reverts", `{"generatedRefName":"refs/heads/undo","ontoRefName":"refs/heads/main"}`},
	} {
		h := newADOHarness(t, adoscope.GrantableCapabilities()...)
		h.mustRefuse(t, h.do(t, http.MethodPost, tc.path+"?api-version=7.1", tc.body, nil), "may push only to its own branch")
	}

	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	body := `{"generatedRefName":"` + BranchNSPrefix(h.p.runID) + `undo","ontoRefName":"refs/heads/main"}`
	h.do(t, http.MethodPost, repo+"reverts?api-version=7.1", body, nil)
	if log := h.log(); strings.Contains(log, ruleSourceADODenied) {
		t.Fatalf("a revert onto the run's own branch under code_write was refused: %s", log)
	}
	ro := newADOHarness(t, adoscope.CapRead)
	body = `{"generatedRefName":"` + BranchNSPrefix(ro.p.runID) + `undo","ontoRefName":"refs/heads/main"}`
	ro.mustRefuse(t, ro.do(t, http.MethodPost, repo+"reverts", body, nil), "("+string(adoscope.CapCodeWrite)+")")
}

// The plain forward lane refuses a host with an Azure DevOps grant outright: an
// absolute-form https:// request sent without CONNECT would otherwise reach
// applyInjection with no organisation or capability check. REST belongs in the
// intercepted tunnel and git in the broker — never here.
func TestADOGate_PlainLaneRefusesACoveredHost(t *testing.T) {
	h := newADOHarness(t)
	req := httptest.NewRequest(http.MethodGet, "https://"+adoHost+"/acme/_apis/projects?api-version=7.1", nil)
	rec := httptest.NewRecorder()
	h.p.servePlain(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "HTTPS tunnel (CONNECT)") {
		t.Fatalf("status %d body %s, want the plain-lane refusal", rec.Code, rec.Body.String())
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("upstream saw %d request(s), want none", n)
	}
	if log := h.log(); !strings.Contains(log, `"`+ruleSourceADODenied+`"`) {
		t.Fatalf("decision log lacks %s: %s", ruleSourceADODenied, log)
	}
}

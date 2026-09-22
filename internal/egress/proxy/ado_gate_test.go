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
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	adoHost  = "dev.azure.com"
	adoToken = "entra-bearer-for-acme"
)

// adoGrantMap is the test double for ADOGrantSource.
type adoGrantMap map[string]ADOGrant

func (m adoGrantMap) ADOGrantFor(host string) (ADOGrant, bool) {
	g, ok := m[host]
	return g, ok
}

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
	p.adoGrants = adoGrantMap{adoHost: {Organization: "acme", Capabilities: caps}}
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
	p := &Proxy{adoGrants: adoGrantMap{}}
	if got := p.gateADO(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "example.com", 443, "artifact:mitm"); got != "artifact:mitm" {
		t.Errorf("gateADO on an uncovered host = %q, want the source unchanged", got)
	}
}

// ONE RULE PER REF ACROSS BOTH DOORS: a REST push to the run's own branch
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

// A REST push to any other ref is a protected-ref move and needs
// policy_bypass, as on the git door.
func TestADOGate_NonRunRefPushNeedsPolicyBypass(t *testing.T) {
	h := newADOHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	body := `{"refUpdates":[{"name":"refs/heads/main","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
	h.mustRefuse(t, h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1", body, nil),
		string(adoscope.CapPolicyBypass))
}

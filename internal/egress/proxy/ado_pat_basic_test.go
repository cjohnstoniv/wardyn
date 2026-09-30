// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure DevOps lane when the credential the control plane resolves is a
// personal access token, sent as HTTP Basic (":" + PAT), rather than an Entra
// bearer. The proxy never formats the header: it injects the name and value the
// control plane resolved, so these tests fix what is still the proxy's to
// guarantee for a Basic PAT on BOTH doors (the REST tunnel and the git broker):
// the sandbox's own credential headers are stripped, both renderings of the PAT
// are masked, and the organisation pin, capability check, run-branch rule and
// tokens-API refusal apply exactly as they do to a bearer.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// adoBasicPAT is the header value the control plane resolves for a personal
// access token: HTTP Basic with an empty user, the PAT as the password.
func adoBasicPAT(pat string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+pat))
}

// useBasicPAT re-points every host of p's injector at the Basic form of pat.
// Call it before the proxy serves.
func useBasicPAT(p *Proxy, pat string) {
	for _, e := range p.inject.byHost {
		e.header = injectedHeader{name: "Authorization", value: adoBasicPAT(pat)}
	}
}

// sandboxADOHeaders is what an in-sandbox tool that insists on a token sends: a
// Basic placeholder (the value of AZURE_DEVOPS_EXT_PAT), plus the other
// credential headers a client may add.
func sandboxADOHeaders() map[string]string {
	return map[string]string{
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(":wardyn-proxy-injects-this-credential")),
		"Cookie":        "session=SANDBOX-OWN-COOKIE",
		"Private-Token": "SANDBOX-OWN-PRIVATE-TOKEN",
	}
}

func newADOBasicHarness(t *testing.T, caps ...adoscope.Capability) (*adoHarness, string) {
	t.Helper()
	h := newADOHarness(t, caps...)
	pat := "pat-" + uuid.NewString()
	h.fake.RegisterToken(pat, adofake.ScopeCodeRead, adofake.ScopeCodeWrite, adofake.ScopeWorkRead,
		adofake.ScopeWorkWrite, adofake.ScopeProjectRead, adofake.ScopeTokens)
	useBasicPAT(h.p, pat)
	echoControlPlane(t, h.p.inject)
	return h, pat
}

// REST DOOR: the PAT rides as Basic, and nothing the sandbox put on the request
// reaches Azure DevOps.
func TestADOGate_BasicPATIsInjectedAndSandboxHeadersAreStripped(t *testing.T) {
	h, pat := newADOBasicHarness(t, adoscope.CapRead)
	rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", sandboxADOHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	reqs := h.fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	got := reqs[0]
	if want := adoBasicPAT(pat); got.Headers.Get("Authorization") != want || got.Token != pat || !got.Authorized {
		t.Errorf("upstream Authorization = %q (token %q, authorized %v), want %q", got.Headers.Get("Authorization"), got.Token, got.Authorized, want)
	}
	for _, name := range []string{"Cookie", "Private-Token"} {
		if v := got.Headers.Get(name); v != "" {
			t.Errorf("the sandbox's %s = %q reached Azure DevOps", name, v)
		}
	}
	if log := h.log(); !strings.Contains(log, `"`+ruleSourceADO+`"`) || strings.Contains(log, ruleSourceADODenied) ||
		strings.Contains(log, pat) || strings.Contains(log, strings.TrimPrefix(adoBasicPAT(pat), "Basic ")) {
		t.Errorf("decision log is not a clean %s allow: %s", ruleSourceADO, log)
	}
}

// REST DOOR: a Basic PAT gets the same gate as a bearer. Each refusal below is
// the gate's, since the fake's PAT holds every scope.
func TestADOGate_BasicPATGrantRunsTheSameGate(t *testing.T) {
	t.Run("write beyond the grant", func(t *testing.T) {
		h, _ := newADOBasicHarness(t, adoscope.CapRead)
		rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1",
			`[{"op":"add","path":"/fields/System.Title","value":"x"}]`, sandboxADOHeaders())
		h.mustRefuse(t, rec, "this run was not granted it")
	})
	t.Run("granted write is forwarded", func(t *testing.T) {
		h, pat := newADOBasicHarness(t, adoscope.CapRead, adoscope.CapWorkWrite)
		rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1",
			`[{"op":"add","path":"/fields/System.Title","value":"x"}]`, sandboxADOHeaders())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if reqs := h.fake.Requests(); len(reqs) != 1 || reqs[0].Headers.Get("Authorization") != adoBasicPAT(pat) {
			t.Fatalf("upstream saw %+v, want one write carrying the injected Basic PAT", reqs)
		}
	})
	t.Run("another organisation", func(t *testing.T) {
		h, _ := newADOBasicHarness(t, adoscope.GrantableCapabilities()...)
		h.mustRefuse(t, h.do(t, http.MethodGet, "/evil/_apis/projects", "", nil), `granted the \"acme\" organisation only`)
	})
	t.Run("the tokens API", func(t *testing.T) {
		h, _ := newADOBasicHarness(t, adoscope.GrantableCapabilities()...)
		body := `{"displayName":"x","scope":"app_token"}`
		h.mustRefuse(t, h.do(t, http.MethodPost, "/acme/_apis/tokens/pats?api-version=7.1-preview.1", body, nil), "No run is granted this")
	})
	t.Run("a ref outside the run's branch", func(t *testing.T) {
		h, _ := newADOBasicHarness(t, adoscope.GrantableCapabilities()...)
		body := `{"refUpdates":[{"name":"refs/heads/main","oldObjectId":"` + zeroOID + `"}],"commits":[]}`
		h.mustRefuse(t, h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/pushes?api-version=7.1", body, nil),
			"this run may push only to its own branch")
	})
}

// A re-resolve (the control plane replaced the run's PAT on renewal or
// widening) is what the next request carries, and the new PAT is masked in
// every rendering as the first was.
func TestInjectionMaskCoversTheBasicPATRenderings(t *testing.T) {
	first, second := "pat-first-"+uuid.NewString(), "pat-second-"+uuid.NewString()
	current := first
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Host: "dev.azure.com", Header: "Authorization", Value: adoBasicPAT(current), JTI: uuid.NewString(),
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	defer cp.Close()

	pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"dev.azure.com"}})
	rules := []InjectionConfig{{
		InjectionRule: egress.InjectionRule{Host: "dev.azure.com", Header: "Authorization", Format: "Basic %s"},
		GrantID:       uuid.New(),
	}}
	inj, err := buildInjector(context.Background(), cp.URL, newTokenSource("tok"), pol, rules, cp.Client())
	if err != nil {
		t.Fatalf("buildInjector: %v", err)
	}
	assertPATMasked(t, first)

	// Renewal: the entry is inside its refresh margin, so the next resolve asks again.
	current = second
	inj.byHost["dev.azure.com"].expiresAt = time.Now().Add(time.Minute).UnixMilli()
	h, ok, err := inj.resolve("dev.azure.com")
	if err != nil || !ok {
		t.Fatalf("resolve = %v, %v", ok, err)
	}
	if h.value != adoBasicPAT(second) {
		t.Fatalf("the renewed PAT is not what the next request carries: %q", h.value)
	}
	assertPATMasked(t, second)
}

// assertPATMasked fails unless every rendering of pat a request or error can
// carry is masked: the header, the base64 wire form, the decoded ":pat", the PAT.
func assertPATMasked(t *testing.T, pat string) {
	t.Helper()
	wire := base64.StdEncoding.EncodeToString([]byte(":" + pat))
	assertMasked(t, "Basic "+wire, "the formatted Basic header")
	assertMasked(t, wire, "the base64 Basic rendering of the PAT")
	assertMasked(t, ":"+pat, "the decoded user:password rendering of the PAT")
	assertMasked(t, pat, "the raw PAT")
}

// basicGitHarness is the git-broker harness with the run's PAT injected as
// Basic instead of a bearer.
func basicGitHarness(t *testing.T, caps ...adoscope.Capability) *adoGitHarness {
	t.Helper()
	return newADOGitHarnessWith(t, func(p *Proxy, pat string) { useBasicPAT(p, pat) }, caps...)
}

// cloneWithSandboxBasic clones with the sandbox's own Basic placeholder and a
// Private-Token smuggled onto every request.
func (h *adoGitHarness) cloneWithSandboxBasic(t *testing.T, cloneURL string) string {
	t.Helper()
	args := []string{}
	for name, value := range sandboxADOHeaders() {
		args = append(args, "-c", "http.extraHeader="+name+": "+value)
	}
	if out, err := h.git(t, append(args, "clone", cloneURL, "app")...); err != nil {
		t.Fatalf("git clone %s: %v\n%s", cloneURL, err, out)
	}
	return h.work + "/app"
}

// GIT DOOR: a clone carries the run's PAT as Basic and nothing the sandbox put
// on the request, and the broker mask-registers the PAT in both renderings
// before anything can log it.
func TestADOGitBroker_BasicPATCloneCarriesThePATAndStripsTheSandbox(t *testing.T) {
	h := basicGitHarness(t, adoscope.CapRead)
	wire := base64.StdEncoding.EncodeToString([]byte(":" + h.bearer))
	if !strings.Contains(string(maskDecisionBytes([]byte("error "+wire+" "+h.bearer))), wire) {
		t.Fatal("precondition: the PAT is masked before the broker ever used it")
	}
	h.cloneWithSandboxBasic(t, "https://acme@dev.azure.com/acme/proj/_git/app")
	assertPATMasked(t, h.bearer)

	reqs := h.fake.Requests()
	if len(reqs) < 2 {
		t.Fatalf("the fake saw %d requests, want the advertisement and the upload-pack", len(reqs))
	}
	for _, r := range reqs {
		if got := r.Headers.Get("Authorization"); got != "Basic "+wire || r.Token != h.bearer || !r.Authorized ||
			r.Headers.Get("Cookie") != "" || r.Headers.Get("Private-Token") != "" {
			t.Errorf("%s %s reached Azure DevOps with Authorization %q, Cookie %q, Private-Token %q; want the brokered Basic PAT only",
				r.Method, r.Path, got, r.Headers.Get("Cookie"), r.Headers.Get("Private-Token"))
		}
	}
	log := h.finish(t)
	if strings.Contains(log, wire) || !strings.Contains(log, ruleSourceADOGit) {
		t.Errorf("decision log carries the base64 PAT or has no %s allow row:\n%s", ruleSourceADOGit, log)
	}
	if strings.Contains(h.logs.String(), wire) {
		t.Errorf("slog carries the base64 PAT:\n%s", h.logs.String())
	}
}

// GIT DOOR: a Basic PAT is held to the run's capabilities on the pack POST,
// refused in git's terms, and the refused pack never reaches Azure DevOps.
func TestADOGitBroker_BasicPATPushBeyondTheGrantIsRefused(t *testing.T) {
	h := basicGitHarness(t, adoscope.CapRead)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err := h.push(t, dir, h.runBranch())
	mustBeGitRefusal(t, out, err, "this run was not granted it")
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the refused pack reached Azure DevOps %d times", n)
	}
	h.finish(t)

	// The run's own-branch rule holds whatever the capabilities: main is refused
	// before the pack reaches Azure DevOps.
	off := basicGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	offDir := off.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	out, err = off.push(t, offDir, "main")
	mustBeGitRefusal(t, out, err, "this run may push only to its own branch")
	if n := off.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 0 {
		t.Errorf("the off-branch pack reached Azure DevOps %d times", n)
	}
	off.finish(t)

	other := basicGitHarness(t, adoscope.CapRead, adoscope.CapCodeWrite)
	if out, err := other.git(t, "clone", "https://dev.azure.com/evil/loot/_git/app", "loot"); err == nil ||
		!strings.Contains(out, `granted the "acme" Azure DevOps organisation only`) {
		t.Errorf("another organisation was not refused (err %v):\n%s", err, out)
	}
	if n := len(other.fake.Requests()); n != 0 {
		t.Errorf("the fake saw %d requests for another organisation, want 0", n)
	}
	other.finish(t)
}

// GIT DOOR: a push held for approval goes out under the run's Basic PAT once approved.
func TestADOGitBroker_BasicPATHeldPushForwardsUnderThePAT(t *testing.T) {
	h := basicGitHarness(t, adoscope.CapRead)
	cp := newCapControlPlane(t)
	h.withHold(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)})
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	if out, err := h.push(t, dir, h.runBranch()); err != nil {
		t.Fatalf("held push, approved once: %v\n%s", err, out)
	}
	want := adoBasicPAT(h.bearer)
	for _, r := range h.fake.Requests() {
		if r.Headers.Get("Authorization") != want || !r.Authorized {
			t.Errorf("%s %s carried Authorization %q (authorized %v), want the brokered Basic PAT", r.Method, r.Path, r.Headers.Get("Authorization"), r.Authorized)
		}
	}
	if n := h.countEndpoint(adofake.EndpointGitReceivePack, ""); n != 1 {
		t.Errorf("Azure DevOps saw %d pack uploads, want 1", n)
	}
	h.finish(t)
}

// The PAT Lifecycle API's own hosts: a Basic PAT under a grant holding every
// grantable capability still cannot list, mint, update or revoke a PAT there,
// whatever the spelling of the path (§4a S3).
func TestADOGate_BasicPATTokensAPIRefusedOnThePATLifecycleHost(t *testing.T) {
	for _, tc := range []struct{ name, host, path string }{
		{"pats", "vssps.dev.azure.com", "/acme/_apis/tokens/pats?api-version=7.1-preview.1"},
		{"mixed case", "vssps.dev.azure.com", "/acme/_apis/Tokens/PATs?api-version=7.1-preview.1"},
		{"encoded underscore", "vssps.dev.azure.com", "/acme/%5Fapis/tokens/pats"},
		{"tokenadmin", "vssps.dev.azure.com", "/acme/_apis/tokenadmin/personalaccesstokens/x"},
		{"legacy host", "acme.vssps.visualstudio.com", "/_apis/tokens/pats?api-version=7.1-preview.1"},
	} {
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
			t.Run(tc.name+" "+m, func(t *testing.T) {
				fake := adofake.New()
				t.Cleanup(fake.Close)
				pat := "pat-" + uuid.NewString()
				fake.RegisterToken(pat, adofake.ScopeTokens, adofake.ScopeCodeRead)
				inj := &injector{byHost: map[string]*injEntry{tc.host: {
					grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: adoBasicPAT(pat)},
				}}}
				p, buf := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", strings.TrimPrefix(fake.URL(), "http://"), inj, nil)
				p.mitmHosts = map[string]bool{tc.host: true}
				p.mitmPorts = map[string]int{tc.host: 443}
				p.mitmPlaintext = map[string]bool{plaintextKey(tc.host, 443): true}
				p.adoGrants = adoGrantsByHost{tc.host: {Organization: "acme", Capabilities: adoscope.GrantableCapabilities()}}

				raw := m + " " + tc.path + " HTTP/1.1\r\nHost: " + tc.host + "\r\n"
				body := ""
				if m == http.MethodPost || m == http.MethodPut {
					body = `{"displayName":"x","scope":"app_token"}`
					raw += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
				}
				req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + body)))
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				p.serveMITMRequest(rec, req, tc.host, 443)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, body = %s; want 403", rec.Code, rec.Body.String())
				}
				if n := len(fake.Requests()); n != 0 {
					t.Fatalf("the upstream saw %d request(s) the gate refused: %+v", n, fake.Requests())
				}
				_ = p.sink.close(context.Background())
				if !strings.Contains(buf.String(), `"`+ruleSourceADODenied+`"`) {
					t.Errorf("decision log does not name %s: %s", ruleSourceADODenied, buf.String())
				}
			})
		}
	}
}

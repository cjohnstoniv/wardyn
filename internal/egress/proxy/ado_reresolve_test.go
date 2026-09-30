// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The proxy's one rule for a stale Azure DevOps header (lane L3F): the sidecar
// holds one header per Azure DevOps host and nothing in the control plane can
// invalidate it, so when Azure DevOps refuses that header (a 401, or a 203
// sign-in page) the proxy drops it, re-resolves once with ?stale_jti=, and
// retries the request once if its body can be sent again.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

const (
	logReresolved    = `"brokered:ado:reresolved"`
	logGitReresolved = `"brokered:ado-git:reresolved"`
)

// adoPAT is one personal access token as the control plane knows it.
type adoPAT struct{ value, jti string }

func newADOPAT(jti string) adoPAT { return adoPAT{value: "pat-" + uuid.NewString(), jti: jti} }

// staleCP answers injection resolves the way wardynd answers them for a
// minted_pat run (plan §15 L2): it holds the run's current PAT; a stale_jti
// naming an older PAT gets the current one, without minting; a stale_jti naming
// the current one mints a fresh PAT, at most once a minute, else gets the
// current one. locked makes that many resolves answer 423 reauth_pending first.
type staleCP struct {
	srv        *httptest.Server
	approvalID uuid.UUID

	mu         sync.Mutex
	onMint     func(adoPAT) // registers a minted PAT with Azure DevOps
	cur        adoPAT
	older      map[string]bool
	lastForced time.Time
	mints      int
	queries    []string // every resolve's stale_jti, "" for none
	locked     int
}

func newStaleCP(t *testing.T, cur adoPAT, older ...string) *staleCP {
	t.Helper()
	cp := &staleCP{cur: cur, older: map[string]bool{}, approvalID: uuid.New()}
	for _, j := range older {
		cp.older[j] = true
	}
	cp.srv = httptest.NewServer(http.HandlerFunc(cp.resolve))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *staleCP) resolve(w http.ResponseWriter, r *http.Request) {
	stale := r.URL.Query().Get("stale_jti")
	cp.mu.Lock()
	cp.queries = append(cp.queries, stale)
	if cp.locked > 0 {
		cp.locked--
		cp.mu.Unlock()
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "reauth_pending", "approval_id": cp.approvalID.String()})
		return
	}
	var minted *adoPAT
	if stale != "" && stale == cp.cur.jti && time.Since(cp.lastForced) >= time.Minute {
		cp.mints++
		cp.lastForced = time.Now()
		cp.older[cp.cur.jti] = true
		cp.cur = newADOPAT(fmt.Sprintf("auth-minted-%d", cp.mints))
		minted = &cp.cur
	}
	cur, onMint := cp.cur, cp.onMint
	cp.mu.Unlock()
	if minted != nil && onMint != nil {
		onMint(*minted)
	}
	_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
		Header: "Authorization", Value: adoBasicPAT(cur.value), JTI: cur.jti,
		ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli(),
	})
}

// staleQueries is every resolve that carried a stale_jti, in order.
func (cp *staleCP) staleQueries() []string {
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

func (cp *staleCP) resolves() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return len(cp.queries)
}

func (cp *staleCP) mintCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.mints
}

func (cp *staleCP) lockNext(n int) {
	cp.mu.Lock()
	cp.locked = n
	cp.mu.Unlock()
}

func (cp *staleCP) current() adoPAT {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.cur
}

// wire points inj at cp and gives host the PAT held, as its boot resolve would.
func (cp *staleCP) wire(inj *injector, held adoPAT, hosts ...string) {
	inj.base, inj.token, inj.client = cp.srv.URL, newTokenSource("RUNTOK"), cp.srv.Client()
	for _, host := range hosts {
		inj.byHost[host].install(types.ResolvedInjection{
			Header: "Authorization", Value: adoBasicPAT(held.value), JTI: held.jti,
			ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli(),
		}, time.Now())
	}
}

// echoControlPlane points inj at a control plane that answers every resolve with the credential
// the grant's entry holds now, as wardynd answers a stale hint for a bearer or own_pat grant: the
// same credential again. A harness whose injector had no control plane wires one, so a refusal's
// re-resolve has somewhere to go and the refusal is relayed as before.
func echoControlPlane(t *testing.T, inj *injector) {
	t.Helper()
	creds := map[string]injectedHeader{}
	for _, e := range inj.byHost {
		creds[e.grantID.String()] = e.header
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := creds[r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: h.name, Value: h.value, JTI: h.jti})
	}))
	t.Cleanup(srv.Close)
	inj.base, inj.token, inj.client = srv.URL, newTokenSource("RUNTOK"), srv.Client()
}

// frontReq is one request the front saw on its way to Azure DevOps.
type frontReq struct {
	path, token string
	body        []byte
}

// adoFront sits between the proxy and the fake: it records every request with
// its body, and answers a request refuse picks with Azure DevOps' own 401
// (FaultBasicUnauthorized's shape) or 203 sign-in page.
type adoFront struct {
	mu     sync.Mutex
	seen   []frontReq
	refuse func(path, token string) int
}

func (f *adoFront) setRefuse(fn func(path, token string) int) {
	f.mu.Lock()
	f.refuse = fn
	f.mu.Unlock()
}

func (f *adoFront) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, pass, ok := r.BasicAuth(); ok {
			token = pass
		}
		f.mu.Lock()
		f.seen = append(f.seen, frontReq{path: r.URL.Path, token: token, body: body})
		refuse := f.refuse
		f.mu.Unlock()
		status := 0
		if refuse != nil {
			status = refuse(r.URL.Path, token)
		}
		switch status {
		case http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="https://tfsprodcus14.visualstudio.com/"`)
			w.WriteHeader(http.StatusUnauthorized)
		case http.StatusNonAuthoritativeInfo:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNonAuthoritativeInfo)
			_, _ = io.WriteString(w, adofake.SignInPage)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// requests is what the front saw whose path ends in suffix.
func (f *adoFront) requests(suffix string) []frontReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []frontReq
	for _, q := range f.seen {
		if strings.HasSuffix(q.path, suffix) {
			out = append(out, q)
		}
	}
	return out
}

func tokensOf(reqs []frontReq) []string {
	out := make([]string, len(reqs))
	for i, q := range reqs {
		out[i] = q.token
	}
	return out
}

// refuseToken refuses every request carrying token with status.
func refuseToken(token string, status int) func(string, string) int {
	return func(_, got string) int {
		if got == token {
			return status
		}
		return 0
	}
}

// reresolveHarness is the REST door with a front before the fake and the
// dev.azure.com entry holding old, a PAT Azure DevOps no longer takes, while the
// control plane already holds cur (another host re-resolved first).
type reresolveHarness struct {
	*adoHarness
	front    *adoFront
	cp       *staleCP
	old, cur adoPAT
}

var allADOScopes = []string{adofake.ScopeCodeRead, adofake.ScopeCodeWrite, adofake.ScopeWorkRead,
	adofake.ScopeWorkWrite, adofake.ScopeProjectRead}

func newReresolveHarness(t *testing.T, caps ...adoscope.Capability) *reresolveHarness {
	t.Helper()
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.AddProject("acme", "", "proj")
	old, cur := newADOPAT("auth-1"), newADOPAT("auth-2")
	fake.RegisterToken(cur.value, allADOScopes...)
	front := &adoFront{}
	fu, _ := url.Parse(fake.URL())
	up := httptest.NewServer(front.wrap(httputil.NewSingleHostReverseProxy(fu)))
	t.Cleanup(up.Close)

	inj := &injector{byHost: map[string]*injEntry{adoHost: {grantID: uuid.New()}}}
	cp := newStaleCP(t, cur, old.jti)
	cp.mu.Lock()
	cp.onMint = func(p adoPAT) { fake.RegisterToken(p.value, allADOScopes...) }
	cp.mu.Unlock()
	cp.wire(inj, old, adoHost)
	p, buf := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", strings.TrimPrefix(up.URL, "http://"), inj, nil)
	p.mitmHosts = map[string]bool{adoHost: true}
	p.mitmPorts = map[string]int{adoHost: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(adoHost, 443): true}
	p.adoGrants = adoGrantsByHost{adoHost: {Organization: "acme", Capabilities: caps}}
	h := &adoHarness{p: p, fake: fake, log: func() string {
		_ = p.sink.close(context.Background())
		return buf.String()
	}}
	return &reresolveHarness{adoHarness: h, front: front, cp: cp, old: old, cur: cur}
}

// noCredentialBytes asserts no rendering of any PAT appears in the texts.
func noCredentialBytes(t *testing.T, texts map[string]string, pats ...adoPAT) {
	t.Helper()
	for _, pat := range pats {
		for name, s := range texts {
			if strings.Contains(s, pat.value) || strings.Contains(s, adoBasicPAT(pat.value)) {
				t.Errorf("%s carries PAT %s: %s", name, pat.jti, s)
			}
		}
	}
}

// masked reports whether the process mask hides every rendering of pat.
func masked(pat adoPAT) bool {
	m := procRegistry.Masker(uuid.Nil)
	for _, v := range []string{pat.value, adoBasicPAT(pat.value)} {
		if bytes.Contains(m.Mask([]byte("x "+v+" y")), []byte(v)) {
			return false
		}
	}
	return true
}

const projectsGET = "/acme/_apis/projects?api-version=7.1"

// Host B still holds the PAT host A replaced: Azure DevOps answers B's GET 401
// once, the proxy re-resolves with stale_jti, the control plane hands over the
// newer PAT without minting, and the sandbox sees 200.
func TestADOReresolve_StaleHostHealsAndRetries(t *testing.T) {
	logs := captureSlog(t)
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	h.front.setRefuse(refuseToken(h.old.value, http.StatusUnauthorized))

	rec := h.do(t, http.MethodGet, projectsGET, "", sandboxADOHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry. body=%s", rec.Code, rec.Body.String())
	}
	if got := tokensOf(h.front.requests("/_apis/projects")); len(got) != 2 || got[0] != h.old.value || got[1] != h.cur.value {
		t.Fatalf("Azure DevOps saw tokens %v, want the stale PAT once then the current one", got)
	}
	if got := h.cp.staleQueries(); len(got) != 1 || got[0] != h.old.jti {
		t.Fatalf("stale re-resolves = %v, want one naming %s", got, h.old.jti)
	}
	if n := h.cp.mintCount(); n != 0 {
		t.Errorf("the control plane minted %d PATs, want 0: it already held a newer one", n)
	}
	if !masked(h.cur) {
		t.Error("the re-resolved PAT is not registered for masking")
	}

	// The next request rides the fresh header: no refusal, no resolve.
	if rec := h.do(t, http.MethodGet, projectsGET, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("next request: status = %d", rec.Code)
	}
	if got := tokensOf(h.front.requests("/_apis/projects")); len(got) != 3 || got[2] != h.cur.value {
		t.Errorf("next request carried %v, want the current PAT", got)
	}
	if n := h.cp.resolves(); n != 1 {
		t.Errorf("control-plane resolves = %d, want 1", n)
	}
	log := h.log()
	if strings.Count(log, logReresolved) != 1 || strings.Contains(log, ruleSourceADOUpstreamRefused) {
		t.Errorf("decision log: want exactly one %s and no refusal row: %s", logReresolved, log)
	}
	noCredentialBytes(t, map[string]string{"decision log": log, "slog": logs.String(), "body": rec.Body.String()}, h.old, h.cur)
}

// A 203 sign-in page heals the same way.
func TestADOReresolve_SignInPageHeals(t *testing.T) {
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	h.front.setRefuse(refuseToken(h.old.value, http.StatusNonAuthoritativeInfo))
	if rec := h.do(t, http.MethodGet, projectsGET, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry. body=%s", rec.Code, rec.Body.String())
	}
	if got := h.cp.staleQueries(); len(got) != 1 || got[0] != h.old.jti {
		t.Fatalf("stale re-resolves = %v, want one naming %s", got, h.old.jti)
	}
	if log := h.log(); strings.Count(log, logReresolved) != 1 || strings.Contains(log, ruleSourceADOUpstreamNotSignedIn) {
		t.Errorf("decision log: %s", log)
	}
}

// A body the gate buffered whole (a ref update is classified on its body) is
// retried byte for byte.
func TestADOReresolve_BufferedBodyIsReplayedByteIdentical(t *testing.T) {
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead, adoscope.CapCodeWrite)
	h.front.setRefuse(refuseToken(h.old.value, http.StatusUnauthorized))
	body := `[{"name":"` + BranchNSPrefix(h.p.runID) + `work","oldObjectId":"` + zeroOID + `","newObjectId":"` + strings.Repeat("a", 40) + `"}]`
	rec := h.do(t, http.MethodPost, "/acme/proj/_apis/git/repositories/app/refs?api-version=7.1", body, nil)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("status = %d, want the retry's answer. body=%s", rec.Code, rec.Body.String())
	}
	sent := h.front.requests("/refs")
	if len(sent) != 2 || sent[1].token != h.cur.value {
		t.Fatalf("Azure DevOps saw %v, want the stale PAT then the current one", tokensOf(sent))
	}
	for i, q := range sent {
		if string(q.body) != body {
			t.Errorf("attempt %d carried body %q, want %q", i+1, q.body, body)
		}
	}
}

// A body nobody buffered (over adoscope.MaxBodyPeek, on a route classified
// without it) is not retried: the refusal is relayed, but the header is dropped
// all the same, and the next request re-resolves and uses the fresh one.
func TestADOReresolve_UnbufferedBodyIsNotRetriedButTheHeaderIsDropped(t *testing.T) {
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead, adoscope.CapWorkWrite)
	h.front.setRefuse(refuseToken(h.old.value, http.StatusUnauthorized))
	big := `[{"op":"add","path":"/fields/System.Description","value":"` + strings.Repeat("x", adoscope.MaxBodyPeek+1) + `"}]`
	rec := h.do(t, http.MethodPatch, "/acme/proj/_apis/wit/workitems/1?api-version=7.1", big, nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get(egressHeaderDetail), adoMsgRefused) {
		t.Fatalf("status = %d detail=%q, want the relayed 401", rec.Code, rec.Header().Get(egressHeaderDetail))
	}
	if n := len(h.front.requests("/workitems/1")); n != 1 {
		t.Fatalf("the unbuffered PATCH reached Azure DevOps %d times, want 1", n)
	}
	if n := h.cp.resolves(); n != 0 {
		t.Fatalf("control-plane resolves before the next request = %d, want 0", n)
	}

	if rec := h.do(t, http.MethodGet, projectsGET, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("next request: status = %d, want 200", rec.Code)
	}
	if got := tokensOf(h.front.requests("/_apis/projects")); len(got) != 1 || got[0] != h.cur.value {
		t.Errorf("next request carried %v, want only the current PAT", got)
	}
	if got := h.cp.staleQueries(); len(got) != 1 || got[0] != h.old.jti {
		t.Errorf("stale re-resolves = %v, want one naming %s", got, h.old.jti)
	}
	if log := h.log(); strings.Contains(log, logReresolved) {
		t.Errorf("a request that was not retried is logged as re-resolved: %s", log)
	}
}

// A refusal of the retry is relayed through the existing refusal, and there is
// no second re-resolve or retry.
func TestADOReresolve_SecondRefusalIsRelayed(t *testing.T) {
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	h.front.setRefuse(func(_, _ string) int { return http.StatusUnauthorized }) // every PAT refused
	rec := h.do(t, http.MethodGet, projectsGET, "", nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get(egressHeaderDetail), adoMsgRefused) {
		t.Fatalf("status = %d detail=%q, want the existing 401 refusal", rec.Code, rec.Header().Get(egressHeaderDetail))
	}
	if got := tokensOf(h.front.requests("/_apis/projects")); len(got) != 2 {
		t.Fatalf("Azure DevOps saw %v, want exactly one retry", got)
	}
	if n := h.cp.resolves(); n != 1 {
		t.Errorf("control-plane resolves = %d, want 1", n)
	}
	log := h.log()
	if strings.Count(log, logReresolved) != 1 || strings.Count(log, `"`+ruleSourceADOUpstreamRefused+`"`) != 1 {
		t.Errorf("decision log: want one re-resolved row and one refusal row: %s", log)
	}
}

// Concurrent 401s on one host make one re-resolve; within lastGoodRetry a
// refusal of the fresh header asks nothing; past it, a stale_jti naming the
// current PAT makes the control plane mint a fresh one.
func TestADOReresolve_ConcurrentRefusalsMakeOneReresolveAndArePaced(t *testing.T) {
	h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	h.front.setRefuse(refuseToken(h.old.value, http.StatusUnauthorized))
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() { codes[i] = h.do(t, http.MethodGet, projectsGET, "", nil).Code })
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status %d, want 200", i, c)
		}
	}
	if got := h.cp.staleQueries(); len(got) != 1 {
		t.Fatalf("stale re-resolves = %v, want exactly one for concurrent refusals", got)
	}

	// Azure DevOps now refuses the current PAT too: inside the pacing window the
	// proxy asks nothing and relays the refusal.
	h.front.setRefuse(func(_, _ string) int { return http.StatusUnauthorized })
	if rec := h.do(t, http.MethodGet, projectsGET, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("paced refusal: status %d, want the relayed 401", rec.Code)
	}
	if n := h.cp.resolves(); n != 1 {
		t.Fatalf("control-plane resolves inside the pacing window = %d, want 1", n)
	}

	// Past the window, the refusal of the current PAT re-resolves once more and
	// the control plane mints a fresh one, which Azure DevOps takes.
	e := h.p.inject.byHost[adoHost]
	e.reMu.Lock()
	e.staleAt = e.staleAt.Add(-lastGoodRetry)
	e.reMu.Unlock()
	h.front.setRefuse(func(_, tok string) int {
		if tok == h.old.value || tok == h.cur.value {
			return http.StatusUnauthorized
		}
		return 0
	})
	if rec := h.do(t, http.MethodGet, projectsGET, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("past the window: status %d, want 200 with a freshly minted PAT", rec.Code)
	}
	if got := h.cp.staleQueries(); len(got) != 2 || got[1] != h.cur.jti {
		t.Errorf("stale re-resolves = %v, want the second naming %s", got, h.cur.jti)
	}
	if n := h.cp.mintCount(); n != 1 || !masked(h.cp.current()) {
		t.Errorf("mints = %d, masked = %v; want one forced mint, registered for masking", n, masked(h.cp.current()))
	}
}

// A 423 on the stale re-resolve joins the re-auth hold exactly as an expiry
// re-resolve does: the request waits for the sign-in and is retried with the
// credential the hold resolves; a hold that ends answers the existing refusal.
func TestADOReresolve_423JoinsTheReauthHold(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	for _, tc := range []struct {
		name   string
		reader *fakeApprovalReader
		want   int
	}{
		{"SignedIn", &fakeApprovalReader{steps: steps(types.ApprovalPending, types.ApprovalApproved)}, http.StatusOK},
		{"Ended", &fakeApprovalReader{steps: steps(types.ApprovalCancelled)}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReresolveHarness(t, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
			h.front.setRefuse(refuseToken(h.old.value, http.StatusUnauthorized))
			h.cp.lockNext(1)
			h.p.inject.reauth, h.p.inject.approvals = newReauthCoordinator(), tc.reader
			t.Cleanup(h.p.inject.reauth.stop)

			rec := h.do(t, http.MethodGet, projectsGET, "", nil)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d. body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if q := h.cp.staleQueries(); len(q) == 0 || q[0] != h.old.jti {
				t.Errorf("the held re-resolve did not name the stale PAT: %v", q)
			}
			switch tc.want {
			case http.StatusOK:
				if got := tokensOf(h.front.requests("/_apis/projects")); len(got) != 2 || got[1] != h.cur.value {
					t.Errorf("Azure DevOps saw %v, want the retry with the held resolve's PAT", got)
				}
			default:
				if !strings.Contains(rec.Body.String(), adoSignInEndedRefusal) {
					t.Errorf("an ended hold answered %s, want the existing sign-in-ended refusal", rec.Body.String())
				}
				if n := len(h.front.requests("/_apis/projects")); n != 1 {
					t.Errorf("Azure DevOps saw %d requests, want no retry after an ended hold", n)
				}
			}
		})
	}
}

// newReresolveGitHarness is the git door with the front recording what reaches
// Azure DevOps. Both broker hosts hold old; the control plane holds the
// harness's own token, which the fake takes. oldValid registers old with the
// fake too, for tests where only the front refuses it.
func newReresolveGitHarness(t *testing.T, oldValid bool, caps ...adoscope.Capability) (*adoGitHarness, *adoFront, *staleCP, adoPAT) {
	t.Helper()
	front := &adoFront{}
	old := newADOPAT("auth-1")
	var cp *staleCP
	h := newADOGitHarnessFront(t, func(p *Proxy, token string) {
		cp = newStaleCP(t, adoPAT{value: token, jti: "auth-2"}, old.jti)
		cp.wire(p.inject, old, "dev.azure.com", "acme.visualstudio.com")
	}, front.wrap, caps...)
	if oldValid {
		h.fake.RegisterToken(old.value, adofake.ScopeCodeRead, adofake.ScopeCodeWrite)
	}
	return h, front, cp, old
}

// git: info/refs answered 401 or with a 203 sign-in page for the stale PAT
// heals, and the clone succeeds.
func TestADOReresolve_GitAdvertisementHeals(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNonAuthoritativeInfo} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, front, cp, old := newReresolveGitHarness(t, false, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
			front.setRefuse(refuseToken(old.value, status))
			h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
			if got := tokensOf(front.requests("/info/refs")); len(got) != 2 || got[0] != old.value || got[1] != h.bearer {
				t.Fatalf("info/refs carried %v, want the stale PAT then the current one", got)
			}
			if got := cp.staleQueries(); len(got) != 1 || got[0] != old.jti {
				t.Fatalf("stale re-resolves = %v, want one naming %s", got, old.jti)
			}
			log := h.finish(t)
			if strings.Count(log, logGitReresolved) != 1 || strings.Contains(log, ":upstream-") {
				t.Errorf("decision log: want one %s and no refusal row: %s", logGitReresolved, log)
			}
			noCredentialBytes(t, map[string]string{"decision log": log, "slog": h.logs.String()}, old, adoPAT{value: h.bearer})
		})
	}
}

// git: an upload-pack POST refused for the stale PAT is retried with the same
// bytes.
func TestADOReresolve_GitUploadPackIsReplayedByteIdentical(t *testing.T) {
	h, front, _, old := newReresolveGitHarness(t, true, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	front.setRefuse(func(path, tok string) int {
		if tok == old.value && strings.HasSuffix(path, "/git-upload-pack") {
			return http.StatusUnauthorized
		}
		return 0
	})
	h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	// Protocol v2 makes more than one upload-pack POST; the first is refused and retried, the rest ride the fresh header.
	sent := front.requests("/git-upload-pack")
	if len(sent) < 2 || sent[0].token != old.value || slices.Contains(tokensOf(sent[1:]), old.value) {
		t.Fatalf("upload-pack carried %v, want the stale PAT once, then only the current one", tokensOf(sent))
	}
	if len(sent[0].body) == 0 || !bytes.Equal(sent[0].body, sent[1].body) {
		t.Errorf("the retry's body differs from the first attempt's (%d vs %d bytes)", len(sent[0].body), len(sent[1].body))
	}
}

// git: a push is NEVER retried. The refusal reaches git, the header is dropped
// all the same, and the next request uses the fresh one.
func TestADOReresolve_GitPushIsNeverRetried(t *testing.T) {
	h, front, cp, old := newReresolveGitHarness(t, true, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead, adoscope.CapCodeWrite)
	dir := h.clone(t, "https://dev.azure.com/acme/proj/_git/app")
	front.setRefuse(func(path, tok string) int {
		if tok == old.value && strings.HasSuffix(path, "/git-receive-pack") {
			return http.StatusUnauthorized
		}
		return 0
	})
	out, err := h.push(t, dir, h.runBranch())
	mustBeGitRefusal(t, out, err, adoMsgRefused)
	if got := tokensOf(front.requests("/git-receive-pack")); len(got) != 1 {
		t.Fatalf("receive-pack reached Azure DevOps %d times (%v), want once", len(got), got)
	}
	if n := len(cp.staleQueries()); n != 0 {
		t.Fatalf("stale re-resolves before the next request = %d, want 0", n)
	}
	if out, err := h.git(t, "-C", dir, "ls-remote", "origin"); err != nil {
		t.Fatalf("git ls-remote after the refused push: %v\n%s", err, out)
	}
	refs := front.requests("/info/refs")
	if last := refs[len(refs)-1]; last.token != h.bearer {
		t.Errorf("the request after the refused push carried the stale PAT, want the current one")
	}
	if got := cp.staleQueries(); len(got) != 1 || got[0] != old.jti {
		t.Errorf("stale re-resolves = %v, want one naming %s", got, old.jti)
	}
	if log := h.finish(t); strings.Contains(log, logGitReresolved) {
		t.Errorf("a push was logged as re-resolved: %s", log)
	}
}

// git: an upload-pack body over adoscope.MaxBodyPeek is not buffered and not
// retried.
func TestADOReresolve_GitOversizeUploadPackIsNotRetried(t *testing.T) {
	h, front, _, old := newReresolveGitHarness(t, true, adoscope.CapProjectRead, adoscope.CapCodeRead, adoscope.CapWorkRead)
	front.setRefuse(refuseToken(old.value, http.StatusUnauthorized))
	body := bytes.Repeat([]byte("0032want 0000000000000000000000000000000000000000\n"), adoscope.MaxBodyPeek/50+1)
	req, err := http.NewRequest(http.MethodPost, h.proxy+"/wardyn/git/dev.azure.com/acme/proj/_git/app/git-upload-pack", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want git's plain-text refusal", resp.StatusCode)
	}
	if n := len(front.requests("/git-upload-pack")); n != 1 {
		t.Fatalf("the oversize upload-pack reached Azure DevOps %d times, want once", n)
	}
}

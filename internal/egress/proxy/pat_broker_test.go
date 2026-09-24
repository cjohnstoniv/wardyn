// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newPATBrokerUpstream is newGitBrokerUpstream's git_pat sibling: the SAME
// control-plane mint route + forge, answering with a git_pat credential (a token
// and the git username the forge expects) instead of a github_token.
func newPATBrokerUpstream(t *testing.T, token, username string) *gitBrokerUpstream {
	t.Helper()
	return newPATBrokerUpstreamTTL(t, token, username, time.Hour)
}

// newPATBrokerUpstreamTTL is newPATBrokerUpstream with the mint's STATED expiry
// under test.
//
// A real git_pat mint always states one — internal/broker/broker_mint_kinds.go
// mints with `ExpiresAt: time.Now().Add(ttlFor(spec))`, and ttlFor honours an
// operator's GrantSpec.TTLSeconds up to the 1h cap, so an operator can author a
// grant whose whole life is shorter than a cache margin. The fixture that
// stated NO expiry exercised only brokeredToken's unparseable-response arm
// (expiresAt == 0, cache forever), which is why the 5-minute margin bug (F120)
// survived a green cache pin.
func newPATBrokerUpstreamTTL(t *testing.T, token, username string, ttl time.Duration) *gitBrokerUpstream {
	t.Helper()
	exp := time.Now().Add(ttl).UTC().Format(time.RFC3339)
	return newBrokerUpstream(t,
		`{"kind":"git_pat","token":"`+token+`","username":"`+username+`","jti":"j","expires_at":"`+exp+`"}`)
}

// newPATBrokerProxy is newGitBrokerProxy for the git_pat lane: the per-HOST
// allowlist wired, with the control plane and the forge both reachable at the
// single upstream (HTTPS, so the mint forward and the re-origination share one
// TLS server).
func newPATBrokerProxy(t *testing.T, grants map[string]PATGrant, upstreamAddr string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	return newPATBrokerProxySpec(t, types.RunPolicySpec{}, grants, upstreamAddr)
}

// newPATBrokerProxySpec is newPATBrokerProxy with the run policy under test —
// the branch-namespace cases need git_push_any_branch.
func newPATBrokerProxySpec(t *testing.T, spec types.RunPolicySpec, grants map[string]PATGrant, upstreamAddr string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(spec),
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		PATGrants:       grants,
	})
	return p, buf
}

// TestPATBrokerClonesGrantedHost is the git_pat lane's counterpart to
// TestGitBrokerClonesGrantedRepo, and until it existed NOTHING exercised
// handlePATBroker/patToken end to end — only the path split and the empty-grant
// map had tests, so every byte between "the host is granted" and "the forge
// answered" was unpinned.
//
// The contract: a granted host's info/refs is re-originated to the forge over
// HTTPS with Basic <username>:<token> built from the SERVER-SIDE mint (the
// sandbox's smuggled Authorization stripped first), the path and query
// preserved, the response streamed back verbatim, and a brokered:git-pat allow
// row in the decision log. The credential is proxy-side throughout: the sandbox
// spoke cleartext HTTP to its own sidecar and never held the PAT.
//
// This lane had no end-to-end test until this one, and writing it found the
// lane dead: handlePATBroker handed validGitRest the FULL upstream path with
// its leading slash ("/org/repo.git/info/refs"), but validGitRest is the GitHub
// lane's helper and matches only the bare smart-HTTP tail ("info/refs") —
// parseGitBrokerPath strips <org>/<repo> for it, while parsePATBrokerPath
// cannot, because a forge repo path is arbitrarily deep (gitlab subgroups,
// "o/p/_git/r" on Azure DevOps). Every well-formed clone of a granted host 403'd
// before the mint, and agent-run-lib.sh's insteadOf rewrite produces exactly
// that shape. It failed CLOSED — the PAT was simply never brokered — and the
// lane now takes the verb from the last segment, with refs discovery's two
// segments as the one exception.
func TestPATBrokerClonesGrantedHost(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "git-pack-data" {
		t.Fatalf("info/refs body = %q, want the upstream response streamed verbatim", rec.Body.String())
	}
	// The mint's own username wins over the grant's and over the "pat" fallback,
	// so a GitLab grant authenticates as oauth2 — an EMPTY username is the one
	// value that fails on every forge and must never be sent.
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:T"))
	if up.gitAuth != wantAuth {
		t.Fatalf("upstream Authorization = %q, want %q (Basic oauth2:T; the smuggled bearer stripped)", up.gitAuth, wantAuth)
	}
	if up.gitPath != "/org/repo.git/info/refs" {
		t.Fatalf("upstream path = %q, want the host segment stripped and the rest preserved", up.gitPath)
	}
	if up.gitQuery != "service=git-upload-pack" {
		t.Fatalf("upstream query = %q, want service preserved", up.gitQuery)
	}
	if up.mintCalls != 1 {
		t.Fatalf("mintCalls = %d, want 1 (the PAT is minted server-side and cached per grant)", up.mintCalls)
	}
	// F014: the row must name the FORGE it dialled. Logged through
	// emitLocalDecision it named the CONTROL PLANE, so a clone of gitlab.com and
	// a credential mint were the same row, and two granted forges could not be
	// told apart at all — in the one stream an egress review reads.
	d := lastDecision(t, sink)
	if d.RuleSource != ruleSourcePAT || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want a %s allow row", d, ruleSourcePAT)
	}
	if d.Request.Host != "gitlab.com" || d.Request.Port != 443 {
		t.Fatalf("decision row host:port = %s:%d, want gitlab.com:443 (the forge this request actually reached)",
			d.Request.Host, d.Request.Port)
	}
}

// The path split IS the authorization surface: whatever comes out of it is
// looked up in the per-host allowlist, and anything that is not an exact
// granted host 403s before an upstream URL is formed. So the cases that matter
// are the ones a hostile path could produce.
func TestParsePATBrokerPath(t *testing.T) {
	for _, tc := range []struct {
		in         string
		host, rest string
		ok         bool
	}{
		{"/wardyn/git/gitlab.com/org/repo.git/info/refs", "gitlab.com", "/org/repo.git/info/refs", true},
		{"/wardyn/git/dev.azure.com/o/p/_git/r", "dev.azure.com", "/o/p/_git/r", true},
		// Host matching is case-insensitive, so the key is lowered once here
		// rather than at every lookup.
		{"/wardyn/git/GitLab.COM/o/r", "gitlab.com", "/o/r", true},
		// Not the broker prefix at all.
		{"/wardyn/gh/org/repo", "", "", false},
		{"/wardyn/git/", "", "", false},
		// A host segment with no trailing path: there is nothing to forward.
		{"/wardyn/git/gitlab.com", "", "", false},
	} {
		host, rest, ok := parsePATBrokerPath(tc.in)
		if ok != tc.ok || host != tc.host || rest != tc.rest {
			t.Errorf("parsePATBrokerPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, host, rest, ok, tc.host, tc.rest, tc.ok)
		}
	}
}

// A traversal must not reach the network. It does not need its own rejection
// branch — it simply cannot BE a granted host — but that is a property worth
// pinning, because someone could later "fix" the parse to be more permissive.
func TestParsePATBrokerPath_TraversalIsNotAGrantedHost(t *testing.T) {
	granted := map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}
	for _, in := range []string{
		"/wardyn/git/../gh/org/repo",
		"/wardyn/git/..%2f..%2fetc/passwd",
		"/wardyn/git/evil.example.com/o/r",
		"/wardyn/git/gitlab.com.evil.example.com/o/r",
		// userinfo smuggled into the host segment
		"/wardyn/git/user@evil.example.com/o/r",
	} {
		host, _, ok := parsePATBrokerPath(in)
		if !ok {
			continue // rejected outright, fine
		}
		if _, isGranted := granted[host]; isGranted {
			t.Errorf("parsePATBrokerPath(%q) produced the GRANTED host %q — a path that is not a plain granted host must never reach the allowlist", in, host)
		}
	}
}

// The lane is off unless dispatch populates it, and off means the route 403s
// rather than falling back to anything.
func TestPATGrants_EmptyMeansNoHostBrokered(t *testing.T) {
	p := &Proxy{}
	if len(p.patGrants) != 0 {
		t.Fatal("a proxy built with no PAT grants must broker nothing")
	}
	if _, ok := p.patGrants["gitlab.com"]; ok {
		t.Error("an ungranted host resolved a grant")
	}
}

// TestPATBrokerReportsH2MismatchNotDialFailed is #382's PAT-broker case,
// TestGitBrokerReportsH2MismatchNotDialFailed's sibling: a forge answering
// unnegotiated HTTP/2 on a clone must classify as
// builtin:upstream-protocol-mismatch with a 400, not the generic
// builtin:dial-failed 502 this lane gave before roundTripUpstream's error arm
// called refuseH2Mismatch.
func TestPATBrokerReportsH2MismatchNotDialFailed(t *testing.T) {
	mintUp := newPATBrokerUpstream(t, "T", "oauth2")
	forgeAddr := startH2MismatchPeer(t)
	grantID := uuid.New()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            splitDial(upstreamAddr(mintUp.srv), forgeAddr),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		PATGrants:       map[string]PATGrant{"gitlab.com": {GrantID: grantID}},
	})

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := denyBody(rec); !strings.Contains(got, "peer answered HTTP/2") {
		t.Errorf("body = %q, want the h2-mismatch sentence", got)
	}
	d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch)
	if d.Via != viaDirect {
		t.Errorf("via = %q, want %q", d.Via, viaDirect)
	}
}

// F085: the smart-HTTP verb check reads the DECODED path, so a '#' (%23) or '?'
// (%3F) inside the sandbox-supplied rest used to satisfy the "/info/refs" suffix
// and then re-split the concatenated upstream URL — the brokered PAT delivered
// to an arbitrary path on the granted forge (the forge's REST API included),
// which is exactly what handlePATBroker's own comment forbids and what the
// GitHub-lane sibling prevents by building the URL from validated pieces.
//
// The pin is the credential, not the status code: a smuggled shape must reach
// NEITHER the mint nor the forge.
func TestPATBrokerRejectsPathSmuggle(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
	}{
		{"fragment", http.MethodGet, "/wardyn/git/gitlab.com/api/v4/user%23/info/refs?service=git-upload-pack"},
		{"query", http.MethodGet, "/wardyn/git/gitlab.com/api/v4/user%3Fz/info/refs?service=git-upload-pack"},
		{"postFragment", http.MethodPost, "/wardyn/git/gitlab.com/api/v4/user%23/git-upload-pack"},
		{"traversal", http.MethodGet, "/wardyn/git/gitlab.com/api/v4/user/..%2f..%2finfo/refs?service=git-upload-pack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newPATBrokerUpstream(t, "T", "oauth2")
			p, _ := newPATBrokerProxy(t,
				map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, nil))

			if up.gitHits != 0 {
				t.Errorf("forge saw %d request(s) at path %q — a re-split path carried the brokered PAT off the smart-HTTP surface",
					up.gitHits, up.gitPath)
			}
			if up.mintCalls != 0 {
				t.Errorf("mintCalls = %d, want 0 (a path the verb check cannot vouch for must be denied BEFORE the mint)", up.mintCalls)
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

// The same fix must not narrow the lane below the forge paths it exists for: an
// Azure DevOps project name may contain a space, and the rebuilt URL escapes it
// rather than refusing it.
func TestPATBrokerForwardsOrdinaryForgePath(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, _ := newPATBrokerProxy(t,
		map[string]PATGrant{"dev.azure.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/dev.azure.com/org/my%20project/_git/repo/info/refs?service=git-upload-pack", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if up.gitPath != "/org/my project/_git/repo/info/refs" {
		t.Fatalf("upstream path = %q, want the space-bearing project preserved", up.gitPath)
	}
	if up.gitQuery != "service=git-upload-pack" {
		t.Fatalf("upstream query = %q, want service preserved", up.gitQuery)
	}
}

// patApprovalUpstream is gitBrokerApprovalUpstream's git_pat sibling: the first
// mint 409s with an approval_id, the approval polls PENDING then APPROVED, and
// the re-mint answers a git_pat credential. Any other path is the forge.
type patApprovalUpstream struct {
	srv        *httptest.Server
	approvalID uuid.UUID

	mu        sync.Mutex
	mintCalls int
	pollCalls int
	gitAuth   string
	gitHits   int
}

func newPATApprovalUpstream(t *testing.T, token string, approvalID uuid.UUID, approvePollsAfter int) *patApprovalUpstream {
	t.Helper()
	u := &patApprovalUpstream{approvalID: approvalID}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/internal/credentials/mint":
			u.mintCalls++
			w.Header().Set("Content-Type", "application/json")
			if u.mintCalls == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"pending","approval_id":"`+u.approvalID.String()+`"}`)
				return
			}
			_, _ = io.WriteString(w, `{"kind":"git_pat","token":"`+token+`","username":"oauth2"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/"):
			u.pollCalls++
			state := "PENDING"
			if u.pollCalls > approvePollsAfter {
				state = "APPROVED"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"`+u.approvalID.String()+`","state":"`+state+`"}`)
		default:
			u.gitHits++
			u.gitAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, "git-pack-data")
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// F120: ONE clone is ONE mint on the git_pat lane too.
//
// patToken used to call the control-plane mint route on every sub-request, and
// a clone is two of them (GET info/refs, then POST git-upload-pack). An
// approval-gated git_pat grant is SINGLE-USE, so the second mint 409s
// ErrAlreadyMinted and the clone dies half-way — the default posture
// (WARDYN_GIT_PAT_BROKER=on) for every ADO/GitLab run. The GitHub lane has
// carried the per-grant cache + single-flight for exactly this reason since it
// shipped; this pins that the sibling lane now shares it.
func TestPATBrokerCachesTheMintAcrossOneClone(t *testing.T) {
	up := newPATBrokerUpstream(t, "T", "oauth2")
	p, _ := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	for _, step := range []struct {
		method, path string
		body         io.Reader
	}{
		{http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil},
		{http.MethodPost, "/wardyn/git/gitlab.com/org/repo.git/git-upload-pack", strings.NewReader("0000")},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, step.method, step.path, step.body))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s -> %d body=%q", step.method, step.path, rec.Code, rec.Body.String())
		}
	}
	if up.mintCalls != 1 {
		t.Fatalf("control-plane mint calls for ONE clone = %d, want 1 — a single-use (approval-gated) git_pat grant 409s ErrAlreadyMinted on the second", up.mintCalls)
	}
}

// TestPATBrokerCachesAShortTTLMintAcrossOneClone is the same contract for the
// grant shape the cache used to fail on outright (F120 fix-up).
//
// The freshness margin was injectRefreshMargin (5m), sized for a rotating
// INJECTED credential. A git_pat grant states a real expiry and an operator may
// author ttl_seconds as low as they like, so any grant with ttl <= 5m was born
// INSIDE the margin: `time.Now().Before(exp - 5m)` was false on the very next
// sub-request, the second half of one clone re-minted, and a single-use grant
// 409'd ErrAlreadyMinted. Two mints for one clone is exactly the failure this
// lane's cache exists to prevent.
func TestPATBrokerCachesAShortTTLMintAcrossOneClone(t *testing.T) {
	for _, ttl := range []time.Duration{2 * time.Minute, 45 * time.Second, time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			up := newPATBrokerUpstreamTTL(t, "T", "oauth2", ttl)
			p, _ := newPATBrokerProxy(t,
				map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

			for _, step := range []struct {
				method, path string
				body         io.Reader
			}{
				{http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil},
				{http.MethodPost, "/wardyn/git/gitlab.com/org/repo.git/git-upload-pack", strings.NewReader("0000")},
			} {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, step.method, step.path, step.body))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s -> %d body=%q", step.method, step.path, rec.Code, rec.Body.String())
				}
			}
			if up.mintCalls != 1 {
				t.Fatalf("ttl=%s: control-plane mint calls for ONE clone = %d, want 1 — a grant whose "+
					"whole life is shorter than the cache margin must still serve one clone from one mint",
					ttl, up.mintCalls)
			}
		})
	}
}

// F120: a PENDING credential approval must be waited out, not returned as a 502.
//
// The broker mints server-side with no caller able to retry, so the GitHub lane
// polls the same approval itself (W23-S1-1 / W19-W19a-1). patToken returned an
// error on any non-200, which made the FIRST clone against an approval-gated
// git_pat grant fail before a human could possibly have approved it.
func TestPATBrokerWaitsOutAPendingApproval(t *testing.T) {
	orig := gitApprovalPollInterval
	gitApprovalPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { gitApprovalPollInterval = orig })

	approvalID := uuid.New()
	up := newPATApprovalUpstream(t, "pat-after-approval", approvalID, 2 /* PENDING twice, then APPROVED */)
	p, _ := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200 once the approval clears", rec.Code, rec.Body.String())
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:pat-after-approval"))
	if up.gitAuth != wantAuth {
		t.Fatalf("upstream Authorization = %q, want %q (the post-approval PAT)", up.gitAuth, wantAuth)
	}
	if up.mintCalls != 2 {
		t.Fatalf("mintCalls = %d, want 2 (initial 409 pending + re-mint after approval)", up.mintCalls)
	}
	if up.pollCalls < 3 {
		t.Fatalf("pollCalls = %d, want >= 3 (two PENDING + the APPROVED that releases it)", up.pollCalls)
	}
}

// TestPATBranchNSEnforcedEnv: the opt-IN env is the App-lane switch's mirror —
// OFF when unset (so nothing a 0.7.1 deployment pushed changes), off on an
// explicit disable word, on for the enable words, and FAIL CLOSED (on) for
// garbage, because a typo must not undo a control the operator turned on
// deliberately.
func TestPATBranchNSEnforcedEnv(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"", false},    // unset => the git_pat lane is unconfined, as in 0.7.1
		{"   ", false}, // whitespace-only is still "unset"
		{"off", false}, {"0", false}, {"false", false}, {"disabled", false}, {"none", false},
		{"1", true}, {"true", true}, {"on", true}, {"enforce", true},
		{"maybe", true}, // garbage => enforce, never silently off
	} {
		t.Run("val="+tc.val, func(t *testing.T) {
			t.Setenv(envEnforcePATBranchNS, tc.val)
			if got := PATBranchNSEnforced(); got != tc.want {
				t.Fatalf("PATBranchNSEnforced(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestPATBrokerPushUnconfinedByDefault is the upgrade pin: with the switch
// unset, a git_pat push to ANY ref is forwarded byte-for-byte and audited
// exactly as it is in 0.7.1 — one brokered:git-pat allow, no branch-ns row.
// The confinement is opt-in, and "opt-in" has to mean the default deployment
// sees no change at all.
func TestPATBrokerPushUnconfinedByDefault(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "") // never inherit an operator's setting
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the git_pat lane is unconfined by default); body=%q", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != body {
		t.Fatalf("upstream body = %q, want the push streamed through unchanged", up.gitBody)
	}
	d := lastDecision(t, sink)
	if d.RuleSource != ruleSourcePAT || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want the ordinary %s allow", d, ruleSourcePAT)
	}
	if strings.Contains(sink.String(), "branch-ns") {
		t.Fatalf("decision log = %q, want no branch-namespace row while the switch is off", sink.String())
	}
}

// TestPATBrokerDeniesOutOfNamespacePushWhenEnforcing: opted in, the git_pat lane
// refuses an out-of-namespace push the way the App lane does — the SAME
// rule_source (one rule, two brokers), the same 403 words, before the PAT is
// minted and before a byte reaches the forge. The decision row names the FORGE,
// not github.com: that is the one thing the two lanes must NOT share.
func TestPATBrokerDeniesOutOfNamespacePushWhenEnforcing(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "on")
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refs/heads/main") ||
		!strings.Contains(rec.Body.String(), "refs/heads/wardyn/"+p.runID.String()+"/") {
		t.Fatalf("body = %q, want the App lane's words: the offending ref and the allowed namespace", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PAT") && strings.Contains(rec.Body.String(), "Basic") {
		t.Fatal("the brokered PAT leaked into the denial body")
	}
	if up.gitHits != 0 || up.mintCalls != 0 {
		t.Fatalf("upstream hits=%d mints=%d, want 0/0 — a refused push must not mint the PAT", up.gitHits, up.mintCalls)
	}
	d := lastDecision(t, sink)
	if d.RuleSource != ruleSourceGitRef || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want a %s deny (the App lane's value, reused)", d, ruleSourceGitRef)
	}
	if d.Request.Host != "gitlab.com" || d.Request.Port != 443 {
		t.Fatalf("decision row host:port = %s:%d, want the forge (gitlab.com:443)", d.Request.Host, d.Request.Port)
	}
}

// TestPATBrokerForwardsInNamespacePushWhenEnforcing: an in-namespace push is
// forwarded with the command section re-prepended byte-for-byte ahead of the
// still-streaming pack, and keeps the ordinary brokered:git-pat allow.
func TestPATBrokerForwardsInNamespacePushWhenEnforcing(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "1")
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	ref := "refs/heads/wardyn/" + p.runID.String() + "/feature"
	body := pkt(someOID+" "+otherOID+" "+ref+firstCaps) + "0000" +
		"PACK\x00\x02\x00\x00\x00\x01\xff\xfe\x00 binary"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != body {
		t.Fatalf("upstream body = %q, want the push forwarded byte-for-byte (%q)", up.gitBody, body)
	}
	if strings.Contains(sink.String(), ruleSourceGitNSOff) {
		t.Fatalf("decision log = %q, want NO %s row for a push the parser cleared", sink.String(), ruleSourceGitNSOff)
	}
}

// TestPATBrokerFetchUnaffectedWhenEnforcing: confinement is a PUSH rule. Even
// opted in, refs discovery and upload-pack are pure streaming — nothing
// buffered, nothing refused, no new row.
func TestPATBrokerFetchUnaffectedWhenEnforcing(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "on")
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	d := lastDecision(t, sink)
	if d.RuleSource != ruleSourcePAT || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want the ordinary %s allow for a fetch", d, ruleSourcePAT)
	}
}

// TestPATBrokerRejectsEncodedPushWhenEnforcing: a content-encoded push body
// cannot be ref-checked on this lane either, so it is refused (415) rather than
// waved through unparsed — the same silent-bypass rule, the same words.
func TestPATBrokerRejectsEncodedPushWhenEnforcing(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "on")
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxy(t,
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	req := mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader("gzipped-bytes"))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if up.gitHits != 0 {
		t.Fatal("the forge was reached with an unparseable push body")
	}
	if d := lastDecision(t, sink); d.RuleSource != ruleSourceGitEnc {
		t.Fatalf("decision = %+v, want a %s deny", d, ruleSourceGitEnc)
	}
}

// TestPATBrokerPushPolicyOptOut: the run's own git_push_any_branch opts out of
// the confinement on this lane too, and the allow row SAYS so
// (brokered:git:branch-ns-off) — which is the only way a PAT push carries that
// marker, since a push on a proxy whose switch is off was never confined and
// keeps the ordinary brokered:git-pat allow.
func TestPATBrokerPushPolicyOptOut(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "on")
	up := newPATBrokerUpstream(t, "PAT", "oauth2")
	p, sink := newPATBrokerProxySpec(t, types.RunPolicySpec{GitPushAnyBranch: true},
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))

	body := pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps) + "0000" + "PACKDATA"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the run opted out); body=%q", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != body {
		t.Fatalf("upstream body = %q, want the unenforced push streamed through unchanged", up.gitBody)
	}
	if d := lastDecision(t, sink); d.RuleSource != ruleSourceGitNSOff {
		t.Fatalf("decision = %+v, want the %s allow so the opt-out is visible after the fact", d, ruleSourceGitNSOff)
	}
}

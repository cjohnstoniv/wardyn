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
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
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

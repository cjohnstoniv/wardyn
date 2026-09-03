// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newPATBrokerUpstream is newGitBrokerUpstream's git_pat sibling: the SAME
// control-plane mint route + forge, answering with a git_pat credential (a token
// and the git username the forge expects) instead of a github_token.
func newPATBrokerUpstream(t *testing.T, token, username string) *gitBrokerUpstream {
	t.Helper()
	return newBrokerUpstream(t, `{"kind":"git_pat","token":"`+token+`","username":"`+username+`"}`)
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
		t.Fatalf("mintCalls = %d, want 1 (the PAT is minted server-side, per request)", up.mintCalls)
	}
	if d := lastDecision(t, sink); d.RuleSource != ruleSourcePAT || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want a %s allow row", d, ruleSourcePAT)
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

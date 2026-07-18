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

// gitBrokerUpstream is one TLS server standing in for BOTH the control-plane mint
// route and github.com — redirectDial funnels every dial to a single addr, so the
// server routes by path: /api/v1/internal/credentials/mint mints a token (counting
// calls, to prove the per-grant cache), everything else is the git-smart-HTTP
// upstream (capturing the injected Authorization + rewritten path).
type gitBrokerUpstream struct {
	srv       *httptest.Server
	mu        sync.Mutex
	mintCalls int
	gitAuth   string // Authorization the upstream github request carried
	gitPath   string
	gitQuery  string
	gitProto  string
	gitHits   int
}

func newGitBrokerUpstream(t *testing.T, token string) *gitBrokerUpstream {
	t.Helper()
	u := &gitBrokerUpstream{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			u.mintCalls++
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"`+token+`","username":"x-access-token","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		u.gitHits++
		u.gitAuth = r.Header.Get("Authorization")
		u.gitPath = r.URL.Path
		u.gitQuery = r.URL.RawQuery
		u.gitProto = r.Header.Get("Git-Protocol")
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "git-pack-data")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// newGitBrokerProxy builds a Proxy with the git-broker allowlist wired and both the
// control plane and github reachable at the single upstream (HTTPS so the mint
// forward and the github re-origination share one TLS server).
func newGitBrokerProxy(t *testing.T, grants map[string]uuid.UUID, upstreamAddr string) (*Proxy, *bytes.Buffer) {
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
		GitGrants:       grants,
	})
	return p, buf
}

// TestGitBrokerClonesGrantedRepo: a granted repo's info/refs is re-originated to
// github with a Basic x-access-token:<token> auth (the sandbox's smuggled
// Authorization stripped), the path/query/Git-Protocol preserved, and the response
// streamed back. A following git-upload-pack reuses the cached token (one mint).
func TestGitBrokerClonesGrantedRepo(t *testing.T) {
	grantID := uuid.New()
	up := newGitBrokerUpstream(t, "gh-inst-token")
	// The allowlist key is the canonical lowercased "<org>/<repo>".
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": grantID}, upstreamAddr(up.srv))

	// info/refs (note mixed-case in the request path — github is case-insensitive).
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/octocat/Hello-World.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	req.Header.Set("Git-Protocol", "version=2")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("info/refs status = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "git-pack-data" {
		t.Fatalf("info/refs body = %q, want streamed git-pack-data", rec.Body.String())
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:gh-inst-token"))
	if up.gitAuth != wantAuth {
		t.Fatalf("upstream Authorization = %q, want %q (Basic x-access-token; smuggled stripped)", up.gitAuth, wantAuth)
	}
	if up.gitAuth == "Bearer SANDBOX-SMUGGLED" {
		t.Fatal("sandbox-smuggled Authorization reached github")
	}
	if up.gitPath != "/octocat/hello-world.git/info/refs" {
		t.Fatalf("upstream path = %q", up.gitPath)
	}
	if up.gitQuery != "service=git-upload-pack" {
		t.Fatalf("upstream query = %q, want service preserved", up.gitQuery)
	}
	if up.gitProto != "version=2" {
		t.Fatalf("upstream Git-Protocol = %q, want version=2 preserved", up.gitProto)
	}

	// The following git-upload-pack POST must reuse the cached token — one mint for
	// the whole clone (mandatory for single-use approval-gated grants).
	rec2 := httptest.NewRecorder()
	req2 := mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/Hello-World.git/git-upload-pack", strings.NewReader("0011command=fetch"))
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("git-upload-pack status = %d body=%q", rec2.Code, rec2.Body.String())
	}
	if up.mintCalls != 1 {
		t.Fatalf("mintCalls = %d, want 1 (token cached across info/refs + upload-pack)", up.mintCalls)
	}
}

// TestGitBrokerDeniesUngrantedRepo: a repo not in the allowlist is 403'd BEFORE any
// github URL is formed or token minted (repo is the unit of trust).
func TestGitBrokerDeniesUngrantedRepo(t *testing.T) {
	up := newGitBrokerUpstream(t, "unused")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/attacker/evil.git/info/refs?service=git-upload-pack", nil)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted repo status = %d, want 403", rec.Code)
	}
	if up.gitHits != 0 {
		t.Fatalf("github upstream was hit %d times for an ungranted repo", up.gitHits)
	}
	if up.mintCalls != 0 {
		t.Fatalf("mint was called %d times for an ungranted repo (must not mint)", up.mintCalls)
	}
}

// TestGitBrokerRejectsBadRequests: traversal / short / unknown-verb / bad-service
// requests never reach github, whether they 403 (matched-repo, bad rest) or 404
// (malformed path that doesn't even parse to a key).
func TestGitBrokerRejectsBadRequests(t *testing.T) {
	grants := map[string]uuid.UUID{"octocat/hello-world": uuid.New()}
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"traversal", http.MethodGet, "/wardyn/gh/octocat/../secret.git/info/refs?service=git-upload-pack"},
		{"too-short", http.MethodGet, "/wardyn/gh/onlyorg"},
		{"bad-service", http.MethodGet, "/wardyn/gh/octocat/Hello-World.git/info/refs?service=evil"},
		{"unknown-verb", http.MethodPost, "/wardyn/gh/octocat/Hello-World.git/git-evil-pack"},
		{"info-refs-wrong-method", http.MethodPost, "/wardyn/gh/octocat/Hello-World.git/info/refs?service=git-upload-pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "unused")
			p, _ := newGitBrokerProxy(t, grants, upstreamAddr(up.srv))
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.path, nil))
			if rec.Code == http.StatusOK {
				t.Fatalf("%s: status = 200, want a rejection", tc.name)
			}
			if up.gitHits != 0 {
				t.Fatalf("%s: github upstream was hit for a rejected request", tc.name)
			}
		})
	}
}

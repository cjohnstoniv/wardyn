// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ghCLIToken = "ghs_cli_minted"

// ghUpstream is the control plane's mint route and api.github.com behind one
// TLS server: it answers every mint with ghCLIToken and records every other
// request.
type ghUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	mints int
	seen  []string // "METHOD path auth"
}

func newGHUpstream(t *testing.T) *ghUpstream {
	t.Helper()
	u := &ghUpstream{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			u.mints++
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			_, _ = io.WriteString(w, `{"kind":"github_token","token":"`+ghCLIToken+`","expires_at":"`+exp+`"}`)
			return
		}
		u.seen = append(u.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *ghUpstream) counts() (mints int, seen []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.mints, append([]string(nil), u.seen...)
}

// newGHProxy is a proxy whose run attaches a GitHub CLI row granting
// issues_read over acme/app. The policy allows the door's host, as dispatch
// writes it (confineBrokerEgress).
func newGHProxy(t *testing.T, up *ghUpstream) (*Proxy, *GitHubAPIGrant) {
	t.Helper()
	return newGHProxySpec(t, up, types.RunPolicySpec{AllowedDomains: []string{"api.github.com:443"}})
}

func newGHProxySpec(t *testing.T, up *ghUpstream, spec types.RunPolicySpec) (*Proxy, *GitHubAPIGrant) {
	t.Helper()
	g := &GitHubAPIGrant{Capabilities: []ghscope.Capability{ghscope.CapIssuesRead}, Repos: []string{"acme/app"},
		Grants: map[string]uuid.UUID{"acme": uuid.New()}}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(spec),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(up.srv)),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		GitHubAPI:       g,
	})
	return p, g
}

// ghTunnelRequest is a request as the terminated tunnel reads it, carrying the
// sandbox's inert token.
func ghTunnelRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token wardyn-proxy-injects-github-token\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func ghRefused(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body["wardyn"] != gitHubAPIRefused {
		t.Fatalf("status %d body %s, want 403 %s", rec.Code, rec.Body, gitHubAPIRefused)
	}
}

// The gate holds on both lanes: an admitted request through the terminated
// tunnel reaches GitHub with the minted token in place of the sandbox's, and
// the same request as an absolute-form request line on the plain lane is
// refused before any mint or any byte upstream.
func TestGitHubAPI_GateOnBothLanes(t *testing.T) {
	t.Run("tunnel", func(t *testing.T) {
		up := newGHUpstream(t)
		p, _ := newGHProxy(t, up)
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues?state=open"), ghscope.APIHost, 443)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body)
		}
		mints, seen := up.counts()
		if want := "GET /repos/acme/app/issues token " + ghCLIToken; mints != 1 || len(seen) != 1 || seen[0] != want {
			t.Fatalf("mints %d seen %q, want 1 mint and %q", mints, seen, want)
		}
	})
	t.Run("plain", func(t *testing.T) {
		up := newGHUpstream(t)
		p, _ := newGHProxy(t, up)
		rec := httptest.NewRecorder()
		p.servePlain(rec, mustProxyReq(t, http.MethodGet, "https://api.github.com/repos/acme/app/issues"))
		ghRefused(t, rec)
		if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
			t.Fatalf("plain lane reached upstream: mints %d seen %q", mints, seen)
		}
	})
}

// A request naming a repository or owner outside the run's own, a capability
// the row does not grant, or an area no run is granted is refused before the
// mint.
func TestGitHubAPI_RepoOutsideRunRefused(t *testing.T) {
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/repos/other/repo/issues"},
		{http.MethodGet, "/repos/acme/other/issues"},
		{http.MethodGet, "/repos/other/repo"},
		{http.MethodGet, "/orgs/other/repos"},
		{http.MethodPost, "/repos/acme/app/issues"},
		{http.MethodPost, "/graphql"},
		{http.MethodGet, "/search/issues?q=x"},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			up := newGHUpstream(t)
			p, _ := newGHProxy(t, up)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, tc.method, tc.target), ghscope.APIHost, 443)
			ghRefused(t, rec)
			if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
				t.Fatalf("refused request spent: mints %d seen %q", mints, seen)
			}
		})
	}
}

// With code_read granted over the run's own repository, a contents read passes
// and a dot segment carrying a path parameter in its tail is refused before
// the mint, in every spelling.
func TestGitHubAPI_DotSegmentWithPathParameterRefused(t *testing.T) {
	grant := func(t *testing.T) (*Proxy, *ghUpstream) {
		up := newGHUpstream(t)
		p, g := newGHProxy(t, up)
		g.Capabilities = append(g.Capabilities, ghscope.CapCodeRead)
		return p, up
	}
	t.Run("plain semicolon is admitted", func(t *testing.T) {
		p, up := grant(t)
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/contents/a;b"), ghscope.APIHost, 443)
		if mints, seen := up.counts(); rec.Code != http.StatusOK || mints != 1 || len(seen) != 1 {
			t.Fatalf("status %d mints %d seen %q, want 200 and one forwarded request", rec.Code, mints, seen)
		}
	})
	for _, target := range []string{
		"/repos/acme/app/contents/..;/..;/other",
		"/repos/acme/app/contents/%2e%2e%3b/other",
		"/repos/acme/app/contents/..%3B/other",
	} {
		t.Run(target, func(t *testing.T) {
			p, up := grant(t)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, target), ghscope.APIHost, 443)
			ghRefused(t, rec)
			if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
				t.Fatalf("refused request spent: mints %d seen %q", mints, seen)
			}
		})
	}
}

// The sandbox cannot mint a CLI grant through the raw mint relay, and the
// proxy hands its cached token back to GitHub when it stops.
func TestGitHubAPI_MintRelayAndShutdownRevoke(t *testing.T) {
	up := newGHUpstream(t)
	p, g := newGHProxy(t, up)
	body, _ := json.Marshal(map[string]string{"grant_id": g.Grants["acme"].String()})
	if !p.isBrokeredGitGrant(body) {
		t.Fatal("the mint relay would serve the GitHub CLI grant to the sandbox")
	}
	p.serveMITMRequest(httptest.NewRecorder(), ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues"), ghscope.APIHost, 443)
	p.revokeGitHubAPITokens(t.Context())
	_, seen := up.counts()
	if want := "DELETE /installation/token token " + ghCLIToken; len(seen) != 2 || seen[1] != want {
		t.Fatalf("seen %q, want the request then %q", seen, want)
	}
}

// Shutdown hands the cached token back before it stops serving: the runtime's
// stop timeout may kill the process while http.Shutdown still waits on open
// sandbox connections.
func TestGitHubAPI_ShutdownRevokesFirst(t *testing.T) {
	up := newGHUpstream(t)
	p, _ := newGHProxy(t, up)
	p.serveMITMRequest(httptest.NewRecorder(), ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues"), ghscope.APIHost, 443)
	srv := &Server{proxy: p, http: &http.Server{}, sink: p.sink}
	seenAtShutdown := make(chan []string, 1)
	srv.http.RegisterOnShutdown(func() { _, seen := up.counts(); seenAtShutdown <- seen })
	_ = srv.Shutdown(t.Context())
	seen := <-seenAtShutdown
	if want := "DELETE /installation/token token " + ghCLIToken; len(seen) != 2 || seen[1] != want {
		t.Fatalf("when serving stopped the upstream had seen %q, want the request then %q", seen, want)
	}
}

// A refusal says why in a sentence: GraphQL names the REST way that works.
func TestGitHubAPI_RefusalSentences(t *testing.T) {
	for _, tc := range []struct{ method, target, want string }{
		{http.MethodPost, "/graphql", gitHubAPIRESTOnly},
		{http.MethodGet, "/repos/acme/other/issues", gitHubAPIRefusedPrefix + "it names a repository or an owner outside this run's own."},
		{http.MethodPost, "/repos/acme/app/issues", gitHubAPIRefusedPrefix + `this run's GitHub CLI is not granted "` + ghscope.Label(ghscope.CapIssuesWrite) + `".`},
		{http.MethodGet, "/search/issues", "Wardyn refused this GitHub API request. " + ghscope.Label(ghscope.CapDeniedSearch) + "."},
	} {
		up := newGHUpstream(t)
		p, _ := newGHProxy(t, up)
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, ghTunnelRequest(t, tc.method, tc.target), ghscope.APIHost, 443)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["message"] != tc.want {
			t.Errorf("%s %s: message %q, want %q", tc.method, tc.target, body["message"], tc.want)
		}
	}
}

// A CONNECT to the door's host on any port the door does not terminate is
// refused, even where the policy would let a blind tunnel through.
func TestGitHubAPI_OtherPortTunnelRefused(t *testing.T) {
	up := newGHUpstream(t)
	p, _ := newGHProxySpec(t, up, types.RunPolicySpec{AllowAllEgress: true})
	req := httptest.NewRequest(http.MethodConnect, "api.github.com:8443", nil)
	req.Host = "api.github.com:8443"
	rec := httptest.NewRecorder()
	p.handleConnect(rec, req)
	ghRefused(t, rec)
}

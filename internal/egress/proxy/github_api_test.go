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
	g := &GitHubAPIGrant{Capabilities: []ghscope.Capability{ghscope.CapIssuesRead}, Repos: []string{"acme/app"},
		Grants: map[string]uuid.UUID{"acme": uuid.New()}}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"api.github.com:443"}}),
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

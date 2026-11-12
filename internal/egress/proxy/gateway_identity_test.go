// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	gwidHost     = "gw.corp.test"
	gwidOther    = "other.corp.test"
	gwidAudience = "https://gw.corp.test/wardyn"
	gwidForged   = "sandbox-forged-value"
	gwidCallID   = "sandbox-call-id"
)

func gwidToken(aud any, exp time.Time, n int64) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "jti": fmt.Sprintf("jti-%d-%s", n, uuid.NewString())})
	// An ES256-sized signature, its own for each token. The leading 0xff makes it start
	// "_", so no test's benign answer ends in the first byte of a random signature.
	signature := sha512.Sum512(payload)
	signature[0] = 0xff
	return enc([]byte(`{"alg":"ES256"}`)) + "." + enc(payload) + "." + enc(signature[:])
}

// gwidUpstream records every request, and by default echoes all its headers back in
// its own headers and body, gzipped when the request asked for gzip.
type gwidUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	seen    []*http.Request
	respond func(http.ResponseWriter, *http.Request)
}

func gwidEcho(w http.ResponseWriter, r *http.Request) {
	all, _ := json.Marshal(r.Header)
	w.Header().Set("X-Echo", r.Header.Get(gatewayIdentityHeader))
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(all)
		_ = zw.Close()
		return
	}
	_, _ = w.Write(all)
}

func newGwidUpstream(t *testing.T, h2 bool) *gwidUpstream {
	t.Helper()
	u := &gwidUpstream{respond: gwidEcho}
	u.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, r.Clone(context.Background()))
		respond := u.respond
		u.mu.Unlock()
		respond(w, r)
	}))
	u.srv.EnableHTTP2 = h2
	u.srv.StartTLS()
	t.Cleanup(u.srv.Close)
	return u
}

func (u *gwidUpstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.seen) }

func (u *gwidUpstream) last(t *testing.T) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.seen) == 0 {
		t.Fatal("the upstream received nothing")
	}
	return u.seen[len(u.seen)-1]
}

// gwidMint is the fake control plane.
type gwidMint struct {
	srv    *httptest.Server
	hits   atomic.Int64
	mu     sync.Mutex
	answer func(w http.ResponseWriter, r *http.Request, n int64)
	minted []string
}

type gwidRig struct {
	p         *Proxy
	buf       *bytes.Buffer
	gw, other *gwidUpstream
	mint      *gwidMint
	nowMu     sync.Mutex
	nowV      time.Time
}

func (g *gwidRig) now() time.Time { g.nowMu.Lock(); defer g.nowMu.Unlock(); return g.nowV }
func (g *gwidRig) advance(d time.Duration) {
	g.nowMu.Lock()
	defer g.nowMu.Unlock()
	g.nowV = g.nowV.Add(d)
}

// answerToken makes the mint answer a fresh token with the given audience, valid ten minutes.
func (g *gwidRig) answerToken(aud any) {
	g.mint.mu.Lock()
	defer g.mint.mu.Unlock()
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
		tok := gwidToken(aud, g.now().Add(10*time.Minute), n)
		g.mint.mu.Lock()
		g.mint.minted = append(g.mint.minted, tok)
		g.mint.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": g.now().Add(10 * time.Minute)})
	}
}

func (g *gwidRig) answerStatus(status int) {
	g.mint.mu.Lock()
	defer g.mint.mu.Unlock()
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) { w.WriteHeader(status) }
}

func (g *gwidRig) lastMinted(t *testing.T) string {
	t.Helper()
	g.mint.mu.Lock()
	defer g.mint.mu.Unlock()
	if len(g.mint.minted) == 0 {
		t.Fatal("nothing was minted")
	}
	return g.mint.minted[len(g.mint.minted)-1]
}

func newGwidRig(t *testing.T, routes []GatewayIdentityRoute, h2 bool) *gwidRig {
	t.Helper()
	g := &gwidRig{buf: &bytes.Buffer{}, nowV: time.Now(), gw: newGwidUpstream(t, h2), other: newGwidUpstream(t, false), mint: &gwidMint{}}
	g.mint.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := g.mint.hits.Add(1)
		var body struct {
			ProviderID string `json:"provider_id"`
			MCPServer  string `json:"mcp_server"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/gateway-identity" ||
			r.Header.Get("Authorization") != "Bearer RUNTOK" || json.NewDecoder(r.Body).Decode(&body) != nil || (body.ProviderID == "") == (body.MCPServer == "") {
			t.Errorf("bad mint request %s %s", r.Method, r.URL.Path)
		}
		g.mint.mu.Lock()
		answer := g.mint.answer
		g.mint.mu.Unlock()
		answer(w, r, n)
	}))
	t.Cleanup(g.mint.srv.Close)
	g.answerToken(gwidAudience)
	g.p = newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{gwidHost, gwidOther}}),
		Sink:   &decisionSink{out: g.buf, ch: make(chan egress.DecisionLog, 256)},
		Resolver: fakeResolver{m: map[string][]net.IP{
			gwidHost: ips("93.184.216.10"), gwidOther: ips("93.184.216.20"), "wardynd.test": ips("10.9.9.9"),
		}},
		Dial: func(ctx context.Context, network, target string) (net.Conn, error) {
			addr := upstreamAddr(g.gw.srv)
			switch {
			case strings.HasPrefix(target, "10.9.9.9:"):
				addr = upstreamAddr(g.mint.srv)
			case strings.HasPrefix(target, "93.184.216.20:"), target != "93.184.216.10:443":
				addr = upstreamAddr(g.other.srv)
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		Injector:        staticInj(map[string]injectedHeader{gwidHost: {name: "X-Api-Key", value: "BROKERED-MODEL-KEY"}}),
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + gwidHost + "/v1"},
		GatewayIdentity: routes,
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		Now:             g.now,
	})
	g.p.maskKey = uuid.New() // this test's own mask set
	t.Cleanup(func() { procRegistry.Evict(g.p.maskKey) })
	return g
}

func gwidRoutes() []GatewayIdentityRoute {
	return []GatewayIdentityRoute{{Host: gwidHost, Port: 443, PathPrefix: "/v1", ProviderID: "prov-1", Audience: gwidAudience}}
}

func gwidForge(r *http.Request) *http.Request {
	r.Header.Set(gatewayIdentityHeader, gwidForged)
	r.Header.Set(gatewayCallIDHeader, gwidCallID)
	r.Header.Set("Accept-Encoding", "gzip")
	return r
}

// gwidLanes are the two ways a model call reaches a route: the brokered route and a MITM'd request.
func gwidLanes(g *gwidRig, t *testing.T) map[string]func() *httptest.ResponseRecorder {
	return map[string]func() *httptest.ResponseRecorder{
		"brokered": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.handleLocalRoute(rec, gwidForge(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(`{"hi":1}`))))
			return rec
		},
		"mitm": func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.serveMITMRequest(rec, gwidForge(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", strings.NewReader(`{"hi":1}`))), gwidHost, 443)
			return rec
		},
	}
}

func gwidNotIn(t *testing.T, label, hay, token string) {
	t.Helper()
	if token != "" && strings.Contains(hay, token) {
		t.Errorf("%s holds the token: %q", label, hay)
	}
}

// The attach, on both lanes: the gateway gets the minted token and never the sandbox's value,
// and every echo of it comes back masked, whether or not the sandbox asked for gzip. REVERT
// PROOF: relayGatewayRoute relaying through p.relay fails this test at the echo.
func TestGatewayIdentity_TokenAttachedSandboxStrippedEchoMasked(t *testing.T) {
	for _, lane := range []string{"brokered", "mitm"} {
		t.Run(lane, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			g := newGwidRig(t, gwidRoutes(), false)
			rec := gwidLanes(g, t)[lane]()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			token := g.lastMinted(t)
			seen := g.gw.last(t)
			if got := seen.Header.Values(gatewayIdentityHeader); !slices.Equal(got, []string{token}) {
				t.Fatalf("gateway saw identity %q, want only the minted token", got)
			}
			if got := seen.Header.Get(gatewayCallIDHeader); got == gwidCallID {
				t.Errorf("the sandbox's call id reached the gateway")
			}
			if seen.Header.Get("X-Api-Key") != "BROKERED-MODEL-KEY" {
				t.Errorf("the model credential was lost: %v", seen.Header)
			}
			gwidNotIn(t, "response body", rec.Body.String(), token)
			gwidNotIn(t, "response headers", fmt.Sprint(rec.Header()), token)
			gwidNotIn(t, "decision rows", g.buf.String(), token)
			gwidNotIn(t, "logs", logs.String(), token)
			if strings.Contains(rec.Body.String(), "H4sI") || rec.Header().Get("Content-Encoding") != "" {
				t.Errorf("the answer was not decompressed for the masker: %v", rec.Header())
			}
			if !strings.Contains(rec.Body.String(), `"X-Wardyn-Identity":["<secret-hidden>"]`) {
				t.Errorf("the echo was not relayed masked: %s", rec.Body)
			}
		})
	}
}

// A token for an audience this route does not expect is refused and never sent,
// and refused again if it arrives on a later refresh, dropping the good token held.
func TestGatewayIdentity_AudienceMismatchRefused(t *testing.T) {
	for _, aud := range []any{"https://other-gateway.test", []string{gwidAudience, "https://other-gateway.test"}, []string{}} {
		g := newGwidRig(t, gwidRoutes(), false)
		g.answerToken(aud)
		for lane, send := range gwidLanes(g, t) {
			rec := send()
			if rec.Code < 400 || g.gw.count() != 0 {
				t.Fatalf("%s aud %v: status %d, gateway hits %d", lane, aud, rec.Code, g.gw.count())
			}
			if !strings.Contains(rec.Body.String(), "another audience") {
				t.Errorf("%s: the refusal says nothing clear: %s", lane, rec.Body)
			}
		}
	}
	g := newGwidRig(t, gwidRoutes(), false)
	send := gwidLanes(g, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d", rec.Code)
	}
	if got := g.gw.count(); got != 1 {
		t.Fatalf("gateway hits %d", got)
	}
	g.answerToken("https://new-gateway.test")
	g.advance(6 * time.Minute)
	send() // starts the refresh behind the held token
	gwidWaitNoFlight(t, g)
	before := g.gw.count()
	if rec := send(); rec.Code < 400 || g.gw.count() != before {
		t.Fatalf("after a mismatched refresh: status %d, gateway hits %d -> %d", rec.Code, before, g.gw.count())
	}
}

func gwidWaitNoFlight(t *testing.T, g *gwidRig) {
	t.Helper()
	h := g.p.gatewayHolders["prov-1"]
	for range 500 {
		h.mu.Lock()
		out := h.flight != nil
		h.mu.Unlock()
		if !out {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the mint never settled")
}

// A failing mint fails closed on a model route, with a clear message, and nothing leaves.
func TestGatewayIdentity_MintFailureRefuses(t *testing.T) {
	cases := map[string]func(*gwidRig){
		"500": func(g *gwidRig) { g.answerStatus(http.StatusInternalServerError) },
		"403": func(g *gwidRig) { g.answerStatus(http.StatusForbidden) },
		"404": func(g *gwidRig) { g.answerStatus(http.StatusNotFound) },
		"not json": func(g *gwidRig) {
			g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) { _, _ = io.WriteString(w, "<html>") }
		},
		"no token": func(g *gwidRig) {
			g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) { _, _ = io.WriteString(w, `{"token":""}`) }
		},
		"expired": func(g *gwidRig) {
			g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) {
				_ = json.NewEncoder(w).Encode(map[string]any{"token": gwidToken(gwidAudience, g.now().Add(-time.Second), 1), "expires_at": g.now().Add(-time.Second)})
			}
		},
		"hanging": func(g *gwidRig) {
			g.p.gatewayHolders["prov-1"].timeout = 100 * time.Millisecond
			g.mint.answer = func(_ http.ResponseWriter, r *http.Request, _ int64) { <-r.Context().Done() }
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			arrange(g)
			for lane, send := range gwidLanes(g, t) {
				rec := send()
				if rec.Code < 400 {
					t.Errorf("%s: status %d, want a refusal", lane, rec.Code)
				}
				if !strings.Contains(rec.Body.String(), "gateway identity") {
					t.Errorf("%s: unclear refusal: %s", lane, rec.Body)
				}
				gwidNotIn(t, lane+" refusal", rec.Body.String(), gwidForged)
			}
			if n := g.gw.count(); n != 0 {
				t.Fatalf("the gateway was reached %d times", n)
			}
			if !strings.Contains(g.buf.String(), "gateway identity unavailable") {
				t.Errorf("no deny row names the cause: %s", g.buf)
			}
			// Retries are refused from the last answer: one mint per interval, however many sends.
			send := gwidLanes(g, t)["brokered"]
			for range 5 {
				if rec := send(); rec.Code < 400 {
					t.Errorf("a retry was not refused: %d", rec.Code)
				}
			}
			if n := g.mint.hits.Load(); n != 1 {
				t.Errorf("mints across repeated sends = %d, want 1", n)
			}
			g.advance(lastGoodRetry)
			send()
			if n := g.mint.hits.Load(); n != 2 {
				t.Errorf("mints after the interval = %d, want 2", n)
			}
		})
	}
}

// The clock of a held token: no mint before half-life; at half-life the held token keeps
// serving while one mint runs behind it; an outage keeps it to exp and not a moment past.
func TestGatewayIdentity_RefreshAndOutageTolerance(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	send := gwidLanes(g, t)["brokered"]
	send()
	first := g.lastMinted(t)
	g.advance(4*time.Minute + 59*time.Second)
	send()
	if n := g.mint.hits.Load(); n != 1 {
		t.Fatalf("mints before half-life = %d, want 1", n)
	}
	g.answerStatus(http.StatusServiceUnavailable)
	g.advance(1 * time.Second) // now at half-life, and the control plane is down
	if send(); g.gw.last(t).Header.Get(gatewayIdentityHeader) != first {
		t.Fatal("the held token was not used during the refresh")
	}
	gwidWaitNoFlight(t, g)
	g.advance(4*time.Minute + 59*time.Second) // one second before exp: five minutes of tolerance
	if rec := send(); rec.Code != http.StatusOK || g.gw.last(t).Header.Get(gatewayIdentityHeader) != first {
		t.Fatalf("a 5xx must keep the token to exp: %d", rec.Code)
	}
	gwidWaitNoFlight(t, g)
	hits := g.gw.count()
	g.advance(1 * time.Second) // exactly exp: no grace
	if rec := send(); rec.Code < 400 || g.gw.count() != hits {
		t.Fatalf("past exp: status %d, gateway hits %d -> %d", rec.Code, hits, g.gw.count())
	}
}

// A short-lived token: a failed refresh is asked again within half of what is left of its life,
// not after the full retry interval, so a mint that recovers lands before exp.
func TestGatewayIdentity_ShortLifeRetriesBeforeExp(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	short := func() {
		g.mint.mu.Lock()
		defer g.mint.mu.Unlock()
		g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
			tok := gwidToken(gwidAudience, g.now().Add(time.Minute), n)
			g.mint.mu.Lock()
			g.mint.minted = append(g.mint.minted, tok)
			g.mint.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": g.now().Add(time.Minute)})
		}
	}
	short()
	send := gwidLanes(g, t)["brokered"]
	send()
	first := g.lastMinted(t)
	g.answerStatus(http.StatusServiceUnavailable)
	g.advance(30 * time.Second) // half-life, and the control plane is down
	send()
	gwidWaitNoFlight(t, g)
	if n := g.mint.hits.Load(); n != 2 {
		t.Fatalf("mints at half-life = %d, want 2", n)
	}
	short()                     // the control plane is back
	g.advance(15 * time.Second) // half of the 30 s left, well inside lastGoodRetry
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d before exp", rec.Code)
	}
	gwidWaitNoFlight(t, g)
	if n := g.mint.hits.Load(); n != 3 {
		t.Fatalf("mints 15 s after the failed refresh = %d, want 3", n)
	}
	g.advance(20 * time.Second) // past the first token's exp
	if rec := send(); rec.Code != http.StatusOK || g.gw.last(t).Header.Get(gatewayIdentityHeader) == first {
		t.Fatalf("past the first exp: status %d, still the first token=%v", rec.Code, g.gw.last(t).Header.Get(gatewayIdentityHeader) == first)
	}
}

// Past a short-lived token's exp with the control plane still down, mints fall back to the usual
// interval instead of one a second.
func TestGatewayIdentity_ShortLifeOutagePastExpKeepsTheInterval(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	g.mint.mu.Lock()
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
		tok := gwidToken(gwidAudience, g.now().Add(time.Minute), n)
		g.mint.mu.Lock()
		g.mint.minted = append(g.mint.minted, tok)
		g.mint.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": g.now().Add(time.Minute)})
	}
	g.mint.mu.Unlock()
	send := gwidLanes(g, t)["brokered"]
	send()
	g.answerStatus(http.StatusServiceUnavailable)
	g.advance(61 * time.Second) // past exp, never refreshed
	send()
	gwidWaitNoFlight(t, g)
	after := g.mint.hits.Load()
	g.advance(2 * time.Second)
	send()
	gwidWaitNoFlight(t, g)
	if n := g.mint.hits.Load(); n != after {
		t.Fatalf("mints 2 s after a failure past exp = %d, want %d", n, after)
	}
	g.advance(lastGoodRetry)
	send()
	gwidWaitNoFlight(t, g)
	if n := g.mint.hits.Load(); n != after+1 {
		t.Fatalf("mints after the interval = %d, want %d", n, after+1)
	}
}

// A refusal on refresh drops the held token at once, and an exp far off is held to ten minutes.
func TestGatewayIdentity_RefusalDropsHeldAndLifetimeIsCapped(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		g := newGwidRig(t, gwidRoutes(), false)
		send := gwidLanes(g, t)["brokered"]
		send()
		g.answerStatus(status)
		g.advance(5 * time.Minute)
		send() // served by the held token; starts the mint
		gwidWaitNoFlight(t, g)
		hits := g.gw.count()
		if rec := send(); rec.Code < 400 || g.gw.count() != hits {
			t.Fatalf("%d on refresh: status %d, gateway hits %d -> %d", status, rec.Code, hits, g.gw.count())
		}
	}
	g := newGwidRig(t, gwidRoutes(), false)
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": gwidToken(gwidAudience, g.now().Add(time.Hour), n), "expires_at": g.now().Add(time.Hour)})
	}
	send := gwidLanes(g, t)["brokered"]
	send()
	g.answerStatus(http.StatusInternalServerError)
	g.advance(10 * time.Minute)
	if rec := send(); rec.Code < 400 {
		t.Fatalf("a token held past ten minutes served: %d", rec.Code)
	}
}

// The token's life is judged against the control plane's expires_at, wrong clocks included:
// a proxy clock ahead refuses at once, a missing or unreadable expires_at refuses, and the
// earlier of expires_at and exp is the one held to.
func TestGatewayIdentity_ClockJudgedAgainstExpiresAt(t *testing.T) {
	answer := func(g *gwidRig, exp time.Time, expiresAt any) {
		g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": gwidToken(gwidAudience, exp, n), "expires_at": expiresAt})
		}
	}
	for name, at := range map[string]func(time.Time) (time.Time, any){
		"proxy clock ahead":     func(now time.Time) (time.Time, any) { return now.Add(-time.Minute), now.Add(-time.Minute) },
		"expires_at passed":     func(now time.Time) (time.Time, any) { return now.Add(10 * time.Minute), now.Add(-time.Second) },
		"expires_at missing":    func(now time.Time) (time.Time, any) { return now.Add(10 * time.Minute), nil },
		"expires_at not a time": func(now time.Time) (time.Time, any) { return now.Add(10 * time.Minute), "soon" },
	} {
		g := newGwidRig(t, gwidRoutes(), false)
		exp, expiresAt := at(g.now())
		answer(g, exp, expiresAt)
		if rec := gwidLanes(g, t)["brokered"](); rec.Code < 400 || g.gw.count() != 0 {
			t.Errorf("%s: status %d, gateway hits %d", name, rec.Code, g.gw.count())
		}
	}
	g := newGwidRig(t, gwidRoutes(), false)
	answer(g, g.now().Add(10*time.Minute), g.now().Add(time.Minute))
	send := gwidLanes(g, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("a good token: %d %s", rec.Code, rec.Body)
	}
	g.answerStatus(http.StatusInternalServerError)
	g.advance(time.Minute)
	if rec := send(); rec.Code < 400 {
		t.Fatalf("held past expires_at: %d", rec.Code)
	}
}

// A control plane whose refusal, or unreadable answer, carries a token: it reaches no
// decision row, no sandbox-facing error, no log, and no spool.
func TestGatewayIdentity_RefusalBodyNeverRepeated(t *testing.T) {
	const planted = "eyJwbGFudGVk.planted-token-in-a-refusal-body.c2ln"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for name, reply := range map[string]func(http.ResponseWriter){
		"403 body": func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden); _, _ = io.WriteString(w, planted) },
		"500 body": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, planted)
		},
		"200, not JSON": func(w http.ResponseWriter) { _, _ = io.WriteString(w, planted) },
		"200, not a token": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": planted, "expires_at": time.Now().Add(time.Hour)})
		},
		"200, encoded": func(w http.ResponseWriter) {
			w.Header().Set("Content-Encoding", "br")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": planted})
		},
	} {
		g := newGwidRig(t, gwidRoutes(), false)
		g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) { reply(w) }
		for lane, send := range gwidLanes(g, t) {
			rec := send()
			if rec.Code < 400 || g.gw.count() != 0 {
				t.Fatalf("%s %s: status %d, gateway hits %d", name, lane, rec.Code, g.gw.count())
			}
			gwidNotIn(t, name+" sandbox error", rec.Body.String()+fmt.Sprint(rec.Header()), planted)
		}
		gwidNotIn(t, name+" decision rows", g.buf.String(), planted)
		gwidNotIn(t, name+" logs", logs.String(), planted)
	}
}

// The runner audit spool, on disk: a row for a call that carried the token and one for a
// refusal whose body carried another hold neither.
func TestGatewayIdentity_SpoolHoldsNoToken(t *testing.T) {
	const planted = "eyJwbGFudGVk.planted-token-in-a-refusal-body.c2ln"
	dir := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := newGwidRig(t, gwidRoutes(), false)
	spool, err := runnerspool.Open(dir, g.p.runID)
	if err != nil {
		t.Fatal(err)
	}
	g.p.sink.local = spool
	send := gwidLanes(g, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec.Code, rec.Body)
	}
	token := g.lastMinted(t)
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, planted)
	}
	g.advance(5 * time.Minute)
	send()
	gwidWaitNoFlight(t, g)
	if rec := send(); rec.Code < 400 {
		t.Fatalf("after the refusal: %d", rec.Code)
	}
	var disk bytes.Buffer
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		disk.Write(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disk.String(), "gateway identity unavailable") {
		t.Fatalf("the spool holds no refusal row, so it proves nothing: %d bytes", disk.Len())
	}
	gwidNotIn(t, "spool", disk.String(), token)
	gwidNotIn(t, "spool", disk.String(), planted)
}

// Many callers with no token share one mint, and the mask set stays at two values however
// many times it rotates, so the set does not grow for the run's life.
func TestGatewayIdentity_SingleMintAndMaskSetStaysAtTwoTokens(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	release := make(chan struct{})
	base := g.mint.answer
	g.mint.answer = func(w http.ResponseWriter, r *http.Request, n int64) { <-release; base(w, r, n) }
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := g.p.gatewayHolders["prov-1"].current(context.Background(), g.p); err != nil {
				t.Errorf("current: %v", err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := g.mint.hits.Load(); n != 1 {
		t.Fatalf("16 callers made %d mints", n)
	}
	send := gwidLanes(g, t)["brokered"]
	var all []string
	for range 5 {
		g.advance(5 * time.Minute)
		send()
		gwidWaitNoFlight(t, g)
		all = append(all, g.lastMinted(t))
	}
	set := procRegistry.Snapshot(g.p.maskKey)
	if len(set) != 4 {
		t.Fatalf("mask set holds %d values, want two tokens and their signature heads", len(set))
	}
	for _, token := range all[len(all)-2:] {
		for _, want := range gatewayMaskValues(token) {
			if !slices.ContainsFunc(set, func(v []byte) bool { return bytes.Equal(v, want) }) {
				t.Errorf("a recent token's value is not in the mask set")
			}
		}
	}
}

// A 3xx goes back to the sandbox, not followed, and its target never sees the token: not
// another host, not another port, not a path outside the prefix. A Location echoing it is masked.
func TestGatewayIdentity_RedirectsNeverCarryIt(t *testing.T) {
	for lane := range map[string]bool{"brokered": true, "mitm": true} {
		t.Run(lane, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://"+gwidOther+"/landing?tok="+r.Header.Get(gatewayIdentityHeader))
				w.WriteHeader(http.StatusTemporaryRedirect)
			}
			rec := gwidLanes(g, t)[lane]()
			if rec.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status %d", rec.Code)
			}
			token := g.lastMinted(t)
			gwidNotIn(t, "Location", rec.Header().Get("Location"), token)
			if g.gw.count() != 1 || g.other.count() != 0 {
				t.Fatalf("gateway hits %d, other host hits %d: the proxy followed the redirect", g.gw.count(), g.other.count())
			}
			g.gw.respond = gwidEcho
			for _, follow := range []struct {
				name, host, target string
				port               int
				up                 *gwidUpstream
			}{
				{"another host", gwidOther, "https://" + gwidOther + "/landing", 443, g.other},
				{"another port", gwidHost, "https://" + gwidHost + ":8443/v1/messages", 8443, g.other},
				{"outside the prefix", gwidHost, "https://" + gwidHost + "/other", 443, g.gw},
				{"a prefix look-alike", gwidHost, "https://" + gwidHost + "/v1evil/x", 443, g.gw},
				{"a dot segment out of the prefix", gwidHost, "https://" + gwidHost + "/v1/../other", 443, g.gw},
			} {
				hits := follow.up.count()
				out := httptest.NewRecorder()
				g.p.serveMITMRequest(out, gwidForge(httptest.NewRequest(http.MethodGet, follow.target, nil)), follow.host, follow.port)
				if follow.up.count() != hits+1 {
					t.Fatalf("%s: not forwarded: %d %s", follow.name, out.Code, out.Body)
				}
				seen := follow.up.last(t)
				if seen.Header.Get(gatewayIdentityHeader) != "" || seen.Header.Get(gatewayCallIDHeader) != "" {
					t.Errorf("%s: saw %v", follow.name, seen.Header)
				}
			}
			if n := g.mint.hits.Load(); n != 1 {
				t.Errorf("mints = %d, a request off the route must not mint", n)
			}
			gwidNotIn(t, "decision rows", g.buf.String(), token)
		})
	}
}

// Two providers on a run, one with identity on: only that host carries it. A plain-HTTP
// forward on a run with a route loses the sandbox's values and gains no token.
func TestGatewayIdentity_OtherRoutesAndPlainLaneSeeNothing(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	gwidLanes(g, t)["brokered"]()
	token := g.lastMinted(t)

	plain := captureUpstream(t, false, "ok")
	g.p.dial = redirectDial(upstreamAddr(plain.srv))
	rec := httptest.NewRecorder()
	g.p.handlePlain(rec, gwidForge(httptest.NewRequest(http.MethodPost, "http://"+gwidOther+"/thing", strings.NewReader("x"))))
	if !plain.reached {
		t.Fatalf("plain forward not sent: %d %s", rec.Code, rec.Body)
	}
	if got := plain.header.Get(gatewayIdentityHeader) + plain.header.Get(gatewayCallIDHeader); got != "" {
		t.Errorf("plain lane forwarded %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("plain lane answer %q", rec.Body)
	}
	gwidNotIn(t, "plain lane", fmt.Sprint(plain.header), token)
}

// An MCP server gets a fresh call id and no token, on a run with a route. With none, nothing changes.
func TestGatewayIdentity_MCPGetsCallIDOnly(t *testing.T) {
	seen := make(chan http.Header, 2)
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.linear.test", "mcp.github.test"}}, nil, nil)
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(backend.Close)
	f.p.dial = redirectDial(upstreamAddr(backend))
	hdrs := []string{gatewayIdentityHeader, gwidForged, gatewayCallIDHeader, gwidCallID}

	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, hdrs...); rec.Code != http.StatusOK {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}
	if off := <-seen; off.Get(gatewayIdentityHeader) != gwidForged || off.Get(gatewayCallIDHeader) != gwidCallID {
		t.Errorf("off changed what the sandbox sends: %v", off)
	}

	f.p.gatewayRoutes = gwidRoutes()
	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, hdrs...); rec.Code != http.StatusOK {
		t.Fatalf("on: %d %s", rec.Code, rec.Body)
	}
	on := <-seen
	if on.Get(gatewayIdentityHeader) != "" {
		t.Errorf("an MCP server got identity %q", on.Get(gatewayIdentityHeader))
	}
	if _, err := uuid.Parse(on.Get(gatewayCallIDHeader)); err != nil {
		t.Errorf("call id %q is not the proxy's: %v", on.Get(gatewayCallIDHeader), err)
	}
}

// The stream stays a stream under the forced scrub, and a token split across two chunks is
// still masked. REVERT PROOF: relayGatewayRoute through p.relay reads nothing inside the bound.
func TestGatewayIdentity_StreamingUnderForcedScrub(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	if _, err := g.p.gatewayHolders["prov-1"].current(t.Context(), g.p); err != nil {
		t.Fatal(err)
	}
	idle := make(chan struct{})
	var once sync.Once
	wake := func() { once.Do(func() { close(idle) }) }
	var wrote atomic.Int64
	g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		wrote.Store(time.Now().UnixNano())
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-idle
		tok := r.Header.Get(gatewayIdentityHeader)
		_, _ = io.WriteString(w, "data: "+tok[:20])
		fl.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, tok[20:]+"\n\n")
		fl.Flush()
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.p.handleLocalRoute(w, r) }))
	t.Cleanup(front.Close)
	t.Cleanup(wake)
	// The upstream idles after its first event, so a relay that batches never answers this.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+llmAnthropicPrefix+"messages", strings.NewReader(`{"hi":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no answer while the upstream idles: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	took := time.Since(time.Unix(0, wrote.Load()))
	if err != nil || line != "data: first\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	if took > 200*time.Millisecond {
		t.Errorf("first event took %v, want under 200ms", took)
	}
	wake()
	rest, _ := io.ReadAll(br)
	gwidNotIn(t, "stream", string(rest), g.lastMinted(t))
	gwidNotIn(t, "stream", string(rest), g.lastMinted(t)[:40])
	if !strings.Contains(string(rest), "data: <secret-hidden>") {
		t.Errorf("the split token was not masked: %q", rest)
	}
}

// Bytes the masker cannot read are refused on a route, and a masker with nothing to mask too.
func TestGatewayIdentity_UnreadableAnswerRefused(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, r.Header.Get(gatewayIdentityHeader))
	}
	for lane, send := range gwidLanes(g, t) {
		rec := send()
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s: %d %v", lane, rec.Code, rec.Header())
		}
		gwidNotIn(t, lane, rec.Body.String(), g.lastMinted(t))
	}
}

// The same on the HTTP/2 fallback: the token goes, gzip is negotiated by the transport
// beneath the masker, and the echo comes back masked.
func TestGatewayIdentity_HTTP2FallbackEcho(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), true)
	g.p.h2.hosts.Store(memoKey(gwidHost, "443"), true)
	for lane, send := range gwidLanes(g, t) {
		rec := send()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", lane, rec.Code, rec.Body)
		}
		seen := g.gw.last(t)
		if seen.ProtoMajor != 2 || seen.Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
			t.Fatalf("%s: proto %d, identity %q", lane, seen.ProtoMajor, seen.Header.Get(gatewayIdentityHeader))
		}
		gwidNotIn(t, lane+" body", rec.Body.String(), g.lastMinted(t))
		gwidNotIn(t, lane+" headers", fmt.Sprint(rec.Header()), g.lastMinted(t))
	}
}

// A sandbox's range never reaches a route: a gateway that echoes headers and honours it would
// split the token across responses, past exact-byte masking. Two ranges over the echo prove the
// split a kept header allows. Off the route, and with no route, the headers pass as before.
func TestGatewayIdentity_RangeDroppedOnRoute(t *testing.T) {
	ranged := func(g *gwidRig) {
		g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
			echo := r.Header.Get(gatewayIdentityHeader)
			var from, to int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err == nil && to < len(echo) {
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, echo[from:to+1])
				return
			}
			_, _ = io.WriteString(w, echo)
		}
	}
	withRange := func(r *http.Request, spec string) *http.Request {
		r.Header.Set("Range", spec)
		r.Header.Set("If-Range", `"etag"`)
		return gwidForge(r)
	}
	for lane, send := range map[string]func(*gwidRig, string) *httptest.ResponseRecorder{
		"brokered": func(g *gwidRig, spec string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.handleLocalRoute(rec, withRange(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("x")), spec))
			return rec
		},
		"mitm": func(g *gwidRig, spec string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			g.p.serveMITMRequest(rec, withRange(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", strings.NewReader("x")), spec), gwidHost, 443)
			return rec
		},
	} {
		t.Run(lane, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			ranged(g)
			first, second := send(g, "bytes=0-19"), send(g, "bytes=20-39")
			token := g.lastMinted(t)
			for _, h := range []string{"Range", "If-Range"} {
				if got := g.gw.last(t).Header.Get(h); got != "" {
					t.Errorf("%s reached the gateway: %q", h, got)
				}
			}
			if first.Code != http.StatusOK || second.Code != http.StatusOK {
				t.Errorf("status %d, %d, want whole answers", first.Code, second.Code)
			}
			for _, piece := range []string{token[:20], token[20:40]} {
				gwidNotIn(t, "answer", first.Body.String()+second.Body.String(), piece)
			}
		})
	}
	g := newGwidRig(t, gwidRoutes(), false)
	rec := httptest.NewRecorder()
	g.p.serveMITMRequest(rec, withRange(httptest.NewRequest(http.MethodGet, "https://"+gwidHost+"/other", nil), "bytes=0-1"), gwidHost, 443)
	if got := g.gw.last(t).Header.Get("Range"); got != "bytes=0-1" {
		t.Errorf("off the route, Range = %q, want it kept", got)
	}
	off := newGwidRig(t, nil, false)
	rec = httptest.NewRecorder()
	off.p.handleLocalRoute(rec, withRange(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("x")), "bytes=0-1"))
	if seen := off.gw.last(t); seen.Header.Get("Range") != "bytes=0-1" || seen.Header.Get("If-Range") != `"etag"` {
		t.Errorf("no route, the sandbox's range changed: %v", seen.Header)
	}
}

// An answer on a route that ends in the start of the token: the end is replaced however it
// came, a read failure, a clean short body, one byte or nearly all of it, split or not. REVERT
// PROOF: ending with MaskingWriter.Close sends all but the token's last byte to the sandbox.
func TestGatewayIdentity_AnswerEndingInTheTokensStart(t *testing.T) {
	const hidden = "<secret-hidden>"
	short := func(w http.ResponseWriter, declared int, parts ...string) {
		if declared > 0 {
			w.Header().Set("Content-Length", fmt.Sprint(declared))
		}
		for _, part := range parts {
			_, _ = io.WriteString(w, part)
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}
	cases := []struct {
		name    string
		respond func(w http.ResponseWriter, tok string)
		want    func(tok string) string
	}{
		{"cut by Content-Length, one byte short", func(w http.ResponseWriter, tok string) { short(w, len(tok), tok[:len(tok)-1]) },
			func(string) string { return hidden }},
		{"clean short body, one byte short", func(w http.ResponseWriter, tok string) { short(w, len(tok)-1, tok[:len(tok)-1]) },
			func(string) string { return hidden }},
		{"clean short body, eight bytes", func(w http.ResponseWriter, tok string) { short(w, 8, tok[:8]) },
			func(string) string { return hidden }},
		{"under the floor, seven bytes pass", func(w http.ResponseWriter, tok string) { short(w, 7, tok[:7]) },
			func(tok string) string { return tok[:7] }},
		{"under the floor, one byte passes", func(w http.ResponseWriter, tok string) { short(w, 1, tok[:1]) },
			func(tok string) string { return tok[:1] }},
		{"cut by Content-Length, far short", func(w http.ResponseWriter, tok string) { short(w, len(tok), tok[:30]) },
			func(string) string { return hidden }},
		{"a start split across chunks, at the very end of a stream", func(w http.ResponseWriter, tok string) {
			short(w, 0, "data: ", tok[:10], tok[10:60], tok[60:len(tok)-2])
		},
			func(string) string { return "data: " + hidden }},
		{"the whole token split across writes", func(w http.ResponseWriter, tok string) { short(w, 0, "data: "+tok[:20], tok[20:50], tok[50:]+"\n") },
			func(string) string { return "data: " + hidden + "\n" }},
		{"the whole token, then the start of it again", func(w http.ResponseWriter, tok string) { short(w, 0, tok+" then "+tok[:12]) },
			func(string) string { return hidden + " then " + hidden }},
		{"a body ending in other bytes is untouched", func(w http.ResponseWriter, _ string) { short(w, 0, `{"ok":true}`) },
			func(string) string { return `{"ok":true}` }},
		// JWTs start "e", and the floor keeps text ending in "e" whole.
		{"a benign end equal to the token's first byte", func(w http.ResponseWriter, _ string) { short(w, 0, "Service Unavailable") },
			func(string) string { return "Service Unavailable" }},
	}
	for lane := range map[string]bool{"brokered": true, "mitm": true} {
		for _, tc := range cases {
			t.Run(lane+"/"+tc.name, func(t *testing.T) {
				g := newGwidRig(t, gwidRoutes(), false)
				g.gw.respond = func(w http.ResponseWriter, r *http.Request) { tc.respond(w, r.Header.Get(gatewayIdentityHeader)) }
				rec := gwidLanes(g, t)[lane]()
				if got, want := rec.Body.String(), tc.want(g.lastMinted(t)); got != want {
					t.Errorf("answer %q, want %q", got, want)
				}
				if rec.Header().Get("Content-Length") != "" {
					t.Errorf("Content-Length %q survived the masking", rec.Header().Get("Content-Length"))
				}
			})
		}
	}
}

// An echo cut short with more bytes after it ends in no held value's start and holds no whole
// token, so the signature head is what stops it. What still passes is the residual: the claims
// and under twenty signature characters, never a usable token.
func TestGatewayIdentity_TruncatedEchoFollowedByMoreBytes(t *testing.T) {
	const hidden = "<secret-hidden>"
	// tok up to where its signature starts, the signature, and the head that is masked.
	split := func(tok string) (claims, sig, rest string) {
		at := strings.LastIndexByte(tok, '.') + 1
		return tok[:at], tok[at:], tok[at+gatewaySignatureHeadLen:]
	}
	cases := []struct {
		name string
		echo func(tok string) string
		want func(tok string) string
	}{
		{"json, token less a byte", func(tok string) string { return `{"echo":"` + tok[:len(tok)-1] + `"}` }, func(tok string) string {
			claims, _, rest := split(tok)
			return `{"echo":"` + claims + hidden + rest[:len(rest)-1] + `"}`
		}},
		{"token less a byte, more bytes", func(tok string) string { return tok[:len(tok)-1] + " x" }, func(tok string) string {
			claims, _, rest := split(tok)
			return claims + hidden + rest[:len(rest)-1] + " x"
		}},
		{"cut at 20 signature characters", func(tok string) string {
			claims, sig, _ := split(tok)
			return claims + sig[:20] + " x"
		}, func(tok string) string {
			claims, _, _ := split(tok)
			return claims + hidden + " x"
		}},
		{"cut at 19 signature characters passes: the residual", func(tok string) string {
			claims, sig, _ := split(tok)
			return claims + sig[:19] + " x"
		}, func(tok string) string {
			claims, sig, _ := split(tok)
			return claims + sig[:19] + " x"
		}},
		{"signature head alone", func(tok string) string {
			_, sig, _ := split(tok)
			return "a " + sig[:20] + " x"
		}, func(string) string { return "a " + hidden + " x" }},
		{"the whole token is one placeholder", func(tok string) string { return "a " + tok + " x" }, func(string) string { return "a " + hidden + " x" }},
	}
	// The same values with nothing after them: an answer's end, cut as a whole.
	ending := []struct {
		name string
		echo func(tok string) string
	}{
		{"cut at 19, at the end", func(tok string) string {
			claims, sig, _ := split(tok)
			return claims + sig[:19]
		}},
		{"cut at 20, at the end", func(tok string) string {
			claims, sig, _ := split(tok)
			return claims + sig[:20]
		}},
		{"signature head alone, at the end", func(tok string) string {
			_, sig, _ := split(tok)
			return sig[:20]
		}},
		{"signature head's first eight, at the end", func(tok string) string {
			_, sig, _ := split(tok)
			return sig[:8]
		}},
	}
	for lane := range map[string]bool{"brokered": true, "mitm": true} {
		for _, tc := range cases {
			t.Run(lane+"/"+tc.name, func(t *testing.T) {
				g := newGwidRig(t, gwidRoutes(), false)
				g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
					echo := tc.echo(r.Header.Get(gatewayIdentityHeader))
					w.Header().Set("X-Echo", echo)
					// Across two writes, the short echoes with the head split between them.
					at := len(echo) / 2
					_, _ = io.WriteString(w, echo[:at])
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, echo[at:]+"\n")
				}
				rec := gwidLanes(g, t)[lane]()
				want := tc.want(g.lastMinted(t))
				if got := rec.Body.String(); got != want+"\n" {
					t.Errorf("body %q, want %q", got, want+"\n")
				}
				if got := rec.Header().Get("X-Echo"); got != want {
					t.Errorf("header %q, want %q", got, want)
				}
			})
		}
		for _, tc := range ending {
			t.Run(lane+"/"+tc.name, func(t *testing.T) {
				g := newGwidRig(t, gwidRoutes(), false)
				g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
					echo := tc.echo(r.Header.Get(gatewayIdentityHeader))
					w.Header().Set("X-Echo", echo)
					_, _ = io.WriteString(w, echo)
				}
				rec := gwidLanes(g, t)[lane]()
				if got := rec.Body.String(); got != hidden {
					t.Errorf("body %q, want the placeholder", got)
				}
				if got := rec.Header().Get("X-Echo"); got != hidden {
					t.Errorf("header %q, want the placeholder", got)
				}
			})
		}
	}
}

// Under the floor the end of an answer passes, and that is all that does: seven bytes of a
// signature head with nothing before them are what the floor costs.
func TestGatewayIdentity_FloorPassesSevenSignatureBytes(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	var seven string
	g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(gatewayIdentityHeader)
		seven = tok[strings.LastIndexByte(tok, '.')+1:][:7]
		_, _ = io.WriteString(w, "a "+seven)
	}
	if got := gwidLanes(g, t)["brokered"]().Body.String(); got != "a "+seven {
		t.Errorf("%q", got)
	}
}

// A token whose signature cannot give a head is refused as malformed, on any lane, and no
// call goes out. Twenty characters is enough; nineteen is not.
func TestGatewayIdentity_TokenWithoutASignatureHeadRefused(t *testing.T) {
	short := func(token string, n int) string {
		at := strings.LastIndexByte(token, '.') + 1
		return token[:at] + token[at:at+n]
	}
	cases := []struct {
		name  string
		shape func(token string) string
		sent  bool
	}{
		{"no signature segment", func(tok string) string { return tok[:strings.LastIndexByte(tok, '.')] }, false},
		{"an empty signature", func(tok string) string { return short(tok, 0) }, false},
		{"nineteen characters", func(tok string) string { return short(tok, 19) }, false},
		{"twenty characters", func(tok string) string { return short(tok, 20) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.mint.answer = func(w http.ResponseWriter, _ *http.Request, n int64) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"token":      tc.shape(gwidToken(gwidAudience, g.now().Add(10*time.Minute), n)),
					"expires_at": g.now().Add(10 * time.Minute).Format(time.RFC3339),
				})
			}
			rec := gwidLanes(g, t)["brokered"]()
			if sent := g.gw.count() == 1; sent != tc.sent {
				t.Fatalf("sent %v, want %v: %d %s", sent, tc.sent, rec.Code, rec.Body)
			}
			if !tc.sent && rec.Code != http.StatusBadGateway {
				t.Fatalf("%d %s, want a refusal", rec.Code, rec.Body)
			}
		})
	}
}

// The cut applies to a route's answers alone. The same ending stays as it was off the prefix,
// on a run with no route, and under the scrub flag, whose relay ends as it always did.
func TestGatewayIdentity_CutStaysOnTheRoute(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	gwidLanes(g, t)["brokered"]()
	start := g.lastMinted(t)[:30]
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, start) }
	rec := httptest.NewRecorder()
	g.p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+gwidHost+"/other", nil), gwidHost, 443)
	if rec.Body.String() != start {
		t.Errorf("off the prefix: %q, want the bytes as sent", rec.Body)
	}
	off := newGwidRig(t, nil, false)
	off.gw.respond = g.gw.respond
	for lane, send := range gwidLanes(off, t) {
		if rec := send(); rec.Body.String() != start {
			t.Errorf("%s, no route: %q, want the bytes as sent", lane, rec.Body)
		}
	}
	procMaskFor(off.p.maskKey, []byte(g.lastMinted(t)))
	off.p.scrubResponses = true
	for lane, send := range gwidLanes(off, t) {
		if rec := send(); rec.Body.String() != start {
			t.Errorf("%s, scrub flag: %q, want the held bytes flushed as they always were", lane, rec.Body)
		}
	}
}

// A header on a route gets what a body's end gets: a value ending in the start of the token is
// replaced, and a token crossing two values of one name hides them all. Short ends stay, so
// "true", "false" and "no-cache" keep their last letter though every JWT starts "e".
func TestGatewayIdentity_HeaderValuesCut(t *testing.T) {
	const hidden = "<secret-hidden>"
	cases := []struct {
		name   string
		values func(tok string) []string
		want   func(tok string) []string
	}{
		{"one byte short", func(tok string) []string { return []string{tok[:len(tok)-1]} }, func(string) []string { return []string{hidden} }},
		{"the floor, eight bytes", func(tok string) []string { return []string{tok[:8]} }, func(string) []string { return []string{hidden} }},
		{"under the floor", func(tok string) []string { return []string{tok[:7]} }, func(tok string) []string { return []string{tok[:7]} }},
		{"at the end of a url", func(tok string) []string { return []string{"https://o.test/?t=" + tok[:40]} }, func(string) []string { return []string{"https://o.test/?t=" + hidden} }},
		{"a whole token in one of two", func(tok string) []string { return []string{"a", tok} }, func(string) []string { return []string{"a", hidden} }},
		{"a token across two values", func(tok string) []string { return []string{tok[:20], tok[20:]} }, func(string) []string { return []string{hidden, hidden} }},
		{"a short start across two values", func(tok string) []string { return []string{tok[:20], tok[20:60]} }, func(string) []string { return []string{hidden, hidden} }},
		{"a start across two values, the first short", func(tok string) []string { return []string{tok[:3], tok[3:60]} }, func(string) []string { return []string{hidden, hidden} }},
		{"benign true", func(string) []string { return []string{"true"} }, func(string) []string { return []string{"true"} }},
		{"benign false", func(string) []string { return []string{"false"} }, func(string) []string { return []string{"false"} }},
		{"benign no-cache", func(string) []string { return []string{"no-cache"} }, func(string) []string { return []string{"no-cache"} }},
		{"benign values", func(string) []string { return []string{"a=1; Path=/", "b=2; SameSite=Lax"} }, func(string) []string { return []string{"a=1; Path=/", "b=2; SameSite=Lax"} }},
	}
	for lane := range map[string]bool{"brokered": true, "mitm": true} {
		for _, tc := range cases {
			t.Run(lane+"/"+tc.name, func(t *testing.T) {
				g := newGwidRig(t, gwidRoutes(), false)
				g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
					for _, v := range tc.values(r.Header.Get(gatewayIdentityHeader)) {
						w.Header().Add("X-Echo", v)
					}
				}
				rec := gwidLanes(g, t)[lane]()
				if got, want := rec.Header().Values("X-Echo"), tc.want(g.lastMinted(t)); !slices.Equal(got, want) {
					t.Errorf("values %q, want %q", got, want)
				}
			})
		}
		t.Run(lane+"/Location", func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
				tok := r.Header.Get(gatewayIdentityHeader)
				w.Header().Set("Location", "https://"+gwidOther+"/?t="+tok[:len(tok)-1])
				w.WriteHeader(http.StatusTemporaryRedirect)
			}
			rec := gwidLanes(g, t)[lane]()
			if got, want := rec.Header().Get("Location"), "https://"+gwidOther+"/?t="+hidden; rec.Code != http.StatusTemporaryRedirect || got != want {
				t.Errorf("%d Location %q, want %q", rec.Code, got, want)
			}
		})
	}
	// The scrub flag's relay, and no route at all, keep exact masking of their headers.
	start := "abcdefghijklmnopqrstuvwxyz0123456789"
	off := newGwidRig(t, nil, false)
	procMaskFor(off.p.maskKey, []byte(start+"-the-rest-of-a-held-value"))
	off.gw.respond = func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-Echo", start) }
	for _, flag := range []bool{false, true} {
		off.p.scrubResponses = flag
		if rec := gwidLanes(off, t)["brokered"](); rec.Header().Get("X-Echo") != start {
			t.Errorf("scrub flag %v: X-Echo %q, want the value as sent", flag, rec.Header().Get("X-Echo"))
		}
	}
}

// A call pending longer than the mask set keeps a value: its own token is still masked when
// the answer finally comes, however many refreshes passed while it waited.
func TestGatewayIdentity_PendingCallKeepsItsTokenMasked(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	release := make(chan struct{})
	seen := make(chan struct{})
	var first atomic.Bool
	g.gw.respond = func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			close(seen)
			<-release
		}
		tok := r.Header.Get(gatewayIdentityHeader)
		w.Header().Set("X-Echo", tok)
		w.Header().Set("X-Echo-Short", tok[:len(tok)-1]+" x")
		_, _ = io.WriteString(w, tok)
	}
	send := gwidLanes(g, t)["brokered"]
	pending := make(chan *httptest.ResponseRecorder, 1)
	go func() { pending <- send() }()
	<-seen
	slow := g.lastMinted(t)
	for range 4 { // twenty minutes of refreshes while the first call waits
		g.advance(5 * time.Minute)
		send()
		gwidWaitNoFlight(t, g)
	}
	if slices.ContainsFunc(procRegistry.Snapshot(g.p.maskKey), func(v []byte) bool { return string(v) == slow }) {
		t.Fatal("the pending call's token is still in the mask set, so this proves nothing")
	}
	close(release)
	rec := <-pending
	gwidNotIn(t, "the late body", rec.Body.String(), slow)
	gwidNotIn(t, "the late header", rec.Header().Get("X-Echo"), slow)
	gwidNotIn(t, "the late short echo", rec.Header().Get("X-Echo-Short"), string(gatewayMaskValues(slow)[1]))
	if rec.Body.String() != "<secret-hidden>" {
		t.Errorf("late body %q", rec.Body)
	}
	if set := procRegistry.Snapshot(g.p.maskKey); len(set) != 4 || slices.ContainsFunc(set, func(v []byte) bool { return string(v) == slow }) {
		t.Errorf("the slow call left %d values in the shared mask set, or its token among them", len(set))
	}
}

// The token leaves the shared set at each moment a refresh can land around the answer: before the
// relay reads the set, and with the answer part read. Each masks, since the answer's own masker
// holds the token whatever the set does.
func TestGatewayIdentity_RefreshLandingDuringTheAnswer(t *testing.T) {
	const hidden = "<secret-hidden>"
	token := gwidToken(gwidAudience, time.Now().Add(10*time.Minute), 1)
	relay := func(t *testing.T, g *gwidRig, body io.Reader) *httptest.ResponseRecorder {
		t.Helper()
		resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Echo": {token}}, Body: io.NopCloser(body)}
		rec := httptest.NewRecorder()
		g.p.relayGatewayRoute(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"m", nil), gwidHost, 443, resp, ruleSourceLLM, token, "")
		return rec
	}
	t.Run("dropped before the set is read", func(t *testing.T) {
		g := newGwidRig(t, gwidRoutes(), false)
		for _, v := range gatewayMaskValues(token) {
			procRegistry.DropRunValue(g.p.maskKey, v)
		}
		rec := relay(t, g, strings.NewReader(token+"\n"))
		if rec.Body.String() != hidden+"\n" || rec.Header().Get("X-Echo") != hidden {
			t.Errorf("%q, X-Echo %q", rec.Body, rec.Header().Get("X-Echo"))
		}
	})
	t.Run("dropped with the answer part read", func(t *testing.T) {
		g := newGwidRig(t, gwidRoutes(), false)
		for _, v := range gatewayMaskValues(token) {
			procMaskFor(g.p.maskKey, v)
		}
		pr, pw := io.Pipe()
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- relay(t, g, pr) }()
		_, _ = io.WriteString(pw, "first\n") // returns once the relay has read it, so its masker is built
		for _, v := range gatewayMaskValues(token) {
			procRegistry.DropRunValue(g.p.maskKey, v)
		}
		_, _ = io.WriteString(pw, token+"\n")
		_ = pw.Close()
		if rec := <-done; rec.Body.String() != "first\n"+hidden+"\n" {
			t.Errorf("%q", rec.Body)
		}
		if slices.ContainsFunc(procRegistry.Snapshot(g.p.maskKey), func(v []byte) bool { return string(v) == token }) {
			t.Error("the relay put the token back in the shared set")
		}
	})
}

// The other lanes that forward a sandbox's headers: both names go on a run with a route, and
// pass as they did with none. The API door is driven below serveMITMRequest, which strips too.
func TestGatewayIdentity_BrokerLanesStrip(t *testing.T) {
	forge := map[string]func(t *testing.T, on bool, forged func(*http.Request) *http.Request) http.Header{
		"git broker": func(t *testing.T, on bool, forged func(*http.Request) *http.Request) http.Header {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))
			if on {
				p.gatewayRoutes = gwidRoutes()
			}
			p.ServeHTTP(httptest.NewRecorder(), forged(mustLocalReq(t, http.MethodGet, "/wardyn/gh/octocat/Hello-World.git/info/refs?service=git-upload-pack", nil)))
			return up.gitHeaders
		},
		"pat broker": func(t *testing.T, on bool, forged func(*http.Request) *http.Request) http.Header {
			up := newPATBrokerUpstream(t, "T", "oauth2")
			p, _ := newPATBrokerProxy(t, map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}}, upstreamAddr(up.srv))
			if on {
				p.gatewayRoutes = gwidRoutes()
			}
			p.ServeHTTP(httptest.NewRecorder(), forged(mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", nil)))
			return up.gitHeaders
		},
		"via org": func(t *testing.T, on bool, forged func(*http.Request) *http.Request) http.Header {
			f := newLaptopFixture(t)
			if on {
				f.p.gatewayRoutes = gwidRoutes()
			}
			req, _ := http.NewRequest(http.MethodGet, "http://api.corp.example/v1/models", nil)
			_ = readAll(t, proxyDoHandler(t, f.p, forged(req)))
			return f.cpReq.Header
		},
		"pat api door": func(t *testing.T, on bool, forged func(*http.Request) *http.Request) http.Header {
			h := newAPIHarness(t, types.PATForgeGitLab, apiGrants[types.PATForgeGitLab])
			if on {
				h.p.gatewayRoutes = gwidRoutes()
			}
			h.p.serveAPIDoor(httptest.NewRecorder(), forged(rawAPIRequest(t, http.MethodGet, glBase+"/issues", "", "", nil)), apiHost, 443)
			return h.up.gitHeaders
		},
	}
	for name, run := range forge {
		t.Run(name, func(t *testing.T) {
			on := run(t, true, gwidForge)
			if on == nil {
				t.Fatal("the forge received nothing")
			}
			if got := on.Get(gatewayIdentityHeader) + on.Get(gatewayCallIDHeader); got != "" {
				t.Errorf("with a route the forge saw %q", got)
			}
			off := run(t, false, gwidForge)
			if off.Get(gatewayIdentityHeader) != gwidForged || off.Get(gatewayCallIDHeader) != gwidCallID {
				t.Errorf("with no route the sandbox's headers changed: %v", off)
			}
		})
	}
}

// A plain forward keeps a chunked request's trailers through r.Clone: on a run with a route the
// gateway's two names go and others stay. With none, all stay.
func TestGatewayIdentity_PlainLaneTrailers(t *testing.T) {
	send := func(t *testing.T, routes []GatewayIdentityRoute) http.Header {
		g := newGwidRig(t, routes, false)
		seen := make(chan http.Header, 1)
		backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			seen <- r.Trailer.Clone()
		}))
		t.Cleanup(backend.Close)
		g.p.dial = redirectDial(upstreamAddr(backend))
		front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Scheme, r.URL.Host = "http", gwidOther
			g.p.handlePlain(w, r)
		}))
		t.Cleanup(front.Close)
		req, err := http.NewRequest(http.MethodPost, front.URL+"/thing", io.MultiReader(strings.NewReader("chunked")))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.Trailer = http.Header{gatewayIdentityHeader: {gwidForged}, gatewayCallIDHeader: {gwidCallID}, "X-Other": {"kept"}}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return <-seen
	}
	on := send(t, gwidRoutes())
	if _, ok := on[gatewayIdentityHeader]; ok {
		t.Errorf("identity trailer reached the destination: %v", on)
	}
	if _, ok := on[gatewayCallIDHeader]; ok {
		t.Errorf("call id trailer reached the destination: %v", on)
	}
	if _, ok := on["X-Other"]; !ok {
		t.Errorf("another trailer was lost, so this proves nothing: %v", on)
	}
	off := send(t, nil)
	for _, name := range []string{gatewayIdentityHeader, gatewayCallIDHeader, "X-Other"} {
		if _, ok := off[name]; !ok {
			t.Errorf("no route: %s was lost: %v", name, off)
		}
	}
}

// A route is matched by host, exact port and path prefix at a segment boundary.
func TestGatewayRouteFor(t *testing.T) {
	p := &Proxy{gatewayRoutes: gwidRoutes()}
	for _, tc := range []struct {
		host string
		port int
		path string
		want bool
	}{
		{gwidHost, 443, "/v1", true},
		{gwidHost, 443, "/v1/messages", true},
		{"GW.CORP.TEST.", 443, "/v1/messages", true},
		{gwidHost, 8443, "/v1/messages", false},
		{gwidOther, 443, "/v1/messages", false},
		{gwidHost, 443, "/v1evil", false},
		{gwidHost, 443, "/", false},
		{gwidHost, 443, "/v1/../other", false},
		{gwidHost, 443, "/v1/./x", false},
		{gwidHost, 443, `/v1/..\other`, false},
	} {
		if got := p.gatewayRouteFor(tc.host, tc.port, tc.path) != nil; got != tc.want {
			t.Errorf("gatewayRouteFor(%s, %d, %s) = %v", tc.host, tc.port, tc.path, got)
		}
	}
	whole := &Proxy{gatewayRoutes: []GatewayIdentityRoute{{Host: gwidHost, Port: 443, ProviderID: "p", Audience: "a"}}}
	if whole.gatewayRouteFor(gwidHost, 443, "/anything") == nil {
		t.Error("an empty prefix covers the host and port")
	}
	if (&Proxy{}).gatewayRouteFor(gwidHost, 443, "/v1") != nil {
		t.Error("no route, no match")
	}
}

// The config carries routes only, round-trips strictly, and refuses a malformed route.
func TestGatewayIdentityConfig(t *testing.T) {
	var keys []string
	rt := reflect.TypeFor[GatewayIdentityRoute]()
	for f := range rt.Fields() {
		keys = append(keys, f.Tag.Get("json"))
	}
	if want := []string{"host", "port", "path_prefix,omitempty", "provider_id,omitempty", "mcp_key,omitempty", "audience", "provider_uid,omitempty", "inspection_deferred,omitempty"}; !slices.Equal(keys, want) {
		t.Fatalf("route keys %v: a route holds no token and no key, want %v", keys, want)
	}
	good := map[string]any{"host": gwidHost, "port": 443, "path_prefix": "/v1", "provider_id": "p", "audience": "a"}
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{good}}))
	if err != nil || len(cfg.GatewayIdentity) != 1 || cfg.GatewayIdentity[0].Audience != "a" {
		t.Fatalf("round trip: %+v, %v", cfg, err)
	}
	for name, tc := range map[string]map[string]any{
		"no host":     {"port": 443, "provider_id": "p", "audience": "a"},
		"url host":    {"host": "https://x/", "port": 443, "provider_id": "p", "audience": "a"},
		"no port":     {"host": gwidHost, "provider_id": "p", "audience": "a"},
		"no provider": {"host": gwidHost, "port": 443, "audience": "a"},
		"both":        {"host": gwidHost, "port": 443, "provider_id": "p", "mcp_key": "m", "audience": "a"},
		"no audience": {"host": gwidHost, "port": 443, "provider_id": "p"},
		"bad prefix":  {"host": gwidHost, "port": 443, "provider_id": "p", "audience": "a", "path_prefix": "v1"},
		"unknown key": {"host": gwidHost, "port": 443, "provider_id": "p", "audience": "a", "token": "x"},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{tc}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, second := range map[string]map[string]any{
		"two audiences for a provider":       {"host": gwidOther, "port": 443, "provider_id": "p", "audience": "b"},
		"two providers on a host":            {"host": gwidHost, "port": 443, "provider_id": "q", "audience": "b"},
		"two providers on one path":          {"host": gwidHost, "port": 443, "provider_id": "q", "audience": "b", "path_prefix": "/v1/"},
		"a path under the other's":           {"host": gwidHost, "port": 443, "provider_id": "q", "audience": "b", "path_prefix": "/v1/messages"},
		"an MCP server on a provider's path": {"host": gwidHost, "port": 443, "mcp_key": "m", "audience": "b", "path_prefix": "/v1/mcp"},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{good, second}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// One gateway serving two providers on paths apart: each request matches one route.
	apart := map[string]any{"host": gwidHost, "port": 443, "path_prefix": "/v10", "provider_id": "q", "audience": "b"}
	cfg, err = LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{good, apart}}))
	if err != nil {
		t.Fatalf("two providers on paths apart: %v", err)
	}
	p := &Proxy{gatewayRoutes: cfg.GatewayIdentity}
	if rt := p.gatewayRouteFor(gwidHost, 443, "/v10/chat"); rt == nil || rt.ProviderID != "q" {
		t.Errorf("/v10/chat matched %+v, want q", rt)
	}
	if rt := p.gatewayRouteFor(gwidHost, 443, "/v1/messages"); rt == nil || rt.ProviderID != "p" {
		t.Errorf("/v1/messages matched %+v, want p", rt)
	}
	// An MCP server's route is a destination of its own: apart from the provider's it loads, and holds one audience.
	mcp := map[string]any{"host": gwidOther, "port": 443, "mcp_key": "m", "audience": "b"}
	cfg, err = LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{good, mcp}}))
	if err != nil {
		t.Fatalf("a provider and an MCP server on hosts apart: %v", err)
	}
	p = &Proxy{gatewayRoutes: cfg.GatewayIdentity}
	if rt := p.gatewayRouteFor(gwidOther, 443, "/x"); rt == nil || rt.MCPKey != "m" {
		t.Errorf("the MCP host matched %+v, want the MCP route", rt)
	}
	mcpTwice := map[string]any{"host": gwidHost, "port": 8443, "mcp_key": "m", "audience": "c"}
	if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{mcp, mcpTwice}})); err == nil {
		t.Error("one MCP server with two audiences: accepted")
	}
	unset, err := LoadConfigBytes(baseConfigJSON(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(unset)
	if strings.Contains(string(b), "gateway_identity") {
		t.Error("an unset table must not reach an older proxy's strict decoder")
	}
}

// answerBody makes the mint answer status with a fixed body.
func (g *gwidRig) answerBody(status int, body string) {
	g.mint.mu.Lock()
	defer g.mint.mu.Unlock()
	g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const (
	gwidCutBody     = `{"reason":"gateway_access_cut"}`
	gwidCutSentence = "This run's gateway access was cut when the portal that started it was revoked; the call was not sent. Start a new run if it is still needed."
)

// A 403 that names gateway_access_cut ends the route for the proxy's life: the sandbox gets a 403
// with the sentence (not a 502 an SDK would retry), no later request or clock step mints again,
// and the decision row's cause names the cut. REVERT PROOF: with no terminal state the mint count
// passes 1 and the status is 502.
func TestGatewayIdentity_CutIsTerminal(t *testing.T) {
	const planted = "eyJwbGFudGVk.planted-token-in-a-cut-body.c2ln"
	for lane := range gwidLanes(newGwidRig(t, gwidRoutes(), false), t) {
		t.Run(lane, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			g := newGwidRig(t, gwidRoutes(), false)
			g.answerBody(http.StatusForbidden, `{"reason":"gateway_access_cut","detail":"`+planted+`"}`)
			send := gwidLanes(g, t)[lane]
			steps := []time.Duration{0, 0, lastGoodRetry + time.Second, 11 * time.Minute, time.Hour}
			for i, step := range steps {
				g.advance(step)
				rec := send()
				if rec.Code != http.StatusForbidden {
					t.Fatalf("request %d: status %d, want 403: %s", i, rec.Code, rec.Body)
				}
				if !strings.Contains(rec.Body.String(), gwidCutSentence) {
					t.Errorf("request %d: the sentence is missing: %s", i, rec.Body)
				}
				gwidNotIn(t, "sandbox answer", rec.Body.String()+fmt.Sprint(rec.Header()), planted)
				if n := g.mint.hits.Load(); n != 1 {
					t.Fatalf("request %d: mints = %d, want 1", i, n)
				}
			}
			if n := g.gw.count(); n != 0 {
				t.Errorf("the gateway was reached %d times", n)
			}
			if !strings.Contains(g.buf.String(), "gateway access cut: the run's portal was revoked") {
				t.Errorf("no deny row names the cut: %s", g.buf)
			}
			gwidNotIn(t, "decision rows", g.buf.String(), planted)
			gwidNotIn(t, "logs", logs.String(), planted)
		})
	}
}

// Anything but the closed reason on a 403 is handled as before: a 502 for the SDK, asked again
// once per interval, and the sandbox never sees the cut sentence. The reason is read from a 403 only,
// from the first KiB of the body, and by exact value.
func TestGatewayIdentity_UnknownRefusalUnchanged(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"another reason":          {http.StatusForbidden, `{"reason":"identity_deactivated"}`},
		"free text":               {http.StatusForbidden, "gateway_access_cut"},
		"longer reason":           {http.StatusForbidden, `{"reason":"gateway_access_cut_later"}`},
		"other case":              {http.StatusForbidden, `{"reason":"Gateway_Access_Cut"}`},
		"reason in another field": {http.StatusForbidden, `{"message":"gateway_access_cut"}`},
		"reason not a string":     {http.StatusForbidden, `{"reason":["gateway_access_cut"]}`},
		"past the first KiB":      {http.StatusForbidden, `{"pad":"` + strings.Repeat("x", 2048) + `","reason":"gateway_access_cut"}`},
		"404 with the reason":     {http.StatusNotFound, gwidCutBody},
		"500 with the reason":     {http.StatusInternalServerError, gwidCutBody},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.answerBody(c.status, c.body)
			send := gwidLanes(g, t)["brokered"]
			rec := send()
			if rec.Code != http.StatusBadGateway {
				t.Errorf("status %d, want 502: %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "gateway access was cut") {
				t.Errorf("the cut sentence was sent: %s", rec.Body)
			}
			send()
			if n := g.mint.hits.Load(); n != 1 {
				t.Errorf("mints across repeated sends = %d, want 1", n)
			}
			g.advance(lastGoodRetry)
			send()
			if n := g.mint.hits.Load(); n != 2 {
				t.Errorf("mints after the interval = %d, want 2 (asked again)", n)
			}
		})
	}
}

// Idle past the refresh point, then a call: the refresh runs before the call is forwarded. A cut
// stops it at once, with no call served by the held token first. REVERT PROOF: the old refresh ran
// behind the call, so this first call after the half-life reaches the gateway.
func TestGatewayIdentity_CutStopsTheFirstCallPastRefreshAt(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	send := gwidLanes(g, t)["brokered"]
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first call: %d", rec.Code)
	}
	g.answerBody(http.StatusForbidden, gwidCutBody)
	g.advance(5 * time.Minute) // the refresh point of a ten-minute token
	reached := g.gw.count()
	rec := send()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), gwidCutSentence) {
		t.Fatalf("the first call past the refresh point: status %d: %s", rec.Code, rec.Body)
	}
	if g.gw.count() != reached {
		t.Errorf("the gateway was reached by the call that found the cut")
	}
	send()
	if n := g.mint.hits.Load(); n != 2 {
		t.Errorf("mints = %d, want 2 (the first and the refresh)", n)
	}
}

// A refresh that succeeds past the refresh point is the token the next call carries.
func TestGatewayIdentity_RefreshPastRefreshAtIsSynchronous(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	send := gwidLanes(g, t)["brokered"]
	send()
	first := g.lastMinted(t)
	g.advance(5 * time.Minute)
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := g.gw.last(t).Header.Get(gatewayIdentityHeader); got == first || got != g.lastMinted(t) {
		t.Errorf("the call past the refresh point carried the held token, not the refreshed one")
	}
	if n := g.mint.hits.Load(); n != 2 {
		t.Errorf("mints = %d, want 2", n)
	}
}

// A refresh that fails but may pass keeps the held token to exp: the same call is served by it,
// and the next calls do not ask again before the interval. Existing installs see no change.
func TestGatewayIdentity_RefreshOutageKeepsTheHeldToken(t *testing.T) {
	for name, arrange := range map[string]func(*gwidRig){
		"503": func(g *gwidRig) { g.answerStatus(http.StatusServiceUnavailable) },
		"429": func(g *gwidRig) { g.answerStatus(http.StatusTooManyRequests) },
		"unreachable": func(g *gwidRig) {
			g.mint.mu.Lock()
			defer g.mint.mu.Unlock()
			g.mint.answer = func(w http.ResponseWriter, _ *http.Request, _ int64) {
				c, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = c.Close()
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			send := gwidLanes(g, t)["brokered"]
			send()
			first := g.lastMinted(t)
			arrange(g)
			g.advance(5 * time.Minute)
			for range 3 {
				if rec := send(); rec.Code != http.StatusOK || g.gw.last(t).Header.Get(gatewayIdentityHeader) != first {
					t.Fatalf("the held token did not serve during the outage: %d", rec.Code)
				}
			}
			gwidWaitNoFlight(t, g)
			if n := g.mint.hits.Load(); n != 2 {
				t.Errorf("mints during the outage = %d, want 2 (the first and one refresh)", n)
			}
		})
	}
}

// A call already forwarded runs to completion when the cut lands; the next call is refused.
func TestGatewayIdentity_OpenStreamSurvivesTheCut(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	if _, err := g.p.gatewayHolders["prov-1"].current(t.Context(), g.p); err != nil {
		t.Fatal(err)
	}
	idle := make(chan struct{})
	var once sync.Once
	wake := func() { once.Do(func() { close(idle) }) }
	var calls atomic.Int64
	g.gw.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		if calls.Add(1) == 1 {
			<-idle
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.p.handleLocalRoute(w, r) }))
	t.Cleanup(front.Close)
	t.Cleanup(wake)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+llmAnthropicPrefix+"messages", strings.NewReader(`{"hi":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	g.answerBody(http.StatusForbidden, gwidCutBody)
	g.advance(5 * time.Minute)
	if rec := gwidLanes(g, t)["brokered"](); rec.Code != http.StatusForbidden {
		t.Fatalf("a new call after the cut: status %d, want 403", rec.Code)
	}
	wake()
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: last") {
		t.Errorf("the open stream did not finish: %q", rest)
	}
}

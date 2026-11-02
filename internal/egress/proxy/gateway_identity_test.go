// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
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
	return enc([]byte(`{"alg":"ES256"}`)) + "." + enc(payload) + "." + enc([]byte("signature-bytes"))
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
	p          *Proxy
	buf        *bytes.Buffer
	gw, other  *gwidUpstream
	mint       *gwidMint
	nowMu      sync.Mutex
	nowV       time.Time
	mitmTarget string
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
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/internal/gateway-identity" ||
			r.Header.Get("Authorization") != "Bearer RUNTOK" || json.NewDecoder(r.Body).Decode(&body) != nil || body.ProviderID == "" {
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
func TestGatewayIdentity_SingleMintAndMaskSetStaysAtTwo(t *testing.T) {
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
	if len(set) != 2 {
		t.Fatalf("mask set holds %d values, want 2", len(set))
	}
	for _, want := range all[len(all)-2:] {
		if !slices.ContainsFunc(set, func(v []byte) bool { return string(v) == want }) {
			t.Errorf("a recent token is not in the mask set")
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
	g.gw.respond = gwidEcho
	gwidLanes(g, t)["brokered"]()
	procRegistry.Evict(g.p.maskKey)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unmasked-bytes"))}
	rec := httptest.NewRecorder()
	g.p.relayGatewayRoute(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"m", nil), gwidHost, 443, resp, ruleSourceLLM)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "unmasked-bytes") {
		t.Fatalf("an empty masker relayed: %d %s", rec.Code, rec.Body)
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
		{"clean short body, one byte", func(w http.ResponseWriter, tok string) { short(w, 1, tok[:1]) },
			func(string) string { return hidden }},
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
		// What a benign answer looks like: JWTs start "e", so text ending in "e" loses it.
		{"a benign end equal to the token's first byte", func(w http.ResponseWriter, _ string) { short(w, 0, "the same type") },
			func(string) string { return "the same typ" + hidden }},
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
	if want := []string{"host", "port", "path_prefix,omitempty", "provider_id", "audience", "provider_uid,omitempty"}; !slices.Equal(keys, want) {
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
		"no audience": {"host": gwidHost, "port": 443, "provider_id": "p"},
		"bad prefix":  {"host": gwidHost, "port": 443, "provider_id": "p", "audience": "a", "path_prefix": "v1"},
		"unknown key": {"host": gwidHost, "port": 443, "provider_id": "p", "audience": "a", "token": "x"},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{tc}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, second := range map[string]map[string]any{
		"two audiences for a provider": {"host": gwidOther, "port": 443, "provider_id": "p", "audience": "b"},
		"two providers on a host":      {"host": gwidHost, "port": 443, "provider_id": "q", "audience": "b"},
	} {
		if _, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{"gateway_identity": []any{good, second}})); err == nil {
			t.Errorf("%s: accepted", name)
		}
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

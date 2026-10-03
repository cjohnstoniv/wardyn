// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	azHost      = "foundry.test"
	azToken     = "Bearer entra-token-for-foundry"
	azMain      = "claude-main"
	azFast      = "claude-fast"
	azOpenAI    = "gpt-main"
	azMessages  = "/anthropic/v1/messages"
	azResponses = "/openai/v1/responses"
)

// azSeen is one request that reached the fake Azure upstream.
type azSeen struct {
	method, path, auth string
	bodyLen            int
}

// azureHarness is a proxy terminating one Azure host, a fake upstream behind it that records what
// arrives (with the Authorization header), and a fake control plane counting every token redemption.
// The injector's entry is EXPIRED, so a request that reaches the injector redeems the token: a refusal
// that happens before injection leaves cpHits at zero.
type azureHarness struct {
	p      *Proxy
	mu     sync.Mutex
	seen   []azSeen
	cpHits atomic.Int32
	log    func() string
}

func azureGateCfgs(cap int64) []AzureGateConfig {
	return []AzureGateConfig{
		{Host: azHost, Route: types.AzureRouteAnthropic, Models: []string{azMain, azFast}, BodyCap: cap},
		{Host: azHost, Route: types.AzureRouteOpenAIV1, Models: []string{azOpenAI}, BodyCap: cap},
	}
}

func newAzureHarness(t *testing.T, cap int64) *azureHarness {
	t.Helper()
	h := &azureHarness{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, azSeen{r.Method, r.URL.Path, r.Header.Get("Authorization"), int(n)})
		h.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.cpHits.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: azToken, JTI: "azure-jti", ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)

	pins := append(AzureRoutePins(types.AzureRouteAnthropic), AzureRoutePins(types.AzureRouteOpenAIV1)...)
	inj := &injector{
		byHost: map[string]*injEntry{azHost: {
			grantID:    uuid.New(),
			header:     injectedHeader{name: "Authorization", value: "Bearer stale"},
			expiresAt:  time.Now().Add(-time.Hour).UnixMilli(),
			requireTLS: true,
			rule:       egress.InjectionRule{RequireTLS: true, PinRoutes: pins},
		}},
		base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	buf := &bytes.Buffer{}
	h.p = newProxy(Options{
		RunID:      uuid.New(),
		Policy:     CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{azHost + ":443"}}),
		Injector:   inj,
		Sink:       &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:   publicResolver{},
		Dial:       redirectDial(strings.TrimPrefix(upstream.URL, "http://")),
		RunToken:   newTokenSource("RUNTOK"),
		AzureGates: azureGateCfgs(cap),
	})
	h.p.mitmHosts = map[string]bool{azHost: true}
	h.p.mitmPorts = map[string]int{azHost: 443}
	h.p.mitmPlaintext = map[string]bool{plaintextKey(azHost, 443): true}
	h.log = func() string {
		_ = h.p.sink.close(context.Background())
		return buf.String()
	}
	return h
}

func (h *azureHarness) upstreamSaw() []azSeen {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]azSeen(nil), h.seen...)
}

// azRequest parses a raw request the way the MITM listener would, target and headers exactly as given.
func azRequest(t *testing.T, method, target, body string, hdr map[string]string) *http.Request {
	t.Helper()
	raw := method + " " + target + " HTTP/1.1\r\nHost: " + azHost + "\r\n"
	for k, v := range hdr {
		raw += k + ": " + v + "\r\n"
	}
	if body != "" {
		raw += "Content-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n" + body)))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	return req
}

func (h *azureHarness) do(t *testing.T, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, azRequest(t, method, target, body, hdr), azHost, 443)
	return rec
}

// mustRefuse asserts a 403 azure_route_refused, that nothing reached the upstream, that the token was
// never redeemed, and that the decision log names brokered:azure:denied.
func (h *azureHarness) mustRefuse(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"wardyn":"`+azureRouteRefused+`"`) {
		t.Errorf("%s: status %d body %s, want 403 %s", label, rec.Code, rec.Body.String(), azureRouteRefused)
	}
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Errorf("%s: the upstream saw %+v", label, seen)
	}
	if n := h.cpHits.Load(); n != 0 {
		t.Errorf("%s: the token was redeemed %d time(s) for a refused request", label, n)
	}
	if strings.Contains(rec.Body.String(), azToken) {
		t.Errorf("%s: the refusal carries the token", label)
	}
	if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureDenied+`"`) {
		t.Errorf("%s: decision log lacks %s: %s", label, ruleSourceAzureDenied, l)
	}
}

func azBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

func TestAzureGate_PinnedModelOnAnAllowedPathPasses(t *testing.T) {
	big := `{"model":"` + azMain + `","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	for _, tc := range []struct{ name, path, body string }{
		{"messages", azMessages, azBody(azMain)},
		{"messages fast alias", azMessages, azBody(azFast)},
		{"count_tokens", azMessages + "/count_tokens", azBody(azMain)},
		{"2 MiB body", azMessages, big},
		{"responses", azResponses, azBody(azOpenAI)},
		{"responses compact", azResponses + "/compact", azBody(azOpenAI)},
		{"query is ignored", azMessages + "?beta=true", azBody(azMain)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAzureHarness(t, 0)
			rec := h.do(t, http.MethodPost, tc.path, tc.body, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d body %s, want 200", rec.Code, rec.Body.String())
			}
			seen := h.upstreamSaw()
			if len(seen) != 1 || seen[0].auth != azToken || seen[0].bodyLen != len(tc.body) {
				t.Fatalf("upstream saw %+v, want one request with the injected token and the whole %d-byte body", seen, len(tc.body))
			}
			if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzure+`"`) || strings.Contains(l, ruleSourceAzureDenied) {
				t.Errorf("allowed request not logged as %s: %s", ruleSourceAzure, l)
			}
		})
	}
}

// A sandbox-supplied credential never reaches the upstream, on an allowed request.
func TestAzureGate_SandboxCredentialIsReplacedNotForwarded(t *testing.T) {
	h := newAzureHarness(t, 0)
	rec := h.do(t, http.MethodPost, azMessages, azBody(azMain), map[string]string{"Authorization": "Bearer sandbox-smuggled"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if seen := h.upstreamSaw(); len(seen) != 1 || seen[0].auth != azToken {
		t.Fatalf("upstream saw %+v, want only the injected token", seen)
	}
}

func TestAzureGate_RefusalsNeverRedeemTheToken(t *testing.T) {
	huge := `{"model":"` + azMain + `"}`
	for _, tc := range []struct {
		name, method, target, body string
		hdr                        map[string]string
	}{
		{"another deployment in the path", http.MethodPost, "/openai/deployments/other/responses", azBody(azMain), nil},
		{"the pinned deployment's chat completions", http.MethodPost, "/openai/deployments/" + azMain + "/chat/completions", azBody(azMain), nil},
		{"another model in the body", http.MethodPost, azMessages, azBody("claude-other"), nil},
		{"the other route's model on this route", http.MethodPost, azMessages, azBody(azOpenAI), nil},
		{"missing model", http.MethodPost, azMessages, `{"messages":[]}`, nil},
		{"model of the wrong case key", http.MethodPost, azMessages, `{"Model":"` + azMain + `"}`, nil},
		{"model not a string", http.MethodPost, azMessages, `{"model":["` + azMain + `"]}`, nil},
		{"two top-level model keys", http.MethodPost, azMessages, `{"model":"` + azMain + `","model":"claude-other"}`, nil},
		{"two model keys differing in case", http.MethodPost, azMessages, `{"model":"` + azMain + `","MODEL":"claude-other"}`, nil},
		{"body is not JSON", http.MethodPost, azMessages, `not json`, nil},
		{"body is an array", http.MethodPost, azMessages, `[` + huge + `]`, nil},
		{"gzip body", http.MethodPost, azMessages, azBody(azMain), map[string]string{"Content-Encoding": "gzip"}},
		{"files", http.MethodPost, "/openai/files", azBody(azOpenAI), nil},
		{"fine tuning", http.MethodPost, "/openai/fine_tuning/jobs", azBody(azOpenAI), nil},
		{"GET on an allowed path", http.MethodGet, azMessages, "", nil},
		{"websocket on responses", http.MethodGet, azResponses, "", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}},
		{"parent segment", http.MethodPost, azMessages + "/../../openai/files", azBody(azMain), nil},
		{"trailing slash", http.MethodPost, azMessages + "/", azBody(azMain), nil},
		{"empty segment", http.MethodPost, "/anthropic//v1/messages", azBody(azMain), nil},
		{"percent-encoded letter", http.MethodPost, "/anthropic/v1/%6dessages", azBody(azMain), nil},
		{"backslash", http.MethodPost, `/anthropic/v1\messages`, azBody(azMain), nil},
		{"semicolon", http.MethodPost, azMessages + ";x=1", azBody(azMain), nil},
		{"prefix of an allowed path", http.MethodPost, "/anthropic/v1/messages/batches", azBody(azMain), nil},
		{"other case", http.MethodPost, "/Anthropic/v1/messages", azBody(azMain), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAzureHarness(t, 0)
			h.mustRefuse(t, tc.name, h.do(t, tc.method, tc.target, tc.body, tc.hdr))
		})
	}
}

// Nothing the gate refuses may reach the injector's pin as a way around it: the deployment check never
// admits what the allowlist refuses, whatever the pinned set is.
func TestAzureDeploymentOutside(t *testing.T) {
	models := map[string]struct{}{azMain: {}}
	for path, want := range map[string]bool{
		"/openai/deployments/" + azMain + "/chat/completions": false,
		"/openai/deployments/other/chat/completions":          true,
		"/openai/deployments/":                                true,
		azMessages:                                            false,
	} {
		if got := azureDeploymentOutside(path, models); got != want {
			t.Errorf("azureDeploymentOutside(%q) = %v, want %v", path, got, want)
		}
	}
}

// countingBody reports how many bytes the gate pulled from it.
type countingBody struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingBody) Close() error { return nil }

// A declared length over the cap is refused with not one body byte consumed.
func TestAzureGate_DeclaredLengthOverTheCapIsRefusedUnread(t *testing.T) {
	h := newAzureHarness(t, 0)
	req := azRequest(t, http.MethodPost, azMessages, "", map[string]string{"Content-Length": strconv.Itoa(33 << 20)})
	body := &countingBody{r: strings.NewReader(azBody(azMain))}
	req.Body = body
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, azHost, 443)
	h.mustRefuse(t, "33 MiB", rec)
	if n := body.n.Load(); n != 0 {
		t.Errorf("the gate read %d body bytes of a body declared over the cap", n)
	}
}

// A chunked body that outgrows the cap is refused too, and the bytes it was charged are returned.
func TestAzureGate_ChunkedBodyOverTheCapIsRefusedAndReleased(t *testing.T) {
	const cap = 1 << 10
	h := newAzureHarness(t, cap)
	req := azRequest(t, http.MethodPost, azMessages, "", nil)
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader(`{"model":"`+azMain+`","pad":"`+strings.Repeat("x", 2*cap)+`"}`)), -1
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req, azHost, 443)
	h.mustRefuse(t, "chunked over cap", rec)
	if !h.p.azure.budget.TryAcquire(azureInflightBudget) {
		t.Error("the refused chunked request still holds budget")
	}
}

func chunkedAzureRequest(t *testing.T, body io.ReadCloser) *http.Request {
	t.Helper()
	req := azRequest(t, http.MethodPost, azMessages, "", nil)
	req.Body, req.ContentLength = body, -1
	return req
}

// The budget is charged BEFORE the read: with room for 1.5 caps, a second chunked POST is refused 503
// while the first is still mid-body, having read none of its own body, and once the first has finished a
// third proceeds. The budget is the constant; the cap is derived from it.
func TestAzureGate_InflightBudgetIsChargedBeforeTheRead(t *testing.T) {
	cap := azureInflightBudget * 2 / 3 // the budget is 1.5 x cap
	h := newAzureHarness(t, cap)

	pr, pw := io.Pipe()
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.p.serveMITMRequest(rec, chunkedAzureRequest(t, pr), azHost, 443)
		first <- rec
	}()
	// The first request holds `cap` once it has been charged; wait for that, without taking budget.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !h.p.azure.budget.TryAcquire(azureInflightBudget - cap + 1) {
			break
		}
		h.p.azure.budget.Release(azureInflightBudget - cap + 1)
		if time.Now().After(deadline) {
			t.Fatal("the first request was never charged")
		}
		time.Sleep(time.Millisecond)
	}

	second := &countingBody{r: strings.NewReader(azBody(azMain))}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, chunkedAzureRequest(t, second), azHost, 443)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"wardyn":"`+azureInflightBudgetRefused+`"`) {
		t.Fatalf("second: status %d body %s, want 503 %s", rec.Code, rec.Body.String(), azureInflightBudgetRefused)
	}
	if n := second.n.Load(); n != 0 {
		t.Errorf("the refused request had %d body bytes read before the budget refused it", n)
	}
	if n := h.cpHits.Load(); n != 0 {
		t.Errorf("the refused request redeemed the token %d time(s)", n)
	}

	go func() {
		_, _ = io.WriteString(pw, azBody(azMain))
		_ = pw.Close()
	}()
	select {
	case rec := <-first:
		if rec.Code != http.StatusOK {
			t.Fatalf("first: status %d body %s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first request never finished")
	}
	if rec := h.do(t, http.MethodPost, azMessages, azBody(azMain), nil); rec.Code != http.StatusOK {
		t.Fatalf("third, after the first released: status %d body %s", rec.Code, rec.Body.String())
	}
	if !h.p.azure.budget.TryAcquire(azureInflightBudget) {
		t.Error("budget not fully returned after the round trips")
	}
}

// A body shorter than the cap it was charged at gives the over-charge back as soon as its size is known,
// so one small chunked request does not hold a whole cap while its round trip runs.
func TestAzureGate_OverChargeIsReleasedOnceTheSizeIsKnown(t *testing.T) {
	h := newAzureHarness(t, 0)
	rec := httptest.NewRecorder()
	req := chunkedAzureRequest(t, io.NopCloser(strings.NewReader(azBody(azMain))))
	_, rel, ok := h.p.gateAzure(rec, req, azHost, 443, ruleSourceLLMMITM)
	if !ok {
		t.Fatalf("gate refused: %s", rec.Body.String())
	}
	defer rel()
	// Only the real size stays charged.
	if !h.p.azure.budget.TryAcquire(azureInflightBudget - int64(len(azBody(azMain)))) {
		t.Fatal("more than the real body size is still charged")
	}
}

// The plain forward lane never runs the gate: every absolute-form request to the host is refused, with
// the token neither redeemed nor on the wire.
func TestAzureGate_PlainLaneRefusesEveryRequest(t *testing.T) {
	for _, target := range []string{
		"https://" + azHost + "/openai/files",
		"https://" + azHost + azMessages,
		"http://" + azHost + azResponses,
	} {
		h := newAzureHarness(t, 0)
		rec := httptest.NewRecorder()
		h.p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(azBody(azMain))))
		h.mustRefuse(t, "plain "+target, rec)
	}
}

// A CONNECT the proxy will not terminate is refused rather than tunnelled past the gate.
func TestAzureGate_UnterminatedConnectIsRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dialed := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			dialed <- struct{}{}
			_ = c.Close()
		}
	}()
	h := newAzureHarness(t, 0)
	h.p.mitmHosts, h.p.mitmPorts = nil, nil // no CA: nothing terminates
	h.p.dial = redirectDial(ln.Addr().String())
	srv := httptest.NewServer(h.p)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "CONNECT "+azHost+":443 HTTP/1.1\r\nHost: "+azHost+":443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT status = %d, want 403", resp.StatusCode)
	}
	select {
	case <-dialed:
		t.Error("the proxy opened a tunnel to the Azure host")
	case <-time.After(100 * time.Millisecond):
	}
	srv.Close()
	if l := h.log(); !strings.Contains(l, `"`+ruleSourceAzureDenied+`"`) {
		t.Errorf("no %s row: %s", ruleSourceAzureDenied, l)
	}
}

// The brokered route on an Azure run: the Azure host is no LLMUpstreams entry, so /wardyn/llm/* dials the
// public vendor host, where the run holds no credential. No Authorization header reaches the upstream.
func TestAzureGate_BrokeredRouteHoldsNoCredentialForAzurePaths(t *testing.T) {
	for _, path := range []string{"/wardyn/llm/openai/files", "/wardyn/llm/anthropic/v1/messages"} {
		h := newAzureHarness(t, 0)
		rec := httptest.NewRecorder()
		h.p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, path, strings.NewReader(azBody(azMain))))
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_llm_credential") {
			t.Errorf("%s: status %d body %s, want 404 no_llm_credential", path, rec.Code, rec.Body.String())
		}
		if seen := h.upstreamSaw(); len(seen) != 0 {
			t.Errorf("%s: the upstream saw %+v", path, seen)
		}
		if n := h.cpHits.Load(); n != 0 {
			t.Errorf("%s: the token was redeemed %d time(s)", path, n)
		}
	}
}

// The brokered route honours a method-and-path pin on the rule of the upstream it dials (a configured
// gateway): a request outside the pin reaches the upstream with no injected header and with the
// sandbox's credential headers stripped; a request inside it is injected.
func TestAzureGate_BrokeredRouteHonoursTheRulePin(t *testing.T) {
	const gwHost = "llm-gateway.corp.internal"
	gw := captureUpstream(t, true, `{"ok":true}`)
	res := fakeResolver{m: map[string][]net.IP{gwHost: ips("10.40.1.5")}}
	inj := &injector{byHost: map[string]*injEntry{gwHost: {
		grantID: uuid.New(),
		header:  injectedHeader{name: "X-Api-Key", value: "BROKERED-KEY"},
		rule:    egress.InjectionRule{PinRoutes: []egress.PinRoute{{Method: http.MethodPost, Path: "/v1/messages"}}},
	}}}
	p, _ := gatewayProxy(t, "https://"+gwHost+"/v1", res, upstreamAddr(gw.srv), inj)

	send := func(method, path string) {
		t.Helper()
		gw.reached, gw.header = false, nil
		req := mustLocalReq(t, method, llmAnthropicPrefix+path, strings.NewReader(`{"hi":1}`))
		req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
		req.Header.Set("X-Api-Key", "SANDBOX-KEY")
		p.ServeHTTP(httptest.NewRecorder(), req)
		if !gw.reached {
			t.Fatalf("%s %s: the gateway was never reached", method, path)
		}
	}

	send(http.MethodPost, "messages")
	if got := gw.header.Get("X-Api-Key"); got != "BROKERED-KEY" {
		t.Errorf("inside the pin: X-Api-Key = %q, want the brokered credential", got)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "files"}, {http.MethodGet, "messages"}, {http.MethodPost, "messages/count_tokens"},
	} {
		send(c.method, c.path)
		if got := gw.header.Get("X-Api-Key"); got != "" {
			t.Errorf("%s %s outside the pin: X-Api-Key = %q, want none", c.method, c.path, got)
		}
		if got := gw.header.Get("Authorization"); got != "" {
			t.Errorf("%s %s outside the pin: sandbox Authorization %q reached the gateway", c.method, c.path, got)
		}
	}
}

// A host with no Azure gate is untouched by it.
func TestAzureGate_UngatedHostStandsAside(t *testing.T) {
	h := newAzureHarness(t, 0)
	src, release, ok := h.p.gateAzure(httptest.NewRecorder(), azRequest(t, http.MethodGet, "/anything", "", nil), "elsewhere.test", 443, "x")
	defer release()
	if !ok || src != "x" {
		t.Fatalf("gateAzure on an ungated host = %q, %v, want it to stand aside", src, ok)
	}
}

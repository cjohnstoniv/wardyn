// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func usageFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../modelusage/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// usageRows is every llm:usage row in a sink's mirrored log.
func usageRows(t *testing.T, buf *bytes.Buffer) []egress.DecisionLog {
	t.Helper()
	var out []egress.DecisionLog
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if d.RuleSource == ruleSourceLLMUsage {
			out = append(out, d)
		}
	}
	return out
}

// meteredResponse runs one answer through meterUsage for a POST to path and returns what the relay
// read, plus the sink's log.
func meteredResponse(t *testing.T, p *Proxy, path string, hdr http.Header, body []byte, read func(io.Reader) []byte) ([]byte, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	p.sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	r := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+path, strings.NewReader(`{}`))
	resp := &http.Response{StatusCode: 200, Header: hdr, Body: io.NopCloser(bytes.NewReader(body))}
	p.meterUsage(r, anthropicHost, 443, resp, "")
	got := read(resp.Body)
	_ = resp.Body.Close()
	return got, buf
}

func readAllOneByte(r io.Reader) []byte {
	b, _ := io.ReadAll(iotest.OneByteReader(r))
	return b
}

func sseHeader(enc string) http.Header {
	h := http.Header{"Content-Type": {"text/event-stream"}}
	if enc != "" {
		h.Set("Content-Encoding", enc)
	}
	return h
}

// The meter is a tee: the relay reads exactly the upstream's bytes, compressed or not, one byte at a
// time, and one row carries the stream's counts. gzip and deflate are inflated by the meter itself;
// a coding it cannot inflate is uncounted, undecodable, and still relayed whole.
func TestUsageMeter_ByteIdentityTee(t *testing.T) {
	sse := usageFixture(t, "anthropic_stream.sse")
	var gz, zl bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(sse)
	_ = gw.Close()
	zw := zlib.NewWriter(&zl)
	_, _ = zw.Write(sse)
	_ = zw.Close()
	counted := egress.ModelUsage{Vendor: "anthropic", Host: anthropicHost, Model: "claude-opus-5-5", Input: 25, Output: 15, State: "counted"}
	undecodable := egress.ModelUsage{Vendor: "anthropic", Host: anthropicHost, State: "uncounted", Reason: "undecodable"}
	for _, tc := range []struct {
		name, enc string
		wire      []byte
		want      egress.ModelUsage
	}{
		{"plain", "", sse, counted},
		{"gzip", "gzip", gz.Bytes(), counted},
		{"deflate", "deflate", zl.Bytes(), counted},
		{"br is not inflated", "br", []byte("\x1b\x00opaque brotli bytes"), undecodable},
		{"corrupt gzip", "gzip", append([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff}, "not deflate"...), undecodable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProxy(Options{RunID: uuid.New()})
			got, buf := meteredResponse(t, p, "/v1/messages", sseHeader(tc.enc), tc.wire, readAllOneByte)
			if !bytes.Equal(got, tc.wire) {
				t.Fatalf("the relay read %d bytes that differ from the upstream's %d", len(got), len(tc.wire))
			}
			rows := usageRows(t, buf)
			if len(rows) != 1 {
				t.Fatalf("%d usage rows, want 1", len(rows))
			}
			u := *rows[0].Usage
			if u.CallID == uuid.Nil {
				t.Fatal("the row has no call id")
			}
			u.CallID = uuid.Nil
			if u != tc.want {
				t.Fatalf("usage = %+v, want %+v", u, tc.want)
			}
			if rows[0].Decision != egress.Allow || rows[0].Request.Host != anthropicHost || rows[0].Request.Path != "/v1/messages" {
				t.Fatalf("row = %+v", rows[0])
			}
		})
	}
}

// A stream closed before its final usage event is partial (input from message_start), whether or
// not it was compressed: the inflater never turns a cut into undecodable.
func TestUsageMeter_CutStreamIsPartial(t *testing.T) {
	sse := usageFixture(t, "anthropic_stream.sse")
	half := sse[:bytes.Index(sse, []byte("event: content_block_stop"))]
	// The compressed prefix up to a flush inflates to exactly the plain prefix.
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write(half)
	_ = gw.Flush()
	flushed := gz.Len()
	_, _ = gw.Write(sse[len(half):])
	_ = gw.Close()
	for name, tc := range map[string]struct {
		enc  string
		wire []byte
		keep int
	}{
		"plain": {"", sse, len(half)},
		"gzip":  {"gzip", gz.Bytes(), flushed},
	} {
		t.Run(name, func(t *testing.T) {
			p := newProxy(Options{RunID: uuid.New()})
			_, buf := meteredResponse(t, p, "/v1/messages", sseHeader(tc.enc), tc.wire, func(r io.Reader) []byte {
				b := make([]byte, tc.keep)
				_, _ = io.ReadFull(r, b) // the client hangs up here; the deferred Close follows
				return b
			})
			rows := usageRows(t, buf)
			if len(rows) != 1 || rows[0].Usage.State != "partial" || rows[0].Usage.Reason != "stream_cut" || rows[0].Usage.Input != 25 {
				t.Fatalf("rows = %+v, want one partial stream_cut row with input 25", rows)
			}
		})
	}
}

// usageUpstream answers each model API with usage the table asserts, and records the
// Accept-Encoding each request arrived with.
type usageUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	ae  []string
}

func newUsageUpstream(t *testing.T, useTLS bool) *usageUpstream {
	t.Helper()
	u := &usageUpstream{}
	responses := usageFixture(t, "openai_responses_stream.sse")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		u.mu.Lock()
		u.ae = append(u.ae, r.Header.Get("Accept-Encoding"))
		u.mu.Unlock()
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/messages"):
			_, _ = io.WriteString(w, `{"type":"message","model":"claude-x","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":3}}`)
		case strings.HasSuffix(p, "/responses"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(responses)
		case strings.HasSuffix(p, "/invoke"):
			w.Header().Set("X-Amzn-Bedrock-Input-Token-Count", "17")
			w.Header().Set("X-Amzn-Bedrock-Output-Token-Count", "9")
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(p, "/converse"):
			_, _ = io.WriteString(w, `{"output":{},"usage":{"inputTokens":5,"outputTokens":6,"totalTokens":11}}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	if useTLS {
		u.srv = httptest.NewTLSServer(h)
	} else {
		u.srv = httptest.NewServer(h)
	}
	t.Cleanup(u.srv.Close)
	return u
}

func (u *usageUpstream) acceptEncodings() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.ae...)
}

var (
	usageMessages  = egress.ModelUsage{Vendor: "anthropic", Model: "claude-x", Input: 11, Output: 7, CacheRead: 3, State: "counted"}
	usageResponses = egress.ModelUsage{Vendor: "openai", Model: "gpt-5-codex", Input: 37, Output: 11, State: "counted"}
	usageInvoke    = egress.ModelUsage{Vendor: "bedrock", Model: "m", Input: 17, Output: 9, State: "counted"}
	usageConverse  = egress.ModelUsage{Vendor: "bedrock", Model: "m", Input: 5, Output: 6, State: "counted"}
)

// One request through a provider kind's real route, and the usage row it must leave.
type usageCall struct {
	mitmHost string // "" == the brokered route
	path     string
	body     string
	want     egress.ModelUsage
}

// Every provider kind's model call reaches the meter on the route its dispatch uses (the proxy
// options mirror TestAllowedModels_EveryProviderKindChecked): each leaves exactly one counted usage
// row naming its host, and the upstream sees the sandbox's own Accept-Encoding.
func TestUsageMeter_EveryProviderKind(t *testing.T) {
	const (
		gateway   = "llm-gateway.corp.internal"
		routeThru = "claude-route.corp.internal"
		foundry   = "foundry.corp.internal"
		vpce      = "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com"
	)
	msg, resp := `{"model":"claude-x","messages":[]}`, `{"model":"gpt-5-codex","input":"x"}`
	keys := map[string]injectedHeader{
		anthropicHost: {name: "X-Api-Key", value: "K"}, openaiHost: {name: "Authorization", value: "Bearer K"},
		gateway: {name: "X-Api-Key", value: "K"}, vpce: {name: "Authorization", value: "Bearer K"},
		foundry: {name: "Api-Key", value: "K"}, routeThru: {name: "Authorization", value: "Bearer K"},
	}
	for _, tc := range []struct {
		kind  string
		opts  Options
		calls []usageCall
	}{
		{"anthropic_api_key", Options{MITMLLM: true}, []usageCall{
			{"", llmAnthropicPrefix + "v1/messages", msg, usageMessages},
			{anthropicHost, "/v1/messages", msg, usageMessages}}},
		{"anthropic_subscription via MITM", Options{MITMLLM: true, MITMHosts: []string{routeThru + ":443"},
			LLMChannelHosts: map[string]string{routeThru: anthropicHost}}, []usageCall{
			{anthropicHost, "/v1/messages", msg, usageMessages},
			{routeThru, "/anthropic/v1/messages", msg, usageMessages}}},
		{"openai_api_key /responses", Options{MITMLLM: true}, []usageCall{
			{"", llmOpenAIPrefix + "v1/responses", resp, usageResponses},
			{openaiHost, "/v1/responses", resp, usageResponses}}},
		{"bedrock_bearer", Options{MITMLLM: true, MITMHosts: []string{vpce + ":443"}}, []usageCall{
			{vpce, "/model/m/invoke", `{}`, usageInvoke},
			{vpce, "/model/m/converse", `{}`, usageConverse}}},
		{"azure_foundry with a key", Options{MITMLLM: true, MITMHosts: []string{foundry + ":443"},
			LLMChannelHosts: map[string]string{foundry: anthropicHost}}, []usageCall{
			{foundry, "/anthropic/v1/messages", msg, usageMessages}}},
		{"custom_endpoint", Options{MITMLLM: true, MITMHosts: []string{gateway + ":443"},
			LLMUpstreams: map[string]string{anthropicHost: "https://" + gateway + "/anthropic"}}, []usageCall{
			{"", llmAnthropicPrefix + "v1/messages", msg, usageMessages},
			{gateway, "/anthropic/v1/messages", msg, usageMessages}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			up := newUsageUpstream(t, true)
			buf := &bytes.Buffer{}
			opts := tc.opts
			opts.RunID, opts.CA, opts.Resolver = uuid.New(), fakeCA(t), publicResolver{}
			opts.Dial, opts.TLSClientConfig = redirectDial(upstreamAddr(up.srv)), testInsecureTLSConfig
			opts.Sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
			opts.Injector = staticInj(keys)
			opts.Policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost, openaiHost, gateway, routeThru, foundry, vpce}})
			runUsageCalls(t, newProxy(opts), up, buf, tc.calls)
		})
	}
	t.Run("local_model", func(t *testing.T) {
		up := newUsageUpstream(t, false)
		p, buf := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{Models: []string{"claude-x"}})
		runUsageCalls(t, p, up, buf, []usageCall{{"", llmAnthropicPrefix + "v1/messages", msg, usageMessages}})
	})
	t.Run("azure_foundry with Entra (gated)", func(t *testing.T) {
		up := newUsageUpstream(t, false)
		p, buf := usageAzureProxy(t, up)
		runUsageCalls(t, p, up, buf, []usageCall{
			{azHost, azMessages, azBody(azMain), usageMessages},
			{azHost, azResponses, azBody(azOpenAI), usageResponses}})
	})
	t.Run("bedrock_sso re-signed at the proxy", func(t *testing.T) {
		h := newSigV4Harness(t)
		up := newUsageUpstream(t, false)
		h.respond = up.srv.Config.Handler.ServeHTTP
		buf := &bytes.Buffer{}
		h.p.sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
		for _, path := range []string{"/model/m/converse", "/model/m/invoke"} {
			rec := h.send(t, sandboxRequest(t, http.MethodPost, sigv4Bedrock, path, `{}`, nil, "", "bedrock", "us-east-1", ""), sigv4Bedrock)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d body %s", path, rec.Code, rec.Body.String())
			}
		}
		checkUsageRows(t, buf, []string{sigv4Bedrock, sigv4Bedrock}, []egress.ModelUsage{usageConverse, usageInvoke})
	})
	t.Run("bedrock_sso at a Bedrock gateway", func(t *testing.T) {
		g := newSGWHarness(t)
		up := newUsageUpstream(t, false)
		g.respond = up.srv.Config.Handler.ServeHTTP
		buf := &bytes.Buffer{}
		g.p.sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
		rec := g.send(t, sgwRequest(t, sgwHost, sgwPrefix+"/model/m/converse", `{}`, map[string]string{"Content-Type": "application/json"}), sgwHost)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		checkUsageRows(t, buf, []string{sgwHost}, []egress.ModelUsage{usageConverse})
		// The row joins the gateway's log line on the attempt's call id, as the allow row does.
		if row := usageRows(t, buf)[0]; row.GatewayCallID == "" || row.GatewayCallID != upstreamCallIDOf(t, buf) {
			t.Fatalf("usage row gateway call id %q, want the allow row's", row.GatewayCallID)
		}
	})
}

// upstreamCallIDOf is the gateway call id of the last allow row that is not a usage row.
func upstreamCallIDOf(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	id := ""
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var d egress.DecisionLog
		if json.Unmarshal([]byte(line), &d) == nil && d.Decision == egress.Allow && d.RuleSource != ruleSourceLLMUsage {
			id = d.GatewayCallID
		}
	}
	return id
}

func runUsageCalls(t *testing.T, p *Proxy, up *usageUpstream, buf *bytes.Buffer, calls []usageCall) {
	t.Helper()
	var hosts []string
	var want []egress.ModelUsage
	for _, c := range calls {
		rec := httptest.NewRecorder()
		if c.mitmHost == "" {
			req := mustLocalReq(t, http.MethodPost, c.path, strings.NewReader(c.body))
			req.Header.Set("Accept-Encoding", "gzip, br")
			p.ServeHTTP(rec, req)
		} else {
			req := httptest.NewRequest(http.MethodPost, "https://"+c.mitmHost+c.path, strings.NewReader(c.body))
			req.Header.Set("Accept-Encoding", "gzip, br")
			p.serveMITMRequest(rec, req, c.mitmHost, 443)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d body %s", c.mitmHost, c.path, rec.Code, rec.Body.String())
		}
		host := c.mitmHost
		if host == "" {
			host = upstreamHostOf(t, buf)
		}
		hosts, want = append(hosts, host), append(want, c.want)
	}
	for _, ae := range up.acceptEncodings() {
		if ae != "gzip, br" {
			t.Errorf("the upstream saw Accept-Encoding %q, want the sandbox's own %q", ae, "gzip, br")
		}
	}
	checkUsageRows(t, buf, hosts, want)
}

// upstreamHostOf is the host of the last allow row: the brokered route's upstream.
func upstreamHostOf(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	host := ""
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var d egress.DecisionLog
		if json.Unmarshal([]byte(line), &d) == nil && d.Decision == egress.Allow && d.RuleSource != ruleSourceLLMUsage {
			host = d.Request.Host
		}
	}
	return host
}

func checkUsageRows(t *testing.T, buf *bytes.Buffer, hosts []string, want []egress.ModelUsage) {
	t.Helper()
	rows := usageRows(t, buf)
	if len(rows) != len(want) {
		t.Fatalf("%d usage rows, want %d: %s", len(rows), len(want), buf.String())
	}
	ids := map[uuid.UUID]bool{}
	for i, row := range rows {
		u := *row.Usage
		if u.CallID == uuid.Nil || ids[u.CallID] {
			t.Fatalf("row %d: call id %s is missing or repeated", i, u.CallID)
		}
		ids[u.CallID] = true
		w := want[i]
		w.CallID, w.Host = u.CallID, hosts[i]
		if u != w || row.Request.Host != hosts[i] {
			t.Errorf("row %d: usage %+v (request host %q), want %+v", i, u, row.Request.Host, w)
		}
	}
}

// usageAzureProxy is newAzureHarness's proxy with the usage upstream behind it.
func usageAzureProxy(t *testing.T, up *usageUpstream) (*Proxy, *bytes.Buffer) {
	t.Helper()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: azToken, JTI: "azure-jti", ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli()})
	}))
	t.Cleanup(cp.Close)
	pins := append(AzureRoutePins(types.AzureRouteAnthropic), AzureRoutePins(types.AzureRouteOpenAIV1)...)
	inj := &injector{
		byHost: map[string]*injEntry{azHost: {grantID: uuid.New(), header: injectedHeader{name: "Authorization", value: "Bearer stale"},
			expiresAt: time.Now().Add(-time.Hour).UnixMilli(), requireTLS: true, rule: egress.InjectionRule{RequireTLS: true, PinRoutes: pins}}},
		base: cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{azHost + ":443"}}),
		Injector: inj, Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}, Resolver: publicResolver{},
		Dial: redirectDial(upstreamAddr(up.srv)), RunToken: newTokenSource("RUNTOK"), AzureGates: azureGateCfgs(0),
	})
	p.mitmHosts = map[string]bool{azHost: true}
	p.mitmPorts = map[string]int{azHost: 443}
	p.mitmPlaintext = map[string]bool{plaintextKey(azHost, 443): true}
	return p, buf
}

// With metering off the answer is not wrapped and no usage row is sent; on or off, the request's
// headers go upstream as the sandbox sent them.
func TestUsageMeter_OffLeavesTheAnswerAlone(t *testing.T) {
	for _, off := range []bool{false, true} {
		up := newUsageUpstream(t, true)
		buf := &bytes.Buffer{}
		p := newProxy(Options{RunID: uuid.New(), MITMLLM: true, CA: fakeCA(t), Resolver: publicResolver{},
			Dial: redirectDial(upstreamAddr(up.srv)), TLSClientConfig: testInsecureTLSConfig,
			Sink:   &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
			Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}})})
		p.meterOff = off
		runUsageCallsN(t, p, up, buf, map[bool]int{false: 1, true: 0}[off])

		body := io.NopCloser(strings.NewReader("x"))
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: body}
		p.meterUsage(httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", nil), anthropicHost, 443, resp, "")
		if wrapped := resp.Body != body; wrapped == off {
			t.Errorf("off=%v: wrapped=%v", off, wrapped)
		}
		if _, _, metered := p.usageShape(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); metered == off {
			t.Errorf("off=%v: usageShape metered=%v, so admission reserves the wrong room", off, metered)
		}
	}
}

func runUsageCallsN(t *testing.T, p *Proxy, up *usageUpstream, buf *bytes.Buffer, wantRows int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(`{"model":"claude-x"}`))
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, req, anthropicHost, 443)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	if ae := up.acceptEncodings(); len(ae) != 1 || ae[0] != "gzip, br" {
		t.Fatalf("the upstream saw Accept-Encoding %q, want the sandbox's own", ae)
	}
	if n := len(usageRows(t, buf)); n != wantRows {
		t.Fatalf("%d usage rows, want %d", n, wantRows)
	}
}

// Only 2xx answers to metered requests are metered: a refusal, a count_tokens call, a batch call and
// a GET leave no usage row.
func TestUsageMeter_WhatIsNotMetered(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New()})
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/v1/messages", 429},
		{http.MethodPost, "/v1/messages/count_tokens", 200},
		{http.MethodPost, "/v1/messages/batches", 200},
		{http.MethodGet, "/v1/messages", 200},
	} {
		buf := &bytes.Buffer{}
		p.sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":1}}`))}
		p.meterUsage(httptest.NewRequest(tc.method, "https://"+anthropicHost+tc.path, nil), anthropicHost, 443, resp, "")
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if n := len(usageRows(t, buf)); n != 0 {
			t.Errorf("%s %s %d: %d usage rows, want none", tc.method, tc.path, tc.status, n)
		}
	}
}

// worstUsageRow is a usage row with every string at its cut and made of bytes JSON escapes to six.
func worstUsageRow(p *Proxy) egress.DecisionLog {
	long := strings.Repeat("\x01", 4096)
	r := httptest.NewRequest(http.MethodPost, "https://x/model/"+strings.Repeat("a", 4096)+"/invoke", nil)
	dl := decisionLog(p.reqOf(r, long, 443), egress.Allow, ruleSourceLLMUsage)
	dl.Request.Host = cutUsageField(long, usageFieldBytes)
	dl.Request.Path = cutUsageField(long, usagePathBytes)
	dl.GatewayCallID = uuid.NewString()
	max := int64(1<<63 - 1)
	dl.Usage = &egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: dl.Request.Host, Model: cutUsageField(long, usageFieldBytes),
		Input: max, Output: max, CacheRead: max, CacheWrite: max, Reasoning: max, State: "uncounted", Reason: "stream_without_usage"}
	return dl
}

// The largest usage row, as the spool charges it (its JSON plus 1 KiB), fits the room admission adds,
// and that room fits what a reservation keeps after its first row.
func TestUsageRecordFitsItsReservation(t *testing.T) {
	b, err := json.Marshal(worstUsageRow(newProxy(Options{RunID: uuid.New()})))
	if err != nil {
		t.Fatal(err)
	}
	if size := int64(len(maskDecisionBytes(b))) + 1024; size > spoolUsageRecordBytes {
		t.Fatalf("a usage row charges %d bytes, over spoolUsageRecordBytes %d", size, spoolUsageRecordBytes)
	}
	if spoolUsageRecordBytes > spoolFollowUpBytes {
		t.Fatal("spoolUsageRecordBytes is over spoolFollowUpBytes, so the room is not kept after the allow row")
	}
}

// On the runner's local spool, a metered request reserves room for its usage row: after the largest
// allow row its reservation could hold, the worst usage row is still charged to the reservation (so
// it never meets a full spool, which would latch the audit gap). A request the meter cannot wrap, or
// any request with metering off, reserves no such room.
func TestUsageMeter_SpoolReservationFitsAllowAndUsage(t *testing.T) {
	runID := uuid.New()
	sink, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.close(context.Background()) })
	p := newProxy(Options{RunID: runID, Sink: sink})
	metered := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", nil)
	plain := httptest.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/files", nil)
	if d := p.spoolReservationBytes(metered) - p.spoolReservationBytes(plain); d != spoolUsageRecordBytes+int64(len("messages")-len("files"))*12 {
		t.Fatalf("a metered request reserves %d more than a plain one, want the usage room", d)
	}
	p.meterOff = true
	if a, b := p.spoolReservationBytes(metered), p.spoolReservationBytes(plain); a-b != int64(len("messages")-len("files"))*12 {
		t.Fatalf("metering off still reserves usage room: %d vs %d", a, b)
	}
	p.meterOff = false

	need := p.spoolReservationBytes(metered)
	id, err := sink.reserve(context.Background(), need)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(metered.Context(), spoolTicketKey{}, id)
	metered = metered.WithContext(ctx)
	// The largest allow row the request's own bound covers: a cause padded until the row is charged
	// exactly the reservation without the usage room.
	allow := decisionLog(p.reqOf(metered, anthropicHost, 443), egress.Allow, ruleSourceLLMMITM)
	b, _ := json.Marshal(allow)
	allow.Cause = strings.Repeat("c", int(need-spoolUsageRecordBytes)-len(b)-1024-len(`,"cause":""`))
	sink.emit(allow)
	sink.mu.Lock()
	left := sink.reservations[id].bytes
	sink.mu.Unlock()
	if left < spoolUsageRecordBytes {
		t.Fatalf("after the allow row the reservation keeps %d bytes, under the usage room %d", left, spoolUsageRecordBytes)
	}
	row := worstUsageRow(p)
	row.Request.AuditReservation = id
	rb, _ := json.Marshal(row)
	sink.emit(row)
	sink.mu.Lock()
	after, lost := sink.reservations[id].bytes, sink.localErr
	sink.mu.Unlock()
	if lost != nil || left-after != int64(len(maskDecisionBytes(rb)))+1024 {
		t.Fatalf("the usage row was not charged to its reservation: kept %d then %d, latched %v", left, after, lost)
	}
	depth, _, _, err := sink.local.Stats(context.Background())
	if err != nil || depth != 2 {
		t.Fatalf("spool depth %d (%v), want the allow and the usage rows", depth, err)
	}
}

// A default config carries no metering key, so it loads on the proxy before this one; only the
// admin's opt-out writes disable_model_usage, which that proxy refuses.
func TestUsageMeter_DefaultConfigLoadsOnThePreviousProxy(t *testing.T) {
	var fields []reflect.StructField
	for f := range reflect.TypeFor[Config]().Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "disable_model_usage" {
			fields = append(fields, f)
		}
	}
	decodePrevious := func(b []byte) error {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		return dec.Decode(reflect.New(reflect.StructOf(fields)).Interface())
	}
	def, _ := json.Marshal(Config{RunID: uuid.New()})
	if bytes.Contains(def, []byte("model_usage")) {
		t.Fatalf("the default config carries a metering key: %s", def)
	}
	if err := decodePrevious(def); err != nil {
		t.Fatalf("the default config must load on the previous proxy: %v", err)
	}
	off, _ := json.Marshal(Config{RunID: uuid.New(), DisableModelUsage: true})
	if err := decodePrevious(off); err == nil || !strings.Contains(err.Error(), "disable_model_usage") {
		t.Fatalf("the previous proxy must refuse the opt-out key, got %v", err)
	}
	for _, name := range []string{"v0.9.3.txt"} {
		b, err := os.ReadFile(configKeysDir + name)
		if err != nil {
			t.Fatal(err)
		}
		if slicesContainsLine(string(b), "disable_model_usage") {
			t.Errorf("%s lists disable_model_usage, but that release predates it", name)
		}
	}
}

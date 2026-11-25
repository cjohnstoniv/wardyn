// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const inboundToken = "rly-inbound-0123456789abcdefghijklmnopqrstuvwxyzAB"

func inboundDigest() string {
	sum := sha256.Sum256([]byte(inboundToken))
	return hex.EncodeToString(sum[:])
}

// hostStub stands in for wardyn-mcp-host: it records what it was sent and answers with reply.
type hostStub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  int
	path  string
	hdr   http.Header
	reply func(w http.ResponseWriter)
}

func newHostStub(t *testing.T) *hostStub {
	t.Helper()
	h := &hostStub{reply: func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`) }}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.hits++
		h.path, h.hdr = r.URL.RequestURI(), r.Header.Clone()
		reply := h.reply
		h.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// newInbound is an MCP sandbox proxy's relay handler in front of host, with policy's known secrets.
func newInbound(t *testing.T, host *hostStub, policy types.RunPolicySpec) (*mcpRelayInbound, *bytes.Buffer, *Proxy) {
	t.Helper()
	buf := &bytes.Buffer{}
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(policy), Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}})
	p.maskKey = uuid.New() // this test's own secrets, apart from every other test's
	t.Cleanup(func() { procRegistry.Evict(p.maskKey) })
	cfg := &Config{Policy: policy, MCPRelayInbound: &MCPRelayInboundConfig{Listen: "127.0.0.1:0", TokenSHA256: inboundDigest(), Upstream: upstreamAddr(host.srv)}}
	relay := newMCPRelayInbound(p, cfg, &http.Server{})
	return relay.Handler.(*mcpRelayInbound), buf, p
}

// TestMCPRelayInboundWrongTokenRefused: the relay token is checked by its digest; a wrong one is
// refused with 403 and relay:mcp:unbound and reaches nothing, and the right one is stripped
// before the call goes to wardyn-mcp-host, recorded as relay:mcp with the caller's call id.
func TestMCPRelayInboundWrongTokenRefused(t *testing.T) {
	host := newHostStub(t)
	h, buf, _ := newInbound(t, host, types.RunPolicySpec{})
	for _, tok := range []string{"wrong-0123456789abcdefghijklmnopqrstuvwxyzABCDEFG", inboundToken[:len(inboundToken)-1], strings.ToUpper(inboundToken)} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, routeMCPRelay+tok, strings.NewReader(mcpInit)))
		if rec.Code != http.StatusForbidden || host.hits != 0 {
			t.Fatalf("token %q: %d, host hits %d; want 403 and nothing sent", tok, rec.Code, host.hits)
		}
		wantDecision(t, buf, egress.Deny, ruleSourceRelayMCPUnbound)
	}
	req := httptest.NewRequest(http.MethodPost, routeMCPRelay+inboundToken, strings.NewReader(mcpInit))
	req.Header.Set(gatewayCallIDHeader, "call-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || host.hits != 1 || host.path != mcpHostPath {
		t.Fatalf("right token: %d, host hits %d at %q; want the call at %s", rec.Code, host.hits, host.path, mcpHostPath)
	}
	for k, vs := range host.hdr {
		if strings.Contains(k+strings.Join(vs, ","), inboundToken) {
			t.Fatalf("host header %s carries the relay token", k)
		}
	}
	d := wantDecision(t, buf, egress.Allow, ruleSourceRelayMCP)
	if d.GatewayCallID != "call-1" || strings.Contains(buf.String(), inboundToken) {
		t.Fatalf("row %+v, want the caller's call id and no token", d)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, routeMCPRelay+inboundToken, nil))
	if rec.Code != http.StatusMethodNotAllowed || host.hits != 1 {
		t.Fatalf("PUT: %d, host hits %d; want 405 and nothing sent", rec.Code, host.hits)
	}
}

// TestMCPRelayInboundBindsRelayAddrOnly: the relay listener binds its address only, serves the relay
// path alone (a CONNECT, an absolute-URI request and any other path get 404), and the forward
// listener never serves the relay path.
func TestMCPRelayInboundBindsRelayAddrOnly(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(cp.Close)
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := hold.Addr().String()
	_ = hold.Close()
	cfg, err := LoadConfigBytes(fmt.Appendf(nil, `{"run_id":%q,"control_plane_url":%q,"run_token":"t","listen":"127.0.0.1:0",`+
		`"control_plane_audience":"wardyn-mcp-sandbox","mcp_relay_inbound":{"listen":%q,"token_sha256":%q,"upstream":"10.88.0.3:8765"}}`,
		uuid.New(), cp.URL, relayAddr, inboundDigest()))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	listening := make(chan struct{})
	srv.OnListening(func() { close(listening) })
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy never listened")
	}
	raw := func(addr, request string) string {
		t.Helper()
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return "dial: " + err.Error()
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, request)
		line, _ := bufio.NewReader(c).ReadString('\n')
		return line
	}
	wrongTok := "POST " + routeMCPRelay + "wrong-token HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
	if got := raw(relayAddr, wrongTok); !strings.Contains(got, " 403 ") {
		t.Fatalf("relay listener, wrong token: %q, want 403 from the relay handler", got)
	}
	for name, req := range map[string]string{
		"CONNECT":      "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"absolute URI": "POST http://example.com" + routeMCPRelay + "wrong-token HTTP/1.1\r\nHost: example.com\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		"other path":   "GET /wardyn/v1/approvals/x HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
		"forward use":  "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n",
	} {
		if got := raw(relayAddr, req); !strings.Contains(got, " 404 ") {
			t.Errorf("relay listener, %s: %q, want 404", name, got)
		}
	}
	_, port, _ := net.SplitHostPort(relayAddr)
	if got := raw(net.JoinHostPort("127.0.0.2", port), wrongTok); !strings.HasPrefix(got, "dial: ") {
		t.Errorf("another address at the relay port answered %q, want no listener", got)
	}
	if got := raw(srv.BoundAddr().String(), wrongTok); !strings.Contains(got, " 404 ") {
		t.Errorf("forward listener, relay path: %q, want 404", got)
	}
}

// TestMCPRelayScrubsOwnSecrets: every answer, streamed ones included, leaves with the sandbox's own
// secrets (its env and file secrets from the known-secret list, and the header credential this proxy
// holds) masked, whatever the run's response-scrub setting.
func TestMCPRelayScrubsOwnSecrets(t *testing.T) {
	const (
		envSecret  = "env-secret-value-0123456789"
		fileSecret = "-----BEGIN KEY-----\nfile-secret-line-0123456789\n-----END KEY-----"
		hdrSecret  = "hdr-secret-value-9876543210"
	)
	host := newHostStub(t)
	policy := types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretValues: []string{envSecret, fileSecret}}}
	h, _, p := newInbound(t, host, policy)
	registerHeaderCredentialFor(p.maskKey, "Bearer "+hdrSecret)
	if p.scrubResponses {
		t.Fatal("the test needs response scrubbing off for the run")
	}
	for name, reply := range map[string]func(w http.ResponseWriter){
		"json": func(w http.ResponseWriter) {
			w.Header().Set("X-Echo", envSecret)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"a":%q,"b":%q,"c":%q}}`, envSecret, fileSecret, "Bearer "+hdrSecret)
		},
		"stream": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			msg := "data: " + envSecret + " " + hdrSecret + "\n\n"
			for i := 0; i < len(msg); i += 5 { // a secret split across chunks
				_, _ = io.WriteString(w, msg[i:min(i+5, len(msg))])
				fl.Flush()
			}
		},
	} {
		host.mu.Lock()
		host.reply = reply
		host.mu.Unlock()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, routeMCPRelay+inboundToken, strings.NewReader(mcpInit)))
		out := rec.Body.String() + fmt.Sprint(rec.Header())
		for _, secret := range []string{envSecret, "file-secret-line-0123456789", hdrSecret} {
			if strings.Contains(out, secret) {
				t.Errorf("%s: the answer carries %q: %s", name, secret, out)
			}
		}
		if !strings.Contains(out, string(secretmask.Placeholder())) {
			t.Errorf("%s: no placeholder in %s", name, out)
		}
	}
}

// TestMCPProxySendsNoLoadedReport: an MCP sandbox's proxy starts no loaded report, which its token
// could not post; a run's own proxy starts one as before.
func TestMCPProxySendsNoLoadedReport(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(cp.Close)
	for _, mcp := range []bool{true, false} {
		extra := ""
		if mcp {
			extra = `,"control_plane_audience":"wardyn-mcp-sandbox","mcp_relay_inbound":{"listen":":3130","token_sha256":"` + mcpRelayDigest + `","upstream":"10.88.0.3:8765"}`
		}
		cfg, err := LoadConfigBytes(fmt.Appendf(nil, `{"run_id":%q,"control_plane_url":%q,"run_token":"t"%s}`, uuid.New(), cp.URL, extra))
		if err != nil {
			t.Fatal(err)
		}
		srv, err := NewServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		srv.StartBootReport(context.Background(), LoadedReport{ConfigSHA256: "x"})
		srv.live.reportMu.Lock()
		started := srv.live.reportCancel != nil
		srv.live.reportMu.Unlock()
		if started == mcp {
			t.Errorf("MCP sandbox proxy %v: loaded report started %v", mcp, started)
		}
	}
}

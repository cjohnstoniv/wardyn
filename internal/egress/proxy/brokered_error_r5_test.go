// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A gateway configured by IPv6 literal never reaches the sandbox either: x509
// prints that literal bare, outside the brackets ipLiteralRe matches, and
// lists the certificate's own IP SANs bare too.
func TestGatewayIPv6LiteralIsRedactedFromTheCause(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		LLMUpstreams: map[string]string{anthropicHost: "https://[fd12:3456:0::7]:8443/anthropic"}})
	for _, c := range []struct {
		err  error
		want string
	}{
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "fd12:3456:0::7"},
			"x509: cannot validate certificate for " + redactedEndpoint + " because it doesn't contain any IP SANs"},
		{x509.HostnameError{Certificate: &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("fd00::1"), net.ParseIP("10.0.0.9")}}, Host: "fd12:3456:0::7"},
			"x509: certificate is valid for " + redactedEndpoint + ", not " + redactedEndpoint},
	} {
		rec := httptest.NewRecorder()
		p.writeBrokeredFault(rec, shapeAnthropic, "fd12:3456:0::7", brokeredFault{port: 8443, ruleSource: "builtin:dial-failed"}, c.err)
		v := decodeSDK(t, shapeAnthropic, rec)
		if !strings.HasSuffix(v.message, c.want) || strings.Contains(v.message, "fd12") || strings.Contains(v.message, "fd00") {
			t.Errorf("message %q: want it to end %q with no address", v.message, c.want)
		}
	}
	for _, cause := range []string{"dial tcp [fd12:3456:0::7]:8443: refused", "for fd12:3456::7 because"} {
		if got := p.dialFailureCause(errors.New(cause)); strings.Contains(got, "fd12") {
			t.Errorf("cause %q keeps the gateway literal", got)
		}
	}
}

// A connection that opened and then broke before the answer's headers is
// upstream_error, and its message and decision row say so: never "dial".
func TestReadResetIsLabelledAfterConnect(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{})})
	reset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	rec := httptest.NewRecorder()
	p.writeBrokeredFault(rec, shapeAnthropic, anthropicHost, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, reset)
	v := decodeSDK(t, shapeAnthropic, rec)
	row := p.denyDialFailed("builtin:dial-failed", egress.Request{Host: anthropicHost, Port: 443}, anthropicHost, reset, nil)
	if !strings.HasPrefix(v.message, "wardyn: "+reasonUpstreamError+": ") || !strings.HasSuffix(v.message, ": "+row.Cause) ||
		!strings.HasPrefix(row.Cause, "after connect: read tcp") {
		t.Errorf("message %q, row cause %q: want upstream_error ending with an after-connect cause", v.message, row.Cause)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{&net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}, "after connect"},
		{io.ErrUnexpectedEOF, "after connect"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "tcp dial"},
		{&connectStageError{reset}, "dial"},
	} {
		if got := dialStage(c.err); got != c.want {
			t.Errorf("dialStage(%v) = %q, want %q", c.err, got, c.want)
		}
		if (dialStage(c.err) == "after connect") != (classifyUpstreamErr(c.err) == reasonUpstreamError) {
			t.Errorf("%v: label %q disagrees with reason %q", c.err, dialStage(c.err), classifyUpstreamErr(c.err))
		}
	}
}

// A full audit spool refuses a brokered request through the real route with
// the SDK's 503, in its route's shape.
func TestFullSpoolRefusesBrokeredRoutesInTheirShape(t *testing.T) {
	for _, rt := range []struct {
		prefix string
		shape  errShape
	}{{llmAnthropicPrefix, shapeAnthropic}, {llmOpenAIPrefix, shapeOpenAI}} {
		s, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.close(t.Context()) })
		s.heldBytes = runnerspool.MaxBytes // every byte the spool may hold is reserved
		p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Sink: s, Resolver: publicResolver{},
			Injector: staticInj(map[string]injectedHeader{anthropicHost: {name: "X-Api-Key", value: "K"}, openaiHost: {name: "Authorization", value: "Bearer K"}})})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/messages", strings.NewReader("{}")))
		v := decodeSDK(t, rt.shape, rec)
		if rec.Code != http.StatusServiceUnavailable || v.typ != "api_error" || rec.Header().Get("X-Wardyn-Reason") != reasonAuditUnavailable {
			t.Errorf("%s: status %d type %q reason %q", rt.prefix, rec.Code, v.typ, rec.Header().Get("X-Wardyn-Reason"))
		}
	}
}

// The HTTP/2 mismatch refusal on the brokered routes themselves (the
// forwardInspectedLLM call site) is the route's SDK error.
func TestBrokeredRouteProtocolMismatchIsAProviderError(t *testing.T) {
	for _, rt := range []struct {
		prefix string
		shape  errShape
		inj    *injector
	}{{llmAnthropicPrefix, shapeAnthropic, anthropicInjector()}, {llmOpenAIPrefix, shapeOpenAI, openaiInjector()}} {
		p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", startH2MismatchPeer(t), rt.inj, testInsecureTLSConfig)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/messages", strings.NewReader("{}")))
		v := decodeSDK(t, rt.shape, rec)
		d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch)
		if rec.Code != http.StatusBadRequest || v.typ != "invalid_request_error" || rec.Header().Get("X-Wardyn-Reason") != reasonProtocolMismatch ||
			!strings.HasPrefix(v.message, "wardyn: "+reasonProtocolMismatch+": ") || !strings.HasSuffix(v.message, ": "+d.Cause) {
			t.Errorf("%s: status %d type %q message %q (row cause %q)", rt.prefix, rec.Code, v.typ, v.message, d.Cause)
		}
	}
}

// A peer that resets the connection during the TLS handshake never connected,
// so the brokered route answers upstream_unreachable, not upstream_error:
// only handshakeOver's wrap tells that reset from one on an open connection.
func TestHandshakeResetIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Read(make([]byte, 1)) // the ClientHello has arrived
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", ln.Addr().String(), anthropicInjector(), testInsecureTLSConfig)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader("{}")))
	if v := decodeSDK(t, shapeAnthropic, rec); rec.Header().Get("X-Wardyn-Reason") != reasonUpstreamUnreachable ||
		!strings.HasPrefix(v.message, "wardyn: "+reasonUpstreamUnreachable+": ") {
		t.Errorf("status %d reason %q message %q", rec.Code, rec.Header().Get("X-Wardyn-Reason"), v.message)
	}
}

// On a gateway host both vendors share, the unsupported-path 400 and the
// no-credential 404 still answer in each route's own shape.
func TestSharedGatewayRefusalsKeepTheRoutesShape(t *testing.T) {
	noCred := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Resolver: publicResolver{},
		Injector: staticInj(map[string]injectedHeader{}),
		LLMUpstreams: map[string]string{
			anthropicHost: "https://" + sharedGatewayHost + "/anthropic",
			openaiHost:    "https://" + sharedGatewayHost + "/openai",
		}})
	for _, rt := range []struct {
		prefix string
		shape  errShape
	}{{llmAnthropicPrefix, shapeAnthropic}, {llmOpenAIPrefix, shapeOpenAI}} {
		rec := httptest.NewRecorder()
		sharedGatewayProxy(types.RunPolicySpec{}, publicResolver{}).ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/a%5Cb", strings.NewReader("{}")))
		if v := decodeSDK(t, rt.shape, rec); rec.Code != http.StatusBadRequest || v.typ != "invalid_request_error" ||
			rec.Header().Get("X-Wardyn-Reason") != reasonUnsupportedPath {
			t.Errorf("%s unsupported path: status %d type %q body %q", rt.prefix, rec.Code, v.typ, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		noCred.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/messages", strings.NewReader("{}")))
		if v := decodeSDK(t, rt.shape, rec); rec.Code != http.StatusNotFound || !strings.HasPrefix(v.message, "wardyn: "+reasonCredentialUnavailable+": ") ||
			strings.Contains(rec.Body.String(), sharedGatewayHost) {
			t.Errorf("%s no credential: status %d body %q", rt.prefix, rec.Code, rec.Body.String())
		}
	}
}

// A direct request to a gateway host both vendors share has no schema of its
// own (only a brokered route names one), so on every construction it is the
// shared channel, whose body is marked uninspected and refused under
// fail-closed blocking, never parsed as whichever vendor the table kept.
func TestSharedGatewayDirectRequestIsUninspected(t *testing.T) {
	for range 20 {
		if ch := sharedGatewayProxy(types.RunPolicySpec{}, publicResolver{}).channelForHost(sharedGatewayHost); ch != channelSharedGateway {
			t.Fatalf("channelForHost(shared gateway) = %q, want %q", ch, channelSharedGateway)
		}
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{sharedGatewayHost}}),
		Resolver: publicResolver{}, Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		LLMUpstreams: map[string]string{
			anthropicHost: "https://" + sharedGatewayHost + "/anthropic",
			openaiHost:    "https://" + sharedGatewayHost + "/openai",
		}})
	p.scanner = scanEngineFailClosed(t, scanTestSecret)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://"+sharedGatewayHost+"/anthropic/v1/messages", strings.NewReader(anthropicMessagesBody("hi"))))
	if d := lastDecision(t, buf); rec.Code != http.StatusForbidden || d.RuleSource != ruleSourceLLMBlocked || d.Scan == nil ||
		d.Scan.SkipReason != "uninspected_channel" {
		t.Errorf("status %d decision %+v", rec.Code, d)
	}
}

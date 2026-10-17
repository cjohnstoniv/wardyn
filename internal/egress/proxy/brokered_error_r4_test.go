// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const sharedGatewayHost = "llm-gw.corp.internal"

// tooLongTarget is a path too long for one audit record.
var tooLongTarget = strings.Repeat("x", runnerspool.MaxRecordBytes/6)

// sharedGatewayProxy routes both vendors through one gateway host, a supported
// setup: Claude Code and Codex then reach the same address.
func sharedGatewayProxy(spec types.RunPolicySpec, res resolver) *Proxy {
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(spec),
		Resolver: res,
		Injector: staticInj(map[string]injectedHeader{sharedGatewayHost: {name: "X-Api-Key", value: "K"}}),
		LLMUpstreams: map[string]string{
			anthropicHost: "https://" + sharedGatewayHost + "/anthropic",
			openaiHost:    "https://" + sharedGatewayHost + "/openai",
		},
	})
}

// Each route answers in its own vendor's shape and label on a shared gateway
// host, whatever order the gateway table was built in (run with -count=50),
// and neither the label nor X-Wardyn-Host names the gateway. A terminated
// tunnel to that host, which no route names, gets no vendor's shape.
func TestSharedGatewayAnswersInEachRoutesShape(t *testing.T) {
	routes := []struct {
		prefix, vendor string
		shape          errShape
	}{{llmAnthropicPrefix, anthropicHost, shapeAnthropic}, {llmOpenAIPrefix, openaiHost, shapeOpenAI}}
	for _, rt := range routes {
		t.Run(rt.vendor+"/method deny", func(t *testing.T) {
			p := sharedGatewayProxy(types.RunPolicySpec{AllowedMethods: []string{"GET"}}, publicResolver{})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/x", nil))
			v := decodeSDK(t, rt.shape, rec)
			if rec.Code != http.StatusForbidden || !strings.HasPrefix(v.message, "wardyn: egress_denied: "+rt.vendor+" (via the configured gateway)") ||
				strings.Contains(rec.Body.String(), sharedGatewayHost) || rec.Header().Get("X-Wardyn-Host") != rt.vendor {
				t.Errorf("status %d X-Wardyn-Host %q body %q", rec.Code, rec.Header().Get("X-Wardyn-Host"), rec.Body.String())
			}
		})
		t.Run(rt.vendor+"/gateway vet fault", func(t *testing.T) {
			p := sharedGatewayProxy(types.RunPolicySpec{}, fakeResolver{err: errors.New("dns down")})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, rt.prefix+"v1/x", strings.NewReader("{}")))
			v := decodeSDK(t, rt.shape, rec)
			if rec.Code != http.StatusBadGateway || !strings.HasPrefix(v.message, "wardyn: upstream_error: "+rt.vendor+" (via the configured gateway)") ||
				strings.Contains(rec.Body.String(), sharedGatewayHost) {
				t.Errorf("status %d body %q", rec.Code, rec.Body.String())
			}
		})
	}
	if s := sharedGatewayProxy(types.RunPolicySpec{}, publicResolver{}).shapeOfHost(sharedGatewayHost); s != shapePlain {
		t.Errorf("shapeOfHost(shared gateway) = %v, want no vendor's shape", s)
	}
}

// A gateway's own host never reaches the sandbox in a cause either, such as
// the hostname a certificate does not cover.
func TestGatewayHostIsRedactedFromTheCause(t *testing.T) {
	p := sharedGatewayProxy(types.RunPolicySpec{}, publicResolver{})
	err := x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"other.example"}}, Host: sharedGatewayHost}
	rec := httptest.NewRecorder()
	p.writeBrokeredFault(rec, shapeAnthropic, sharedGatewayHost, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, err)
	v := decodeSDK(t, shapeAnthropic, rec)
	if strings.Contains(rec.Body.String(), sharedGatewayHost) || strings.Contains(rec.Body.String(), "other.example") ||
		!strings.Contains(v.message, "valid for "+redactedEndpoint+", not "+redactedEndpoint) {
		t.Errorf("message %q leaks the gateway host or its certificate's names, or lost the cause", v.message)
	}
	if got := p.dialFailureCause(errors.New("dial tcp " + sharedGatewayHost + ":8443: refused; xllm-gw.corp.internalx")); strings.Contains(got, sharedGatewayHost+":") ||
		!strings.Contains(got, "xllm-gw.corp.internalx") {
		t.Errorf("cause %q: want the host and port redacted, a longer word left alone", got)
	}
}

// A request the egress audit cannot record is refused in the SDK's shape: a
// brokered route's own, an LLM or AWS host's, plain text on the forward lane.
func TestUnauditedRequestIsRefusedInTheSDKShape(t *testing.T) {
	ctx := context.Background()
	newSpoolProxy := func(t *testing.T) *Proxy {
		s, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), uuid.New(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.close(ctx) })
		return &Proxy{sink: s}
	}
	for _, c := range []struct {
		name     string
		req      *http.Request
		mitmHost string
		shape    errShape
	}{
		{"brokered anthropic", mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/"+tooLongTarget, nil), "", shapeAnthropic},
		{"brokered openai", mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/"+tooLongTarget, nil), "", shapeOpenAI},
		{"bedrock tunnel", httptest.NewRequest(http.MethodPost, "https://"+bedrockHost+"/"+tooLongTarget, nil), bedrockHost, shapeAWS},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if admitted, _ := newSpoolProxy(t).admitSpoolHTTP(rec, c.req, c.mitmHost); admitted != nil {
				t.Fatal("an oversized target was admitted")
			}
			v := decodeSDK(t, c.shape, rec)
			if rec.Code != http.StatusRequestURITooLong || rec.Header().Get("X-Wardyn-Reason") != reasonAuditRecordTooLarge ||
				!strings.HasPrefix(v.message, "wardyn: "+reasonAuditRecordTooLarge+": ") {
				t.Errorf("status %d reason %q message %q", rec.Code, rec.Header().Get("X-Wardyn-Reason"), v.message)
			}
		})
	}
	t.Run("forward lane keeps its text", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newSpoolProxy(t).admitSpoolHTTP(rec, httptest.NewRequest(http.MethodGet, "http://allowed.test/"+tooLongTarget, nil), "")
		if rec.Code != http.StatusRequestURITooLong || rec.Body.String() != "egress audit record too large\n" {
			t.Errorf("status %d body %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("bedrock tunnel, spool unavailable", func(t *testing.T) {
		p := newSpoolProxy(t)
		p.sink.close(ctx)
		rec := httptest.NewRecorder()
		if admitted, _ := p.admitSpoolHTTP(rec, httptest.NewRequest(http.MethodPost, "https://"+bedrockHost+"/model/x/converse", nil), bedrockHost); admitted != nil {
			t.Fatal("admitted with the spool closed")
		}
		if v := decodeSDK(t, shapeAWS, rec); rec.Code != http.StatusServiceUnavailable || v.typ != "ServiceUnavailableException" ||
			!strings.HasPrefix(v.message, "wardyn: "+reasonAuditUnavailable+": ") {
			t.Errorf("status %d type %q message %q", rec.Code, v.typ, v.message)
		}
	})
}

// The HTTP/2 mismatch refusal is the SDK's error on every SDK lane, naming the
// host and rule source, and plain text elsewhere.
func TestProtocolMismatchIsAProviderError(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{})})
	mm := &h2MismatchError{hadTLS: true, err: errors.New("frame")}
	for _, c := range []struct {
		shape errShape
		host  string
		typ   string
	}{{shapeAnthropic, anthropicHost, "invalid_request_error"}, {shapeOpenAI, openaiHost, "invalid_request_error"},
		{shapeAWS, bedrockHost, "UpstreamProtocolMismatchException"}} {
		rec := httptest.NewRecorder()
		if !p.refuseH2Mismatch(rec, c.shape, mm, ruleSourceUpstreamProtocolMismatch, nil, c.host, "llm upstream error") {
			t.Fatal("not refused")
		}
		v := decodeSDK(t, c.shape, rec)
		want := "wardyn: " + reasonProtocolMismatch + ": " + c.host + ":443 (" + ruleSourceUpstreamProtocolMismatch + ")"
		if rec.Code != http.StatusBadRequest || v.typ != c.typ || !strings.HasPrefix(v.message, want) ||
			rec.Header().Get("X-Wardyn-Reason") != reasonProtocolMismatch {
			t.Errorf("%s: status %d type %q message %q", c.host, rec.Code, v.typ, v.message)
		}
	}
	rec := httptest.NewRecorder()
	p.refuseH2Mismatch(rec, shapePlain, mm, ruleSourceUpstreamProtocolMismatch, nil, "git.example", "git upstream error")
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), "git upstream error: peer answered HTTP/2") {
		t.Errorf("plain: status %d body %q", rec.Code, rec.Body.String())
	}
}

// A connection that opened and then broke before the answer's headers (a
// reset while the answer was awaited) is upstream_error, not "could not
// connect"; a failure while connecting stays upstream_unreachable.
func TestClassifyAReadResetAsUpstreamError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, reasonUpstreamError},
		{&net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}, reasonUpstreamError},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, reasonUpstreamUnreachable},
		// The same reset during the TLS handshake is still "could not connect".
		{&connectStageError{&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}, reasonUpstreamUnreachable},
	} {
		if got := classifyUpstreamErr(c.err); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

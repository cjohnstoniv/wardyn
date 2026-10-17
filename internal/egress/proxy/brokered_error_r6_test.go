// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A failure on a connection that had opened, whatever its error type, is
// "after connect" with reason upstream_error in the message and the decision
// row alike: an HTTP/2 GOAWAY or stream reset, an idle connection the server
// closed. Only a dial, or a failure while connecting, reads "dial".
func TestPostConnectFailureIsAfterConnectWhateverItsType(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{})})
	wrap := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://" + anthropicHost + "/v1/messages", Err: err}
	}
	for _, c := range []struct {
		err    error
		stage  string
		reason string
	}{
		{wrap(errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1, ErrCode=NO_ERROR, debug=""`)), "after connect", reasonUpstreamError},
		{wrap(errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")), "after connect", reasonUpstreamError},
		{wrap(errors.New("http: server closed idle connection")), "after connect", reasonUpstreamError},
		// B-R4-L3: the request context's deadline passing while the answer's headers are awaited.
		{wrap(context.DeadlineExceeded), "after connect", reasonUpstreamError},
		{wrap(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), "tcp dial", reasonUpstreamUnreachable},
		{wrap(&connectStageError{errors.New("i/o timeout")}), "dial", reasonUpstreamUnreachable},
	} {
		rec := httptest.NewRecorder()
		p.writeBrokeredFault(rec, shapeAnthropic, anthropicHost, brokeredFault{port: 443, ruleSource: "builtin:dial-failed"}, c.err)
		v := decodeSDK(t, shapeAnthropic, rec)
		row := p.denyDialFailed("builtin:dial-failed", egress.Request{Host: anthropicHost, Port: 443}, anthropicHost, c.err, nil)
		if !strings.HasPrefix(row.Cause, c.stage+": ") || rec.Header().Get("X-Wardyn-Reason") != c.reason ||
			!strings.HasPrefix(v.message, "wardyn: "+c.reason+": ") || !strings.HasSuffix(v.message, ": "+row.Cause) {
			t.Errorf("%v: reason %q message %q row cause %q; want stage %q, reason %q", c.err, rec.Header().Get("X-Wardyn-Reason"), v.message, row.Cause, c.stage, c.reason)
		}
	}
}

// A peer that completes the TLS handshake, reads the request and then resets
// did connect: the brokered route answers upstream_error, and the decision
// row's Cause says "after connect", never "dial".
func TestPostHandshakeResetIsUpstreamError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	cfg := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, NextProtos: []string{"http/1.1"}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tc := tls.Server(c, cfg)
			if tc.Handshake() == nil {
				_, _ = tc.Read(make([]byte, 1)) // the request has arrived
			}
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}
	}()
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", ln.Addr().String(), anthropicInjector(), testInsecureTLSConfig)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader("{}")))
	d := lastDecision(t, buf)
	if v := decodeSDK(t, shapeAnthropic, rec); rec.Header().Get("X-Wardyn-Reason") != reasonUpstreamError ||
		!strings.HasPrefix(v.message, "wardyn: "+reasonUpstreamError+": ") || !strings.HasPrefix(d.Cause, "after connect: ") ||
		d.RuleSource != "builtin:dial-failed" {
		t.Errorf("status %d reason %q message %q row %s %q", rec.Code, rec.Header().Get("X-Wardyn-Reason"), v.message, d.RuleSource, d.Cause)
	}
}

// R6-L1: a plain-HTTP forward whose dial fails before the connection opened with an error that is
// no net dial error (no vetted target on the context here) is a "dial" failure, not "after connect".
func TestPlainHTTPDialFailureBeforeConnectReadsDial(t *testing.T) {
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{})})
	_, err := p.transport.DialContext(context.Background(), "tcp", "example.test:80")
	if err == nil {
		t.Fatal("dial with no vetted target succeeded")
	}
	if got := dialStage(err); got != "dial" || classifyUpstreamErr(err) != reasonUpstreamUnreachable {
		t.Errorf("dialStage(%v) = %q, reason %q; want dial, %q", err, got, classifyUpstreamErr(err), reasonUpstreamUnreachable)
	}
}

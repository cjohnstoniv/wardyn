// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each of the three genuine builtin:dial-failed sites (proxy.go's CONNECT
// tunnel dial, plain_lane.go's forward RoundTrip, llm_routes.go's brokered-LLM
// RoundTrip) must carry the SAME two new fields: Cause (masked, redacted, and
// naming the stage) and Via (the hop class). These pin that per site, plus the
// stage classifier and the secret-never-leaks guarantee denyDialFailed exists
// for.

// TestDenyDialFailed_ConnectTunnel_DirectTCPDial pins site 1 (proxy.go), no
// upstream configured: Via is "direct" and Cause is stage-prefixed.
func TestDenyDialFailed_ConnectTunnel_DirectTCPDial(t *testing.T) {
	// "127.0.0.1:1" is the repo's dead-port convention: nothing listens there.
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, "127.0.0.1:1", nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, _ := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()

	d := findDecision(t, buf, "builtin:dial-failed")
	if d.Via != viaDirect {
		t.Errorf("via = %q, want %q", d.Via, viaDirect)
	}
	if !strings.HasPrefix(d.Cause, "tcp dial: ") {
		t.Errorf("cause = %q, want a %q-prefixed stage", d.Cause, "tcp dial: ")
	}
}

// TestDenyDialFailed_ConnectTunnel_UpstreamProxyHop pins site 1's OTHER
// branch: a configured corp upstream whose own resolve fails is a hop the
// proxy attempted THROUGH the upstream, not directly — Via must say so, and
// Cause must name the upstream leg, not a bare "dial".
func TestDenyDialFailed_ConnectTunnel_UpstreamProxyHop(t *testing.T) {
	const corp = "corp-proxy.internal.test"
	up, err := parseUpstreamProxy("http://" + corp + ":3128")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver: resolverFailingFor{host: corp},
		Upstream: up,
	})
	buf := p.sink.out.(*bytes.Buffer)

	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()
	conn, _ := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()

	d := findDecision(t, buf, "builtin:dial-failed")
	if d.Via != viaUpstreamProxy {
		t.Errorf("via = %q, want %q", d.Via, viaUpstreamProxy)
	}
	if !strings.HasPrefix(d.Cause, "upstream proxy connect: ") {
		t.Errorf("cause = %q, want an %q-prefixed stage", d.Cause, "upstream proxy connect: ")
	}
}

// TestDenyDialFailed_PlainForward_TCPDialStage pins site 2 (plain_lane.go).
func TestDenyDialFailed_PlainForward_TCPDialStage(t *testing.T) {
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, "127.0.0.1:1", nil, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://allowed.test/thing"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}

	d := findDecision(t, buf, "builtin:dial-failed")
	if d.Via != viaDirect {
		t.Errorf("via = %q, want %q", d.Via, viaDirect)
	}
	if !strings.HasPrefix(d.Cause, "tcp dial: ") {
		t.Errorf("cause = %q, want a %q-prefixed stage", d.Cause, "tcp dial: ")
	}
}

// TestDenyDialFailed_PlainForward_TLSHandshakeStage: the destination answers
// TCP but is not a TLS server at all, so the failure is at the ORIGIN'S TLS
// handshake, not the dial — the exact mislabel ("dial failed" on an
// x509/handshake error) the field report named. dialStage must say so.
func TestDenyDialFailed_PlainForward_TLSHandshakeStage(t *testing.T) {
	// A plain TCP listener that never speaks TLS: RoundTrip's ClientHello gets
	// an answer that is not a TLS ServerHello, which crypto/tls reports as a
	// handshake failure ("tls: ...").
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("not a tls server\n"))
			_ = c.Close()
		}
	}()

	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, ln.Addr().String(), nil, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://allowed.test/thing"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}

	d := findDecision(t, buf, "builtin:dial-failed")
	if !strings.HasPrefix(d.Cause, "tls handshake: ") {
		t.Errorf("cause = %q, want a %q-prefixed stage (not a bare dial failure)", d.Cause, "tls handshake: ")
	}
}

// TestDenyDialFailed_BrokeredLLM_RoundTrip pins site 3 (llm_routes.go's
// forwardInspectedLLM), reached through the brokered gateway route.
func TestDenyDialFailed_BrokeredLLM_RoundTrip(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"llm-gateway.corp.internal": ips("10.40.1.5")}}
	inj := staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "K"}})
	// Dead port: the gateway resolves fine (vetTrustedHost passes), but the
	// actual RoundTrip dial fails — this is forwardInspectedLLM's OWN
	// dial-failed arm, not gatewayTarget's guard refusal.
	p, buf := gatewayProxy(t, "https://llm-gateway.corp.internal/v1", res, "127.0.0.1:1", inj)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	d := findDecision(t, buf, "builtin:dial-failed")
	if d.Via != viaDirect {
		t.Errorf("via = %q, want %q (no corp upstream configured in this test)", d.Via, viaDirect)
	}
	if !strings.HasPrefix(d.Cause, "tcp dial: ") {
		t.Errorf("cause = %q, want a %q-prefixed stage", d.Cause, "tcp dial: ")
	}
}

// TestDenyDialFailed_SecretNeverReachesCause: a registered secret embedded in
// the dial error text must never survive into Cause — the same mask/redaction
// contract httpError's sandbox-facing body already holds, now pinned on the
// decision-log datum a run's own CREATOR can also read (auditScope).
func TestDenyDialFailed_SecretNeverReachesCause(t *testing.T) {
	const secret = "dial-failed-cause-test-secret-should-never-appear"
	procRegistry.AddGlobal([]byte(secret))

	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}),
		Sink:     sink,
		Resolver: publicResolver{},
		// A dialer that fails with an error carrying the secret verbatim, as if
		// it had leaked into a transport error string (a misconfigured proxy
		// echoing credentialed config back in its own diagnostic, say).
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Err: errStringWithSecret{secret}}
		},
	})

	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()
	conn, _ := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()

	d := findDecision(t, buf, "builtin:dial-failed")
	if strings.Contains(d.Cause, secret) {
		t.Fatalf("registered secret %q survives into Cause verbatim: %q", secret, d.Cause)
	}
	if !strings.Contains(d.Cause, "<secret-hidden>") {
		t.Errorf("cause %q carries no redaction placeholder for the masked secret", d.Cause)
	}
}

// errStringWithSecret is a minimal error carrying a registered secret verbatim
// in its Error() text, standing in for whatever real transport wrapping would
// otherwise echo it.
type errStringWithSecret struct{ secret string }

func (e errStringWithSecret) Error() string { return "dial failed, config said: " + e.secret }

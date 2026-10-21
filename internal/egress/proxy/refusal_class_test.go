// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refusalOf is the refusal on the last decision logged for ruleSource.
func refusalOf(t *testing.T, buf *bytes.Buffer, ruleSource string) *egress.Refusal {
	t.Helper()
	d := findDecision(t, buf, ruleSource)
	if d.Decision != egress.Deny {
		t.Fatalf("%s decision = %q, want deny", ruleSource, d.Decision)
	}
	return d.Refusal
}

func wantRefusal(t *testing.T, got *egress.Refusal, by, reason string) {
	t.Helper()
	if got == nil || got.By != by || got.Reason != reason {
		t.Fatalf("refusal = %+v, want by %q reason %q", got, by, reason)
	}
}

// attributedTestProxy is newTestProxy with the run attributed to ref, so a Wardyn
// refusal can name its layer.
func attributedTestProxy(t *testing.T, spec types.RunPolicySpec, upstreamAddr string, ref *policyref.Ref) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(spec), Attribution: ref,
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{}, Dial: redirectDial(upstreamAddr),
	})
	return p, buf
}

// A deny the policy decided is Wardyn's, with the layer the run is attributed to.
func TestRefusal_WardynDenyNamesTheLayer(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}, DeniedDomains: []string{"bad.test"}}
	for _, c := range []struct {
		name, host, reason, layer, layerName string
		ref                                  *policyref.Ref
	}{
		{"profile explicit deny", "bad.test", "policy_denied", "profile", "Team A", teamA},
		{"profile default deny", "other.test", "default_deny", "profile", "Team A", teamA},
		{"deployment", "bad.test", "policy_denied", "deployment", "", deploymentRef},
		{"unattributed run", "bad.test", "policy_denied", "", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, buf := attributedTestProxy(t, spec, "127.0.0.1:1", c.ref)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+c.host+"/x"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			rule := "policy:denied"
			if c.reason == "default_deny" {
				rule = "policy:default-deny"
			}
			r := refusalOf(t, buf, rule)
			wantRefusal(t, r, egress.RefusedByWardyn, c.reason)
			if r.Layer != c.layer || r.LayerName != c.layerName {
				t.Errorf("layer %q name %q, want %q %q", r.Layer, r.LayerName, c.layer, c.layerName)
			}
		})
	}
}

// A guard refusal is Wardyn's but not the policy owner's to change, so it names no layer
// even on an attributed run.
func TestRefusal_GuardRefusalNamesNoLayer(t *testing.T) {
	t.Run("private address through the proxy", func(t *testing.T) {
		p, buf := attributedTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"169.254.169.254"}}, "127.0.0.1:1", teamA)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://169.254.169.254/latest/meta-data"))
		r := refusalOf(t, buf, "builtin:private-ip")
		wantRefusal(t, r, egress.RefusedByWardyn, "private_address")
		if r.Layer != "" || r.LayerName != "" {
			t.Errorf("layer %q name %q on a guard refusal, want none", r.Layer, r.LayerName)
		}
	})
	t.Run("desktop VM relay", func(t *testing.T) {
		buf := &bytes.Buffer{}
		s := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8), layer: "profile", layerName: "Team A"}
		s.emit(decisionLog(egress.Request{Host: "h.test"}, egress.Deny, "vmnet:udp"))
		r := refusalOf(t, buf, "vmnet:udp")
		wantRefusal(t, r, egress.RefusedByWardyn, "vmnet")
		if r.Layer != "" || r.LayerName != "" {
			t.Errorf("layer %q name %q on a guard refusal, want none", r.Layer, r.LayerName)
		}
	})
}

// The attribution headers and the audit row's layer share one definition of "policy
// decided": for every rule_source the refusal table names, the two agree.
func TestRefusal_PolicyDecidedAgreesWithTheAttributionHeaders(t *testing.T) {
	sources := append(egress.WardynRuleSources(), "vmnet:udp", "builtin:resolve-failed", "policy:evaluator-error", "builtin:dial-failed")
	for _, rs := range sources {
		want := egress.PolicyDecided(egress.WardynRefusal(rs))
		if got := policyDecidedReason(rs); got != want {
			t.Errorf("%s: policyDecidedReason = %v, egress.PolicyDecided = %v", rs, got, want)
		}
	}
	for _, rs := range []string{"policy:require-tls", "policy:tool-deny", "policy:denied", "policy:port"} {
		if !policyDecidedReason(rs) {
			t.Errorf("%s is the profile's refusal and must be policy-decided", rs)
		}
	}
	if policyDecidedReason("builtin:private-ip") {
		t.Error("the private-address guard is not the policy owner's to fix")
	}
}

// A peer outside Wardyn that refuses an allowed host is "outside": a refused dial on both
// lanes, and the corporate proxy answering the CONNECT with a status.
func TestRefusal_AllowedHostRefusedOutside(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}
	t.Run("connection refused, plain lane", func(t *testing.T) {
		p, buf := attributedTestProxy(t, spec, "127.0.0.1:1", teamA)
		p.ServeHTTP(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "http://allowed.test/x"))
		wantRefusal(t, refusalOf(t, buf, "builtin:dial-failed"), egress.RefusedByOutside, "connection_refused")
	})
	t.Run("connection refused, CONNECT", func(t *testing.T) {
		p, buf := attributedTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, "127.0.0.1:1", teamA)
		srv := httptest.NewServer(p)
		defer srv.Close()
		conn, _ := connectThrough(t, srv.URL, "tls.test:443")
		defer conn.Close()
		r := refusalOf(t, buf, "builtin:dial-failed")
		wantRefusal(t, r, egress.RefusedByOutside, "connection_refused")
		if r.Layer != "" {
			t.Errorf("an outside refusal names layer %q", r.Layer)
		}
	})
	t.Run("corporate proxy refuses the CONNECT", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					_, _ = http.ReadRequest(bufio.NewReader(c))
					_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
				}()
			}
		}()
		up, err := parseUpstreamProxy("http://" + ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		buf := &bytes.Buffer{}
		p := newProxy(Options{
			RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}),
			Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
			Resolver: publicResolver{}, Upstream: up,
			// The corporate proxy is on loopback; the guard exception for it is the same one production uses.
		})
		srv := httptest.NewServer(p)
		defer srv.Close()
		conn, _ := connectThrough(t, srv.URL, "tls.test:443")
		defer conn.Close()
		wantRefusal(t, refusalOf(t, buf, "builtin:dial-failed"), egress.RefusedByOutside, "upstream_proxy_refused")
	})
}

// A failure with no readable cause says so: unknown, not a guessed wall.
func TestRefusal_AmbiguousFailureIsUnknown(t *testing.T) {
	t.Run("not a TLS server answers the hello", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
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
		p, buf := attributedTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, ln.Addr().String(), teamA)
		p.ServeHTTP(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "https://allowed.test/x"))
		// A server that answers the hello with something that is not TLS refused it: outside.
		wantRefusal(t, refusalOf(t, buf, "builtin:dial-failed"), egress.RefusedByOutside, "upstream_not_tls")
	})
	t.Run("name did not resolve", func(t *testing.T) {
		buf := &bytes.Buffer{}
		p := newProxy(Options{
			RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"gone.test"}}),
			Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
			Resolver: resolverFailingFor{host: "gone.test"},
		})
		p.ServeHTTP(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "http://gone.test/x"))
		wantRefusal(t, refusalOf(t, buf, "builtin:resolve-failed"), egress.RefusedByUnknown, "resolve_failed")
	})
	t.Run("error table", func(t *testing.T) {
		timeout := &net.OpError{Op: "dial", Err: timeoutErr{}}
		for _, c := range []struct {
			name   string
			err    error
			by, rs string
		}{
			{"reset", fmt.Errorf("read: %w", syscall.ECONNRESET), egress.RefusedByOutside, "connection_reset"},
			{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, egress.RefusedByOutside, "connection_refused"},
			{"timeout", timeout, egress.RefusedByUnknown, "timed_out"},
			{"unreachable", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}, egress.RefusedByUnknown, "dial_failed"},
			{"peer tls alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, egress.RefusedByOutside, "tls_alert"},
			{"peer answered non-TLS", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, egress.RefusedByOutside, "upstream_not_tls"},
			{"tls text", errors.New("tls: handshake failure"), egress.RefusedByUnknown, "tls_failed"},
			{"eof", io.ErrUnexpectedEOF, egress.RefusedByUnknown, "dial_failed"},
		} {
			wantRefusal(t, refusalOfDialError(c.err), c.by, c.rs)
		}
	})
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A tunnel that died after its 200: a non-TLS answer to the hello is outside, a plain
// close says nothing.
func TestRefusal_TunnelEnded(t *testing.T) {
	t.Run("upstream answers the hello with HTTP", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, clientPeer, upstreamPeer, string(fakeClientHello))
		go func() {
			_, _ = upstreamPeer.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
			_ = upstreamPeer.Close()
		}()
		_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.ReadAll(clientPeer)
		cause, r := w.failureClass(true, nil)
		if cause == "" {
			t.Fatal("no failure recorded")
		}
		wantRefusal(t, r, egress.RefusedByOutside, "upstream_not_tls")
	})
	t.Run("upstream closes without answering", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, clientPeer, upstreamPeer, string(fakeClientHello))
		_ = upstreamPeer.Close()
		_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.ReadAll(clientPeer)
		_, r := w.failureClass(true, nil)
		wantRefusal(t, r, egress.RefusedByUnknown, "connection_closed")
	})
	t.Run("upstream resets", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, clientPeer, upstreamPeer, string(fakeClientHello))
		_ = upstreamPeer.Close()
		_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.ReadAll(clientPeer)
		_, r := w.failureClass(true, syscall.ECONNRESET)
		wantRefusal(t, r, egress.RefusedByOutside, "connection_reset")
	})
}

// Rows the proxy does not classify carry no refusal at all; "absent" is not "unknown".
func TestRefusal_UnclassifiedDenyCarriesNone(t *testing.T) {
	buf := &bytes.Buffer{}
	s := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8), layer: "profile", layerName: "Team A"}
	for _, rule := range []string{"policy:evaluator-error", "builtin:gateway-vet-failed", "brokered:ado:upstream-refused", "scan:blocked"} {
		s.emit(decisionLog(egress.Request{Host: "h.test"}, egress.Deny, rule))
	}
	s.emit(decisionLog(egress.Request{Host: "h.test"}, egress.Allow, "policy:denied"))
	if strings.Contains(buf.String(), `"refusal"`) {
		t.Errorf("an unclassified row carries a refusal: %s", buf.String())
	}
}

// The refusal is closed tokens and the layer. Nothing the request carried reaches it, and
// the audit writers drop a refusal that is not one of the closed shapes.
func TestRefusal_NoRequestContent(t *testing.T) {
	const secret = "s3cr3t-token-value"
	p, buf := attributedTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, "127.0.0.1:1", teamA)
	req := mustProxyReq(t, http.MethodPost, "http://allowed.test/path/"+secret+"?k="+secret)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Api-Key", secret)
	req.Body = io.NopCloser(strings.NewReader("body=" + secret))
	p.ServeHTTP(httptest.NewRecorder(), req)
	d := findDecision(t, buf, "builtin:dial-failed")
	raw, _ := json.Marshal(d.Refusal)
	if d.Refusal == nil || strings.Contains(string(raw), secret) || strings.Contains(string(raw), "allowed.test") {
		t.Errorf("refusal %s carries request content", raw)
	}

	fields := map[string]any{}
	(&egress.Refusal{By: "outside", Reason: "connection_refused"}).AuditFields(fields)
	if len(fields) != 2 || fields["by"] != "outside" || fields["refusal_reason"] != "connection_refused" {
		t.Errorf("fields = %v", fields)
	}
	for _, bad := range []*egress.Refusal{
		{By: "wardyn", Reason: "Bearer " + secret},
		{By: "someone", Reason: "denied"},
		{By: "wardyn", Reason: "denied", Layer: "member"},
		{By: "wardyn", Reason: "denied", LayerName: "a\nb"},
		{By: "wardyn", Reason: "denied", LayerName: "Team\u202eA"},
		{By: "outside", Reason: "connection_reset", Layer: "profile"},
		{By: "unknown", Reason: "timed_out", LayerName: "Team A"},
	} {
		bad.AuditFields(fields)
	}
	if len(fields) != 2 {
		t.Errorf("a malformed refusal reached the audit data: %v", fields)
	}
}

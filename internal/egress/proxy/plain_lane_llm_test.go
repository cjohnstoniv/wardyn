// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The PLAIN forward lane with an ABSOLUTE-FORM request URI — the lane
// ServeHTTP routes to handlePlain, and the one every LLM test in this package
// used to skip: they all reach inspection through CONNECT/MITM
// (serveMITMRequest, mitmConnect) or the brokered local route
// (/wardyn/llm/...). Nothing drove `POST https://api.anthropic.com/v1/messages`
// straight at the proxy, so the handleConnect/handlePlain divergence was
// completely unpinned (F141) and the divergence itself was live (F103, F104).

// newPlainLaneProxy builds a proxy whose only allowlisted host is the Anthropic
// vendor host, dialling a local (TLS) stand-in upstream. TLSClientConfig is set
// because an https absolute-form request really does run a TLS handshake from
// this transport.
func newPlainLaneProxy(t *testing.T, upstream string, inj *injector) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
		Injector:        inj,
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstream),
		TLSClientConfig: testInsecureTLSConfig,
	})
	return p, buf
}

// mustAbsReq builds the raw absolute-form forward request a sandbox can send to
// the proxy listener for ANY scheme — including https, which ordinary clients
// reach via CONNECT but nothing forces them to.
func mustAbsReq(t *testing.T, method, rawurl, body string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, rawurl, strings.NewReader(body))
	req.URL = u // absolute-form: authority in the request target
	req.RequestURI = rawurl
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestPlainLaneLLMHostIsInspected pins F103: an absolute-form prompt POST to a
// model host on the forward lane is the SAME prompt egress as the tunnel, so it
// takes the same per-endpoint classifier. Under mode=block a secret-bearing
// body must be REFUSED and must not reach the upstream — instead of being
// forwarded unscanned under a bare `allow / policy:allowed / scan=nil` row.
func TestPlainLaneLLMHostIsInspected(t *testing.T) {
	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme, func(t *testing.T) {
			cu := captureUpstream(t, scheme == "https", "upstream-ok")
			p, buf := newPlainLaneProxy(t, upstreamAddr(cu.srv), anthropicInjector())
			p.scanner = scanEngine(t, "block", scanTestSecret)

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost,
				scheme+"://"+anthropicHost+"/v1/messages",
				anthropicMessagesBody("please use key "+scanTestSecret+" now")))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("plain-lane prompt POST carrying a secret must be blocked in mode=block, got %d body=%q",
					rec.Code, rec.Body.String())
			}
			if cu.reached {
				t.Fatal("a blocked prompt must not reach the upstream on the plain lane either")
			}
			d := lastDecision(t, buf)
			if d.Decision != egress.Deny || d.RuleSource != ruleSourceLLMBlocked {
				t.Fatalf("decision = %+v, want deny/%s", d, ruleSourceLLMBlocked)
			}
			if d.Scan == nil || len(d.Scan.Findings) == 0 {
				t.Fatalf("scan summary = %+v, want a finding (the row must not read as uninspected)", d.Scan)
			}
			if strings.Contains(buf.String(), scanTestSecret) {
				t.Fatal("decision log leaked the raw secret")
			}
		})
	}
}

// TestPlainLaneInjectionStripsSandboxCredential pins F104: the plain lane's
// injection must strip the sandbox's OWN credential headers before setting the
// brokered one, exactly as forwardInspectedLLM does. Otherwise the upstream
// receives both and picks — which makes credential substitution the UPSTREAM's
// choice rather than Wardyn's.
func TestPlainLaneInjectionStripsSandboxCredential(t *testing.T) {
	cu := captureUpstream(t, true, "upstream-ok")
	p, buf := newPlainLaneProxy(t, upstreamAddr(cu.srv), anthropicInjector())
	p.scanner = scanEngine(t, "alert", scanTestSecret)

	req := mustAbsReq(t, http.MethodPost, "https://"+anthropicHost+"/v1/messages",
		anthropicMessagesBody("hello"))
	req.Header.Set("Authorization", "Bearer SANDBOX-OWN-KEY")
	req.Header.Set("X-Auth-Token", "SANDBOX-OWN-TOKEN")
	// F104 fix-up: the strip list has to cover every header a vendor Wardyn
	// brokers for reads as a credential, not four of them.
	for h, v := range sandboxCredentialHeaders {
		req.Header.Set(h, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !cu.reached {
		t.Fatalf("clean body must forward: status=%d reached=%v", rec.Code, cu.reached)
	}
	if got := cu.header.Get("X-Api-Key"); got != "BROKERED-KEY" {
		t.Fatalf("upstream X-Api-Key = %q, want the brokered value", got)
	}
	if got := cu.header.Get("Authorization"); got != "" {
		t.Fatalf("upstream saw the SANDBOX's Authorization %q alongside the brokered header: "+
			"which credential wins is then the upstream's choice, not Wardyn's", got)
	}
	if got := cu.header.Get("X-Auth-Token"); got != "" {
		t.Fatalf("upstream saw the sandbox's X-Auth-Token %q", got)
	}
	assertNoSandboxCredentials(t, cu.header, "the plain lane's injection")
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want allow", d)
	}
}

// TestPlainLaneHTTPSAbsoluteFormPort pins F141's port half: an https
// absolute-form request with no explicit port means 443 — the port the proxy
// vets, dials and RECORDS. It used to be evaluated and audited as 80 while the
// transport ran a TLS handshake against it.
func TestPlainLaneHTTPSAbsoluteFormPort(t *testing.T) {
	cu := captureUpstream(t, true, "upstream-ok")
	p, buf := newPlainLaneProxy(t, upstreamAddr(cu.srv), nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustAbsReq(t, http.MethodGet, "https://"+anthropicHost+"/v1/models", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	d := lastDecision(t, buf)
	if d.Request.Port != 443 {
		t.Fatalf("recorded port = %d, want 443 for an https absolute-form request "+
			"(the audit row must state the port actually dialled)", d.Request.Port)
	}
	// An http:// absolute-form request still means 80 — the fix is scheme-aware,
	// not a blanket 443.
	buf.Reset()
	cu2 := captureUpstream(t, false, "upstream-ok")
	p2, buf2 := newPlainLaneProxy(t, upstreamAddr(cu2.srv), nil)
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, mustAbsReq(t, http.MethodGet, "http://"+anthropicHost+"/v1/models", ""))
	if d2 := lastDecision(t, buf2); d2.Request.Port != 80 {
		t.Fatalf("recorded port = %d, want 80 for an http absolute-form request", d2.Request.Port)
	}
}

// TestPlainLaneNoCleartextInjectionToTheTLSPort pins the half of F110 the code
// can decide alone: a sandbox-chosen cleartext request to a port the operator
// never authored — reachable only because an api_key grant's exact allowlist
// entry is port-blind — must NOT carry the brokered credential.
//
// The fix-up round widened this from the single value 443 (8443/9443/… were
// wide open, and a brokered LLM key rode cleartext to them) to the rule
// injectableTransport now states: https always; a host this proxy only ever
// speaks TLS to (isLLMHost) never over cleartext; port 80 yes; any other
// cleartext port only when the operator AUTHORED it in the allowlist. The
// controls below are the "cannot break a real connector" half: https injects,
// an ordinary http connector on :80 injects, and an operator-authored
// "vendor.test:8080" injects.
func TestPlainLaneNoCleartextInjectionToTheTLSPort(t *testing.T) {
	inj := func() *injector {
		return staticInj(map[string]injectedHeader{
			"vendor.test": {name: "X-Api-Key", value: "BROKERED-KEY"},
			anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
		})
	}
	newVendorProxyDomains := func(t *testing.T, useTLS bool, domains []string) (*Proxy, *capturedUpstream) {
		t.Helper()
		cu := captureUpstream(t, useTLS, "ok")
		buf := &bytes.Buffer{}
		p := newProxy(Options{
			RunID:           uuid.New(),
			Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: domains}),
			Injector:        inj(),
			Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
			Resolver:        publicResolver{},
			Dial:            redirectDial(upstreamAddr(cu.srv)),
			TLSClientConfig: testInsecureTLSConfig,
		})
		return p, cu
	}
	newVendorProxy := func(t *testing.T, useTLS bool) (*Proxy, *capturedUpstream) {
		t.Helper()
		return newVendorProxyDomains(t, useTLS, []string{"vendor.test", anthropicHost})
	}

	t.Run("cleartext to the TLS port is not injected", func(t *testing.T) {
		p, cu := newVendorProxy(t, false)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.test:443/x", `{"a":1}`))
		if !cu.reached {
			t.Fatalf("request should still be forwarded (uncredentialed), status=%d", rec.Code)
		}
		if got := cu.header.Get("X-Api-Key"); got != "" {
			t.Fatalf("upstream received the brokered credential %q IN CLEARTEXT on a sandbox-chosen "+
				"http:// request to the TLS port", got)
		}
	})

	t.Run("https still injects", func(t *testing.T) {
		p, cu := newVendorProxy(t, true)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "https://vendor.test/x", `{"a":1}`))
		if rec.Code != http.StatusOK || cu.header.Get("X-Api-Key") != "BROKERED-KEY" {
			t.Fatalf("https must still be injected: status=%d key=%q", rec.Code, cu.header.Get("X-Api-Key"))
		}
	})

	t.Run("an ordinary http connector still injects", func(t *testing.T) {
		p, cu := newVendorProxy(t, false)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.test/x", `{"a":1}`))
		if rec.Code != http.StatusOK || cu.header.Get("X-Api-Key") != "BROKERED-KEY" {
			t.Fatalf("a plaintext connector on its own port must be unchanged: status=%d key=%q",
				rec.Code, cu.header.Get("X-Api-Key"))
		}
	})

	// The fix-up: 443 was one value of an open axis, not the axis.
	for _, port := range []string{"8443", "9443", "8080"} {
		t.Run("cleartext to an unauthored port "+port+" is not injected", func(t *testing.T) {
			p, cu := newVendorProxy(t, false)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.test:"+port+"/x", `{"a":1}`))
			if !cu.reached {
				t.Fatalf("request should still be forwarded (uncredentialed), status=%d", rec.Code)
			}
			if got := cu.header.Get("X-Api-Key"); got != "" {
				t.Fatalf("upstream received the brokered credential %q IN CLEARTEXT on a sandbox-chosen "+
					"http:// request to port %s, which the operator never authored", got, port)
			}
		})
	}

	// The authored-port escape hatch must NOT reach back to the TLS port: an
	// api_key grant appends the BARE host beside whatever the operator wrote
	// (addAPIKeyGrant, internal/api/llmcred.go), so "vendor.test:443" — the
	// port-scoping remedy docs/POLICIES.md now recommends — and "vendor.test"
	// coexist, the grant still resolves, and AuthoredPortFor answers true for
	// :443. Without the unconditional clamp that spelling puts the operator's
	// brokered credential on the wire in cleartext on the https port.
	t.Run("an authored :443 cannot re-admit cleartext injection", func(t *testing.T) {
		p, cu := newVendorProxyDomains(t, false, []string{"vendor.test:443", "vendor.test"})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.test:443/x", `{"a":1}`))
		if !cu.reached {
			t.Fatalf("request should still be forwarded (uncredentialed), status=%d", rec.Code)
		}
		if got := cu.header.Get("X-Api-Key"); got != "" {
			t.Fatalf("upstream received the brokered credential %q IN CLEARTEXT on the TLS port after the "+
				"operator authored \"vendor.test:443\" — an authored port must never re-admit :443", got)
		}
	})

	t.Run("an operator-authored cleartext port still injects", func(t *testing.T) {
		p, cu := newVendorProxyDomains(t, false, []string{"vendor.test:8080"})
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, "http://vendor.test:8080/x", `{"a":1}`))
		if rec.Code != http.StatusOK || cu.header.Get("X-Api-Key") != "BROKERED-KEY" {
			t.Fatalf("a connector the operator authored as \"vendor.test:8080\" must still be injected: "+
				"status=%d key=%q — the escape hatch for a plaintext connector on a non-default port",
				rec.Code, cu.header.Get("X-Api-Key"))
		}
	})

	t.Run("a TLS-only vendor host is never injected over cleartext", func(t *testing.T) {
		for _, target := range []string{
			"http://" + anthropicHost + "/v1/x",
			"http://" + anthropicHost + ":8443/v1/x",
		} {
			p, cu := newVendorProxy(t, false)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustAbsReq(t, http.MethodPost, target, `{"a":1}`))
			if got := cu.header.Get("X-Api-Key"); got != "" {
				t.Fatalf("%s delivered the brokered credential %q in cleartext: this proxy only ever "+
					"dials a model-API host over TLS, so there is no plaintext connector to break", target, got)
			}
		}
	})
}

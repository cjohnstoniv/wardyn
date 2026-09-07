// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F152 — Proxy.httpError writes its error text to the SANDBOX, and the mask it
// applies (maskDecisionBytes) only replaces values registered in procRegistry,
// i.e. CREDENTIALS. A Go transport error always embeds the ENDPOINT that failed,
// so every sandbox-reachable caller of httpError was handing the untrusted
// (possibly prompt-injected) process inside the box an internal address it has
// no other way to learn: buildBaseSandboxEnv gives the sandbox only
// WARDYN_PROXY_URL/HTTP_PROXY, never the control-plane or corp-proxy address.
//
// The three arms below are the three sandbox-reachable disclosures the finding
// names, each driven through the real entry point rather than through
// redactTopology, so the pin holds the BOUNDARY and not the helper: a fourth
// caller that skipped the redaction would still be caught by the arm that
// reaches it. Each asserts the diagnosis SURVIVES — a body that said nothing
// would pass a "does not contain" test while making the proxy undebuggable.

// resolverFailingFor resolves every host to a public address except one, which
// fails to resolve — the shape that puts a CONFIGURED endpoint's hostname (not
// merely an IP) into the error text a caller wraps.
type resolverFailingFor struct{ host string }

func (r resolverFailingFor) LookupIP(host string) ([]net.IP, error) {
	if strings.EqualFold(host, r.host) {
		return nil, fmt.Errorf("lookup %s: no such host", host)
	}
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

// assertNoTopology fails when body still discloses any of the internal
// endpoints in leaked, and when the diagnosis has been thrown away with them.
func assertNoTopology(t *testing.T, what, body string, leaked []string, keep string) {
	t.Helper()
	for _, s := range leaked {
		if strings.Contains(body, s) {
			t.Errorf("%s: the sandbox-facing body discloses %q — %s\n"+
				"httpError masks CREDENTIALS (procRegistry); a transport error's ENDPOINT "+
				"is not a credential, and the sandbox is given neither the control-plane nor "+
				"the corp-proxy address anywhere else", what, s, body)
		}
	}
	if keep != "" && !strings.Contains(body, keep) {
		t.Errorf("%s: the body lost %q as well as the topology (%q) — the redaction must "+
			"take the addresses and leave the diagnosis", what, keep, body)
	}
}

// TestControlPlaneRelayErrorDisclosesNoTopology: relayControlPlane is reachable
// from the sandbox by POSTing the brokered mint route, and its 502 wrapped the
// control-plane base URL, the internal API path and the resolved ip:port.
func TestControlPlaneRelayErrorDisclosesNoTopology(t *testing.T) {
	// "127.0.0.1:1" is the repo's dead-port convention: vets fine, nothing answers.
	p, _ := newLocalRouteProxy(t, "http://wardynd.internal.test:8080", "RUNTOK", "127.0.0.1:1", nil, nil)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, routeMint,
		strings.NewReader(`{"grant_id":"`+uuid.New().String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	assertNoTopology(t, "control-plane relay", rec.Body.String(),
		[]string{"wardynd.internal.test", "http://wardynd.internal", "/api/v1/internal/", "127.0.0.1"},
		"control plane")
}

// TestUpstreamDialErrorDisclosesNoTopology: handleConnect's dial-failure branch
// wrapped dialThroughUpstream, whose messages carry the operator's CORPORATE
// proxy address — an endpoint the sandbox is never told.
func TestUpstreamDialErrorDisclosesNoTopology(t *testing.T) {
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

	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, resp := connectThrough(t, srv.URL, "tls.test:443")
	defer func() { _ = conn.Close() }()

	if !strings.Contains(resp, "502") {
		t.Fatalf("CONNECT response = %q, want 502 (the corp proxy is unreachable)", resp)
	}
	assertNoTopology(t, "corp upstream dial", resp,
		[]string{corp, "3128"}, "upstream dial failed")
}

// TestPlainForwardDialErrorDisclosesNoTopology: handlePlain's 502 carried the
// VETTED destination IP — which under the site-config internal_hosts lift is a
// private address the sandbox could not otherwise learn.
func TestPlainForwardDialErrorDisclosesNoTopology(t *testing.T) {
	// The dialer lands on a dead port, so the transport error names an address.
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, "127.0.0.1:1", nil, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://allowed.test/thing"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, rec.Body.String())
	}
	// 93.184.216.34 is publicResolver's vetted answer; 127.0.0.1 is what the
	// dial actually reported. Neither is the sandbox's to know.
	assertNoTopology(t, "plain forward dial", rec.Body.String(),
		[]string{"93.184.216.34", "127.0.0.1"}, "")
}

// TestRedactTopologyIsEndpointScopedButBlanketOnIPLiterals pins the ASYMMETRY
// the two halves of the redaction have, because sandbox_error.go's comments are
// a claim about it and a reader who trusts a comment over the code gets the
// trust boundary wrong.
//
// The endpoint pass (p.topologyRe) is scoped to the operator's CONFIGURED
// endpoints, so a hostname the sandbox itself named survives — that is the half
// of the message a developer needs. The IP pass is NOT scoped: every address
// literal goes, including one the request itself named, because once an address
// is inside a transport error string the proxy cannot tell a vetted destination
// from a pinned internal address, and the literals a sandbox can name at all
// are the internal_hosts lift's private addresses. Fail closed on the
// ambiguity. If a later change narrows the IP arm to spare a sandbox-named
// literal, this test goes red and the comments must move with it.
func TestRedactTopologyIsEndpointScopedButBlanketOnIPLiterals(t *testing.T) {
	up, err := parseUpstreamProxy("http://corp-proxy.internal.test:3128")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		Upstream:        up,
		ControlPlaneURL: "http://wardynd.internal.test:8080",
	})

	for _, tc := range []struct {
		name, in, want string
	}{
		{
			// The sandbox asked for this host by name; it learns nothing from
			// seeing it back, and without it the error is undebuggable.
			name: "a hostname the sandbox named survives",
			in:   `Post "https://api.example.test/v1/thing": dial tcp: lookup api.example.test: no such host`,
			want: `Post "https://api.example.test/v1/thing": dial tcp: lookup api.example.test: no such host`,
		},
		{
			// A sandbox-named literal is redacted TOO — the documented behaviour,
			// not an accident: an internal_hosts-lifted private address reaching
			// the sandbox is the disclosure this arm exists to stop, and the
			// error string does not say which literal is which.
			name: "an address literal the sandbox itself named is redacted anyway",
			in:   "dial tcp 10.1.2.3:8080: connect: connection refused",
			want: "dial tcp " + redactedEndpoint + ": connect: connection refused",
		},
		{
			name: "the configured control-plane endpoint and its path go",
			in:   `Post "http://wardynd.internal.test:8080/api/v1/internal/credentials/mint": EOF`,
			// The endpoint pattern consumes the internal API path with it, and
			// stops at the quote — the route is part of what the sandbox learns.
			want: `Post "` + redactedEndpoint + `": EOF`,
		},
		{
			name: "the configured corp-proxy address goes, host form included",
			in:   "dial upstream proxy: dial tcp corp-proxy.internal.test:3128: i/o timeout",
			// The run of non-space/non-quote bytes after the endpoint (":3128:")
			// goes with it; the diagnosis after the space stays.
			want: "dial upstream proxy: dial tcp " + redactedEndpoint + " i/o timeout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.redactTopology(tc.in); got != tc.want {
				t.Errorf("redactTopology(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

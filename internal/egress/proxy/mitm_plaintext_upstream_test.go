// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE MITM'S UPSTREAM LEG SPEAKS THE SCHEME ITS ENTRY NAMES (walk-3).
//
// Terminating the tunnel is not optional for Phase B: the sandbox holds a
// placeholder, not a credential, so the real token can only be substituted into
// a request the proxy can SEE. What was wrong was the leg AFTER the
// termination — forwardInspectedLLM built every upstream URL as https://, so a
// MITM'd host whose origin serves plain HTTP got a TLS dial at a server with no
// TLS: builtin:dial-failed and a 502 on each of the SDK's 36-69 CONNECTs per
// run, no role credentials, and every case waiting on a model call timed out.
//
// The two arms below are the whole rule, and the https one is production.

func TestParseMITMHostPort_SchemePrefixIsOptionalAndDefaultsToTLS(t *testing.T) {
	for _, tc := range []struct {
		entry     string
		host      string
		port      int
		plaintext bool
	}{
		// Every spelling any lane has ever authored — unchanged, byte for byte.
		{"portal.sso.eu-west-2.amazonaws.com:443", "portal.sso.eu-west-2.amazonaws.com", 443, false},
		{"artifacts.corp.example:8443", "artifacts.corp.example", 8443, false},
		{"legacy.corp.example", "legacy.corp.example", 0, false}, // the historical any-port entry
		{"  Portal.SSO.Eu-West-2.AmazonAWS.com:443.  ", "portal.sso.eu-west-2.amazonaws.com", 443, false},
		// The new one, and the only one that turns the upstream leg cleartext.
		{"http://fake.internal:8090", "fake.internal", 8090, true},
		{"HTTP://Fake.Internal:8090", "fake.internal", 8090, true},
		// An explicit https:// prefix is accepted and means what it says.
		{"https://portal.corp.example:9443", "portal.corp.example", 9443, false},
		// Nothing usable.
		{"", "", 0, false},
		{"http://", "", 0, false},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			host, port, plaintext := parseMITMHostPort(tc.entry)
			if host != tc.host || port != tc.port || plaintext != tc.plaintext {
				t.Errorf("parseMITMHostPort(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.entry, host, port, plaintext, tc.host, tc.port, tc.plaintext)
			}
		})
	}
}

// The end of the lane: a MITM'd request actually REACHES a plain-HTTP origin,
// with the brokered credential on it — and the same code reaching a TLS origin
// is untouched.
func TestForwardInspectedLLM_ReOriginatesInTheSchemeTheEntryNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		// wantReached is whether the plain-HTTP origin below should see the request.
		// The https arm must NOT reach it: dialling TLS at a plaintext server is
		// precisely the failure this fixes, and it has to stay the behaviour for
		// every unprefixed entry, because every real portal serves TLS.
		wantReached bool
		wantStatus  int
	}{
		{"http:// entry — cleartext upstream", "http://" + plainMITMHost + ":8090", true, http.StatusOK},
		{"unprefixed entry — TLS upstream, as production", plainMITMHost + ":8090", false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader string
			var hits int
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				gotHeader = r.Header.Get("x-amz-sso_bearer_token")
				_, _ = w.Write([]byte(`{"roleCredentials":{"accessKeyId":"ASIAFAKE"}}`))
			}))
			defer origin.Close()

			inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
				grantID: uuid.New(),
				header:  injectedHeader{name: "x-amz-sso_bearer_token", value: "the-real-session-token"},
				// Far in the future: this test is about the upstream leg, not the refresh.
				expiresAt: 0,
			}}}
			p, buf := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(origin), inj, nil)
			p.mitmHosts = map[string]bool{plainMITMHost: true}
			p.mitmPorts = map[string]int{plainMITMHost: 8090}
			p.mitmPlaintext = map[string]bool{}
			if h, _, plaintext := parseMITMHostPort(tc.entry); plaintext {
				p.mitmPlaintext[h] = true
			}

			req := httptest.NewRequest(http.MethodGet,
				"https://"+plainMITMHost+":8090/federation/credentials?account_id=111111111111", nil)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, req, plainMITMHost, 8090)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d. body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if reached := hits > 0; reached != tc.wantReached {
				t.Fatalf("the plain-HTTP origin saw %d request(s), want reached=%v — an https upstream leg "+
					"cannot dial a server that serves no TLS, which is the 502 this fixes", hits, tc.wantReached)
			}
			if tc.wantReached {
				// …and it arrived CREDENTIALED, which is the whole point of
				// terminating the tunnel rather than blind-tunnelling it.
				if gotHeader != "the-real-session-token" {
					t.Errorf("the origin saw %q on the injected header, want the brokered token — a blind "+
						"tunnel would have carried the sandbox's placeholder through untouched", gotHeader)
				}
				return
			}
			// The TLS arm fails the way it always has, and says so in the trail.
			if !strings.Contains(buf.String(), "builtin:dial-failed") {
				_ = p.sink.close(req.Context())
				if !strings.Contains(buf.String(), "builtin:dial-failed") {
					t.Errorf("a failed TLS dial at a plaintext origin did not log builtin:dial-failed: %s", buf.String())
				}
			}
		})
	}
}

const plainMITMHost = "wardyn-awsssofake.wardyn.svc.cluster.local"

// THE WHOLE LANE, THE WAY THE SANDBOX DRIVES IT: a real CONNECT through a real
// proxy listener, a real TLS handshake against the Wardyn leaf, and a real
// plain-HTTP origin behind it.
//
// This is the shape walk-3 found and no test had: the agent's SDK does not make
// the plain absolute-URI request the lane's other tests make — it CONNECTs (36-69
// times per failing run). Everything up to the termination was already right; the
// leg AFTER it dialled TLS at a server with no TLS, so the sandbox got a 502 and
// the run starved. Driving serveMITMRequest directly cannot see that, because it
// starts after the tunnel is already terminated.
func TestMITMConnect_PlaintextOriginIsReachedThroughTheTunnel(t *testing.T) {
	var gotHeader, gotPath string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader, gotPath = r.Header.Get("x-amz-sso_bearer_token"), r.URL.Path
		_, _ = w.Write([]byte(`{"roleCredentials":{"accessKeyId":"ASIAFAKE"}}`))
	}))
	defer origin.Close()

	certPEM, keyPEM := genTestCA(t)
	ca, caErr := newCertAuthority(certPEM, keyPEM)
	if caErr != nil {
		t.Fatalf("newCertAuthority: %v", caErr)
	}
	inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
		grantID: uuid.New(),
		header:  injectedHeader{name: "x-amz-sso_bearer_token", value: "the-real-session-token"},
	}}}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{plainMITMHost}}),
		Injector: inj,
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr(origin)),
		// THE ENTRY, exactly as authorBedrockSSOInjection writes it for an
		// http:// endpoint override.
		MITMHosts: []string{"http://" + plainMITMHost + ":8090"},
		CA:        ca,
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// The sandbox: CONNECT, then TLS trusting the Wardyn CA — which is what the
	// agent image does once AWS_CA_BUNDLE names it.
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxySrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, werr := io.WriteString(conn,
		"CONNECT "+plainMITMHost+":8090 HTTP/1.1\r\nHost: "+plainMITMHost+":8090\r\n\r\n"); werr != nil {
		t.Fatal(werr)
	}
	br := bufio.NewReader(conn)
	resp, rerr := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if rerr != nil {
		t.Fatalf("read CONNECT response: %v", rerr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200 — the tunnel must be TERMINATED, not refused: a blind "+
			"tunnel would carry the sandbox's placeholder straight through to the portal", resp.StatusCode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not trust the Wardyn CA")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: plainMITMHost, RootCAs: pool})
	if herr := tlsConn.Handshake(); herr != nil {
		t.Fatalf("TLS handshake against the Wardyn leaf: %v", herr)
	}
	defer func() { _ = tlsConn.Close() }()

	req, _ := http.NewRequest(http.MethodGet,
		"https://"+plainMITMHost+":8090/federation/credentials?account_id=111111111111", nil)
	// The sandbox's OWN placeholder, which the proxy must strip and replace.
	req.Header.Set("x-amz-sso_bearer_token", "wardyn-proxy-injected")
	if werr := req.Write(tlsConn); werr != nil {
		t.Fatalf("write the request into the tunnel: %v", werr)
	}
	got, gerr := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if gerr != nil {
		t.Fatalf("read the response: %v", gerr)
	}
	defer func() { _ = got.Body.Close() }()
	body, _ := io.ReadAll(got.Body)

	if got.StatusCode != http.StatusOK {
		t.Fatalf("GetRoleCredentials through the tunnel = %d (%s), want 200. With the upstream leg hard-coded "+
			"to https this is the 502 that starved every model call in walk-3.\n--- decisions ---\n%s",
			got.StatusCode, strings.TrimSpace(string(body)), buf.String())
	}
	if gotPath != "/federation/credentials" {
		t.Errorf("the origin saw path %q, want /federation/credentials", gotPath)
	}
	if gotHeader != "the-real-session-token" {
		t.Errorf("the origin saw %q on the injected header, want the brokered token — the sandbox's "+
			"placeholder must be stripped and replaced, which is the only reason to terminate at all", gotHeader)
	}
}

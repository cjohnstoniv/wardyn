// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// THE WHOLE LANE, THE WAY THE SANDBOX ACTUALLY DRIVES IT.
//
// An earlier version of this test drove a TLS client into the tunnel, because
// that is what a MITM is "supposed" to see. It is not what happens, and the
// assumption is what let walk-5 stay red after the upstream leg was fixed — see
// the case below, and SDK-PATH.md for the measurement. A TLS client against a
// TLS entry is production, and mitm_test.go's suite owns it.

// THE CLIENT LEG, MEASURED (SDK-PATH.md). The test above drives the tunnel the
// way a TLS client does. The agent's SDK does NOT: with a proxy configured it
// reaches an `http://` endpoint by CONNECT and then sends PLAINTEXT inside the
// tunnel — first byte 0x47, `G`, never 0x16. Reproduced offline against the real
// wardyn/agent-claude-code:local, and it is why walk-5 was still red after the
// upstream leg was fixed: mitmConnect handshook at that client, failed, and
// dropped the connection, so the request was never seen, never injected and
// never forwarded. The SDK retried 36-69 times a run and the portal saw nothing.
func TestMITMConnect_PlaintextClientInsideTheTunnelIsServed(t *testing.T) {
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
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{plainMITMHost}}),
		Injector:  inj,
		Sink:      &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:  publicResolver{},
		Dial:      redirectDial(upstreamAddr(origin)),
		MITMHosts: []string{"http://" + plainMITMHost + ":8090"},
		CA:        ca,
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

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
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// PLAINTEXT into the tunnel — no TLS handshake, which is the SDK's shape.
	req, _ := http.NewRequest(http.MethodGet,
		"http://"+plainMITMHost+":8090/federation/credentials?account_id=111111111111", nil)
	req.Header.Set("x-amz-sso_bearer_token", "wardyn-proxy-injected")
	if werr := req.Write(conn); werr != nil {
		t.Fatalf("write the plaintext request into the tunnel: %v", werr)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, gerr := http.ReadResponse(br, req)
	if gerr != nil {
		t.Fatalf("a PLAINTEXT client inside the tunnel got no response (%v) — mitmConnect handshook TLS at "+
			"it and dropped the connection, which is exactly what left the portal seeing nothing", gerr)
	}
	defer func() { _ = got.Body.Close() }()
	body, _ := io.ReadAll(got.Body)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200\n--- decisions ---\n%s",
			got.StatusCode, strings.TrimSpace(string(body)), buf.String())
	}
	if gotPath != "/federation/credentials" {
		t.Errorf("the origin saw path %q, want /federation/credentials", gotPath)
	}
	if gotHeader != "the-real-session-token" {
		t.Errorf("the origin saw %q on the injected header, want the brokered token — the tunnel is "+
			"TERMINATED precisely so the sandbox's placeholder can be stripped and replaced", gotHeader)
	}
}

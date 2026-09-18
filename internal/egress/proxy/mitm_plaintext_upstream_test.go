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
			if h, port, plaintext := parseMITMHostPort(tc.entry); plaintext {
				p.mitmPlaintext[plaintextKey(h, port)] = true
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

// BOTH CLIENTS, ONE PLAINTEXT ENTRY. The tunnel is terminated either way; what
// the entry's scheme decides is the ORIGIN, and what the first byte decides is
// the CLIENT. They are different questions and the proxy answers them
// separately, so a plaintext entry still serves an ordinary TLS client (curl,
// anything with the CA installed) while also serving the plaintext one the AWS
// SDK actually sends (SDK-PATH.md).
//
// An earlier version of this file assumed only the TLS shape, which is what a
// MITM is "supposed" to see — and that assumption is what let walk-5 stay red
// after the upstream leg was fixed.

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

// …and the TLS client against that SAME plaintext entry still works: the peek
// sees a ClientHello and the unchanged TLS path runs. Without this the fix
// would have traded one broken client for another.
func TestMITMConnect_TLSClientAgainstAPlaintextEntryStillWorks(t *testing.T) {
	var gotHeader string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-amz-sso_bearer_token")
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
	if rerr != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT: %v / %v", rerr, resp)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not trust the Wardyn CA")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: plainMITMHost, RootCAs: pool})
	if herr := tlsConn.Handshake(); herr != nil {
		t.Fatalf("a TLS client against a plaintext ENTRY was refused its handshake: %v — the entry's "+
			"scheme describes the ORIGIN, not the client", herr)
	}
	defer func() { _ = tlsConn.Close() }()

	req, _ := http.NewRequest(http.MethodGet,
		"https://"+plainMITMHost+":8090/federation/credentials?account_id=111111111111", nil)
	req.Header.Set("x-amz-sso_bearer_token", "wardyn-proxy-injected")
	if werr := req.Write(tlsConn); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	got, gerr := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if gerr != nil {
		t.Fatalf("read: %v", gerr)
	}
	defer func() { _ = got.Body.Close() }()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\n--- decisions ---\n%s", got.StatusCode, buf.String())
	}
	if gotHeader != "the-real-session-token" {
		t.Errorf("the origin saw %q, want the brokered token", gotHeader)
	}
}

// ONE HOST, TWO ENTRIES, TWO SCHEMES (W6-S F2). The scheme belongs to the
// ENTRY, and entries are port-scoped: a cleartext entry on :8090 must not make
// the TLS entry on :443 for the same host re-originate in cleartext. Keyed by
// host alone it did — the flag was sticky while the port map was
// last-writer-wins.
func TestCompileMITMHosts_PlaintextIsPortScoped(t *testing.T) {
	_, _, plaintext := compileMITMHosts([]string{"http://fake.internal:8090", "fake.internal:443"})
	p := &Proxy{mitmPlaintext: plaintext}
	if !p.mitmPlaintextUpstream("fake.internal", 8090) {
		t.Error("the http:// entry on :8090 is not plaintext")
	}
	if p.mitmPlaintextUpstream("fake.internal", 443) {
		t.Error("the TLS entry on :443 was made cleartext by the other entry's scheme")
	}
	if scheme, _ := p.upstreamSchemeFor("fake.internal", 443); scheme != "https" {
		t.Errorf("upstreamSchemeFor(:443) = %q, want https", scheme)
	}
	if scheme, _ := p.upstreamSchemeFor("fake.internal", 8090); scheme != "http" {
		t.Errorf("upstreamSchemeFor(:8090) = %q, want http", scheme)
	}
	// A host with no plaintext entry at all is TLS on every port — production.
	if p.mitmPlaintextUpstream("portal.sso.eu-west-2.amazonaws.com", 443) {
		t.Error("a host with no entry was treated as cleartext")
	}
}

// ─── the path/query pin (W6-S F3) ────────────────────────────────────────────

// THE INJECTED SESSION RIDES ONE REQUEST SHAPE, NOT ONE HOST.
//
// Before this, the resolved header went on whatever the sandbox sent to the
// portal host. That includes `POST /logout`, which AWS documents as invalidating
// the owner's server-side sign-in session — for every run they have, not just
// this one — and a GetRoleCredentials naming any other account or role the
// session holds. 0.7.5 could not narrow it (the token was resident in the
// sandbox, so its reach was the agent's); proxy-side injection is the first
// point at which the dispatched pair becomes enforced rather than asserted.
//
// A refused request is FORWARDED, not blocked: it simply carries no credential,
// and the origin answers it as it answers any unauthenticated call. Nothing
// Wardyn holds is exposed either way, and a sandbox cannot tell a withheld
// header from an expired session.
func TestMITMInjection_IsPinnedToTheDispatchedRoleCredentialsCall(t *testing.T) {
	const (
		account = "111122223333"
		role    = "WardynAgent"
	)
	for _, tc := range []struct {
		name       string
		pinned     bool
		method     string
		path       string
		query      string
		wantHeader string
	}{
		{"the dispatched GetRoleCredentials", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&role_name=" + role, "the-real-session-token"},
		{"logout — a sandbox must not end its owner's sign-in session", true, http.MethodPost, "/logout", "", ""},
		{"another account's credentials", true, http.MethodGet, "/federation/credentials",
			"account_id=999988887777&role_name=" + role, ""},
		{"another role's credentials", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&role_name=AdministratorAccess", ""},
		{"the right path, the wrong verb", true, http.MethodPost, "/federation/credentials",
			"account_id=" + account + "&role_name=" + role, ""},
		{"listing the session's accounts", true, http.MethodGet, "/assignment/accounts", "", ""},
		// THE AMBIGUOUS QUERIES (security re-round SHOULD-1). Each carries the
		// pinned pair AND a second account or role. Matching on url.Values.Get
		// accepted all four, with the credential attached and RawQuery forwarded
		// verbatim — so whether a second account was honoured was the ORIGIN's
		// decision, not Wardyn's. The real portal's duplicate-parameter and ';'
		// semantics are undocumented, which is the reason to refuse rather than
		// to reason.
		{"a duplicated account_id", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&account_id=999988887777&role_name=" + role, ""},
		{"a duplicated role_name", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&role_name=" + role + "&role_name=AdministratorAccess", ""},
		// Go >= 1.17 DROPS a pair containing ';', so the pin never saw this one.
		{"a semicolon-separated second account", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&role_name=" + role + "&x=1;account_id=999988887777", ""},
		{"a percent-encoded second spelling of the key", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&account%5Fid=999988887777&role_name=" + role, ""},
		// …and an unrelated extra key is still fine: the pin narrows WHICH
		// account and role the session may be spent on, not what else may be asked.
		{"an unrelated extra parameter", true, http.MethodGet, "/federation/credentials",
			"account_id=" + account + "&role_name=" + role + "&debug=1", "the-real-session-token"},
		// The UNPINNED rule is every other lane, and it is unchanged: the
		// credential rides whatever the sandbox sends, exactly as before.
		{"unpinned — logout still carries it, as it always did", false, http.MethodPost, "/logout", "", "the-real-session-token"},
		{"unpinned — any account, as it always did", false, http.MethodGet, "/federation/credentials",
			"account_id=999988887777", "the-real-session-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader string
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("x-amz-sso_bearer_token")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer origin.Close()

			rule := egress.InjectionRule{Host: plainMITMHost, Header: "x-amz-sso_bearer_token", Format: "%s"}
			if tc.pinned {
				rule.PinPath = "/federation/credentials"
				rule.PinQuery = map[string]string{"account_id": account, "role_name": role}
			}
			inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
				grantID: uuid.New(),
				header:  injectedHeader{name: "x-amz-sso_bearer_token", value: "the-real-session-token"},
				rule:    rule,
			}}}
			p, _ := newLocalRouteProxy(t, "http://cp.invalid", "RUNTOK", upstreamAddr(origin), inj, nil)
			p.mitmHosts = map[string]bool{plainMITMHost: true}
			p.mitmPorts = map[string]int{plainMITMHost: 8090}
			p.mitmPlaintext = map[string]bool{plaintextKey(plainMITMHost, 8090): true}

			u := "https://" + plainMITMHost + ":8090" + tc.path
			if tc.query != "" {
				u += "?" + tc.query
			}
			req := httptest.NewRequest(tc.method, u, nil)
			// The sandbox's own placeholder, which the strip removes either way.
			req.Header.Set("x-amz-sso_bearer_token", "wardyn-proxy-injected")
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, req, plainMITMHost, 8090)

			if gotHeader != tc.wantHeader {
				t.Errorf("the origin saw %q on the injected header, want %q", gotHeader, tc.wantHeader)
			}
			// FORWARDED either way — a withheld credential is not a refusal.
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200: a request the pin does not cover is still forwarded, "+
					"it just carries no credential", rec.Code)
			}
			// …and the sandbox's placeholder never reaches the origin, pinned or not.
			if gotHeader == "wardyn-proxy-injected" {
				t.Error("the sandbox's own placeholder was forwarded — the strip must run whatever the pin says")
			}
		})
	}
}

// THE PLAIN LANE HONOURS THE PIN TOO (docs REVIEW-3 coverage note).
//
// injector.apply is the cleartext path — an ordinary absolute-URI request that
// never enters a tunnel — and it reaches the very same portal host. A pin
// enforced only on the MITM lane would be bypassable by simply not using TLS:
// `POST /logout` sent plainly would be injected while the tunnelled one was not.
//
// The STRIP runs on every arm: a host with an injection rule always has the
// sandbox's own credential headers removed, including the one that rule
// supplies, so a withheld injection never degrades into forwarding whatever the
// sandbox chose to send.
func TestInjectorApply_PlainLaneHonoursThePin(t *testing.T) {
	const (
		account = "111122223333"
		role    = "WardynAgent"
	)
	for _, tc := range []struct {
		name       string
		method     string
		target     string
		wantHeader string
	}{
		{"the dispatched GetRoleCredentials", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account + "&role_name=" + role,
			"the-real-session-token"},
		{"logout", http.MethodPost, "http://" + plainMITMHost + ":8090/logout", ""},
		{"another account", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=999988887777&role_name=" + role, ""},
		{"another role", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account + "&role_name=AdministratorAccess", ""},
		{"the session's account list", http.MethodGet,
			"http://" + plainMITMHost + ":8090/assignment/accounts", ""},
		// The same four ambiguous queries, on the lane that needs no tunnel.
		{"a duplicated account_id", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account +
				"&account_id=999988887777&role_name=" + role, ""},
		{"a duplicated role_name", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account +
				"&role_name=" + role + "&role_name=AdministratorAccess", ""},
		{"a semicolon-separated second account", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account +
				"&role_name=" + role + "&x=1;account_id=999988887777", ""},
		{"a percent-encoded second spelling of the key", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account +
				"&account%5Fid=999988887777&role_name=" + role, ""},
		{"an unrelated extra parameter", http.MethodGet,
			"http://" + plainMITMHost + ":8090/federation/credentials?account_id=" + account +
				"&role_name=" + role + "&debug=1", "the-real-session-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
				grantID: uuid.New(),
				header:  injectedHeader{name: "x-amz-sso_bearer_token", value: "the-real-session-token"},
				rule: egress.InjectionRule{
					Host: plainMITMHost, Header: "x-amz-sso_bearer_token", Format: "%s",
					PinPath:  "/federation/credentials",
					PinQuery: map[string]string{"account_id": account, "role_name": role},
				},
			}}}
			req := httptest.NewRequest(tc.method, tc.target, nil)
			// The sandbox's own placeholder, plus a credential header it has no
			// business setting on a host Wardyn credentials.
			req.Header.Set("x-amz-sso_bearer_token", "wardyn-proxy-injected")
			req.Header.Set("Authorization", "Bearer the-sandboxs-own")

			inj.apply(req, plainMITMHost, 8090)

			if got := req.Header.Get("x-amz-sso_bearer_token"); got != tc.wantHeader {
				t.Errorf("injected header = %q, want %q", got, tc.wantHeader)
			}
			if got := req.Header.Get("x-amz-sso_bearer_token"); got == "wardyn-proxy-injected" {
				t.Error("the sandbox's own placeholder survived: the strip must run whatever the pin decides")
			}
			if got := req.Header.Get("Authorization"); got != "" {
				t.Errorf("Authorization = %q, want it stripped on a host this rule credentials", got)
			}
		})
	}
}

// …and an UNPINNED rule on the plain lane is unchanged, which is every other
// injection lane in the product.
func TestInjectorApply_PlainLaneUnpinnedRuleIsUnchanged(t *testing.T) {
	inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
		grantID: uuid.New(),
		header:  injectedHeader{name: "Authorization", value: "Bearer brokered"},
		rule:    egress.InjectionRule{Host: plainMITMHost, Header: "Authorization", Format: "Bearer %s"},
	}}}
	req := httptest.NewRequest(http.MethodPost, "http://"+plainMITMHost+":8090/anything", nil)
	req.Header.Set("Authorization", "Bearer the-sandboxs-own")
	inj.apply(req, plainMITMHost, 8090)
	if got := req.Header.Get("Authorization"); got != "Bearer brokered" {
		t.Errorf("Authorization = %q, want the brokered credential on every path — an unpinned rule "+
			"narrows nothing", got)
	}
}

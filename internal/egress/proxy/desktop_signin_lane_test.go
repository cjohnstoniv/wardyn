// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// signinLaneRig is one run's proxy serving the agent's listener and the
// person's sign-in listener, with a control plane that accepts goodCred only.
type signinLaneRig struct {
	agentAddr, signinAddr string
	goodCred              string
	caPEM                 []byte
	logs                  *lockedBuf // decision rows, slog and the server's error log
	upstream              *capturedUpstream
	checks                atomic.Int32
}

// newSigninLaneRig builds the rig; each with edits the proxy's options first.
func newSigninLaneRig(t *testing.T, with ...func(*Options)) *signinLaneRig {
	t.Helper()
	rig := &signinLaneRig{logs: &lockedBuf{}, upstream: captureUpstream(t, true, "site-ok")}
	rig.goodCred = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

	cp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.checks.Add(1)
		var body struct{ Credential string }
		if r.URL.Path != signinCheckPath || r.Header.Get("Authorization") != "Bearer RUNTOK" ||
			json.NewDecoder(r.Body).Decode(&body) != nil || body.Credential != rig.goodCred {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(cp.Close)

	certPEM, keyPEM := genTestCA(t)
	rig.caPEM = certPEM
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"site.test"}}),
		Sink:   &decisionSink{out: rig.logs, ch: make(chan egress.DecisionLog, 64)},
		CA:     ca,
		// The agent's listener terminates site.test: the lane must not borrow that.
		MITMHosts:       []string{"site.test"},
		Resolver:        publicResolver{},
		Dial:            splitDial(upstreamAddr(cp), upstreamAddr(rig.upstream.srv)),
		ControlPlaneURL: "https://wardynd.test:8080", RunToken: newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
	}
	for _, edit := range with {
		edit(&opts)
	}
	p := newProxy(opts)
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := &http.Server{Handler: p, ConnContext: signinConnContext, ReadHeaderTimeout: 5 * time.Second,
		ErrorLog: log.New(rig.logs, "", 0)}
	agentLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	signinLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rig.agentAddr, rig.signinAddr = agentLn.Addr().String(), signinLn.Addr().String()
	go func() { _ = srv.Serve(agentLn) }()
	go func() { _ = srv.Serve(signinListener{signinLn}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return rig
}

func signinAuth(cred string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(signinProxyUser+":"+cred))
}

// connect sends one CONNECT to addr and returns the conn, its reader and the status.
func (rig *signinLaneRig) connect(t *testing.T, addr, target, auth string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return c, br, resp.StatusCode
}

// get runs one HTTPS GET for path through an open tunnel, trusting roots
// (nil = trust nothing but say so: InsecureSkipVerify, recording the issuer).
func getThrough(t *testing.T, c net.Conn, br *bufio.Reader, roots *x509.CertPool, path string) (string, *x509.Certificate, error) {
	t.Helper()
	cfg := &tls.Config{ServerName: "site.test", RootCAs: roots}
	if roots == nil {
		cfg.InsecureSkipVerify = true //nolint:gosec // test-only: the issuer is asserted below
	}
	tc := tls.Client(&readerConn{Conn: c, r: br}, cfg)
	if err := tc.Handshake(); err != nil {
		return "", nil, err
	}
	leaf := tc.ConnectionState().PeerCertificates[0]
	req, _ := http.NewRequest(http.MethodGet, "https://site.test"+path, nil)
	if err := req.Write(tc); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), req)
	if err != nil {
		return "", leaf, err
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = tc.Close()
	return string(b), leaf, nil
}

// TestSigninLane_TunnelOnlyLeavesNoTarget: the person's sign-in browser
// reaches a site through an opaque tunnel (the site's own certificate, not the
// run CA's), and the callback's code= appears in no decision row, slog line or
// server error, while the decision row still names the host.
func TestSigninLane_TunnelOnlyLeavesNoTarget(t *testing.T) {
	rig := newSigninLaneRig(t)
	c, br, status := rig.connect(t, rig.signinAddr, "site.test:443", signinAuth(rig.goodCred))
	if status != http.StatusOK {
		t.Fatalf("sign-in CONNECT status = %d, want 200", status)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rig.caPEM)
	body, leaf, err := getThrough(t, c, br, nil, "/cb?code=SECRET")
	if err != nil {
		t.Fatalf("GET through the sign-in tunnel: %v", err)
	}
	if body != "site-ok" || rig.upstream.path != "/cb" {
		t.Fatalf("the site did not answer through the tunnel: body %q path %q", body, rig.upstream.path)
	}
	if _, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "site.test"}); verr == nil {
		t.Fatal("the sign-in tunnel presented a run-CA leaf: the lane terminated TLS")
	}
	_ = c.Close()

	// A plain-HTTP request on the lane is refused without reading its target.
	pc, err := net.Dial("tcp", rig.signinAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, _ = io.WriteString(pc, "GET http://site.test/cb?code=SECRET HTTP/1.1\r\nHost: site.test\r\nProxy-Authorization: "+
		signinAuth(rig.goodCred)+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(pc), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("plain HTTP on the sign-in lane = %d, want 405", resp.StatusCode)
	}

	logs := rig.logs.String()
	if strings.Contains(logs, "SECRET") || strings.Contains(logs, "/cb") {
		t.Fatalf("the sign-in lane logged a request target:\n%s", logs)
	}
	if !strings.Contains(logs, `"Host":"site.test","Port":443,"Method":"CONNECT","Path":""`) {
		t.Fatalf("the sign-in tunnel left no host-only decision row:\n%s", logs)
	}
}

// TestSigninLane_AgentCannotBorrowTheBypass: the identical request from the
// agent's listener is still terminated per policy even when it carries a valid
// sign-in credential, and the sign-in listener refuses anything without one.
func TestSigninLane_AgentCannotBorrowTheBypass(t *testing.T) {
	rig := newSigninLaneRig(t)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(rig.caPEM)

	c, br, status := rig.connect(t, rig.agentAddr, "site.test:443", signinAuth(rig.goodCred))
	if status != http.StatusOK {
		t.Fatalf("agent CONNECT status = %d, want 200", status)
	}
	if _, _, err := getThrough(t, c, br, pool, "/cb?code=SECRET"); err != nil {
		t.Fatalf("the agent's tunnel must still be terminated with the run CA: %v", err)
	}

	before := rig.checks.Load()
	for _, auth := range []string{"", signinAuth(base64.RawURLEncoding.EncodeToString(make([]byte, 32))),
		"Basic " + base64.StdEncoding.EncodeToString([]byte("someone:"+rig.goodCred)), "Bearer " + rig.goodCred} {
		if _, _, status := rig.connect(t, rig.signinAddr, "site.test:443", auth); status != http.StatusProxyAuthRequired {
			t.Fatalf("sign-in CONNECT with %q = %d, want 407", auth, status)
		}
	}
	if got := rig.checks.Load() - before; got != 1 {
		t.Fatalf("control-plane checks = %d, want 1 (only the well-formed unknown credential is asked about)", got)
	}
}

// TestSigninLane_PolicyStillDecidesByHost: an accepted credential opens no
// host the policy refuses.
func TestSigninLane_PolicyStillDecidesByHost(t *testing.T) {
	rig := newSigninLaneRig(t)
	if _, _, status := rig.connect(t, rig.signinAddr, "other.test:443", signinAuth(rig.goodCred)); status != http.StatusForbidden {
		t.Fatalf("sign-in CONNECT to a refused host = %d, want 403", status)
	}
}

// TestSigninLane_DoorHostsStayRefused: a host the agent reaches only through a
// checked door (here the kubectl row's API server and an AWS re-signed host)
// is refused on the sign-in listener too, by the door's own refusal, while an
// allowed sign-in host still tunnels.
func TestSigninLane_DoorHostsStayRefused(t *testing.T) {
	rig := newSigninLaneRig(t, func(o *Options) {
		o.Policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"site.test", "k8s.test:6443", sigv4S3 + ":443"}})
		o.KubeAPI = &KubeAPIGrant{Host: "k8s.test", Port: 6443, Namespaces: []string{"team-a"}}
		o.AWSSigV4 = &AWSSigV4Config{GrantID: uuid.New(), PortalHost: sigv4Portal, AccountID: sigv4Account, RoleName: sigv4Role,
			Hosts: []string{sigv4S3}}
	})
	for target, door := range map[string]func(*http.Response, string) bool{
		"k8s.test:6443":  func(_ *http.Response, body string) bool { return strings.Contains(body, kubeAPIRefused) },
		sigv4S3 + ":443": func(resp *http.Response, _ string) bool { return resp.Header.Get("X-Wardyn-Reason") == awsSigV4Scope },
	} {
		c, err := net.Dial("tcp", rig.signinAddr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\nProxy-Authorization: "+
			signinAuth(rig.goodCred)+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("read CONNECT %s response: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = c.Close()
		if resp.StatusCode != http.StatusForbidden || !door(resp, string(body)) {
			t.Errorf("sign-in CONNECT %s = %d %q, want the door's 403 refusal", target, resp.StatusCode, body)
		}
	}
	if _, _, status := rig.connect(t, rig.signinAddr, "site.test:443", signinAuth(rig.goodCred)); status != http.StatusOK {
		t.Fatalf("sign-in CONNECT to an allowed host = %d, want 200", status)
	}
}

func TestSigninCredentialShape(t *testing.T) {
	good := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if got, ok := signinCredential(signinAuth(good)); !ok || got != good {
		t.Fatalf("well-formed credential refused: %q %v", got, ok)
	}
	for _, h := range []string{"", "Basic", "Basic !!!", signinAuth("short"), signinAuth(good + "A"),
		"Basic " + base64.StdEncoding.EncodeToString([]byte("agent:"+good))} {
		if _, ok := signinCredential(h); ok {
			t.Errorf("signinCredential(%q) accepted", h)
		}
	}
}

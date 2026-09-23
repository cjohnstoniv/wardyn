// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/adofake"
)

// TestADOEntraLane_EndToEnd is the proof the whole runtime path works: a run
// DISPATCHED on the per-person Azure DevOps lane boots the REAL proxy sidecar
// from its own authored configuration, and through it
//
//   - a read inside the grant is intercepted, credentialed with the person's
//     bearer and forwarded;
//   - a write OUTSIDE the grant is refused by the REST gate before any byte
//     reaches Azure DevOps;
//   - a read addressed to ANOTHER organisation is refused (the pin the token
//     itself does not carry).
//
// Wire: authorADOEntraLane -> runner.BuildProxyConfig -> proxy.LoadConfigBytes
// -> proxy.NewServer -> a sandbox client CONNECTing to dev.azure.com:443 and
// trusting only the run's own CA. The sidecar's upstream leg is routed through
// a stand-in corporate proxy that terminates TLS for dev.azure.com and hands
// the request to test/adofake — which answers any credential it knows, so a
// refusal can only be Wardyn's. The control plane's resolve is stubbed to what
// resolveADOInjection answers (that arm has its own tests).
func TestADOEntraLane_EndToEnd(t *testing.T) {
	const bearer = "entra-bearer-for-contoso"
	fake := adofake.New()
	t.Cleanup(fake.Close)
	fake.RegisterToken(bearer, adofake.ScopeCodeRead, adofake.ScopeCodeWrite, adofake.ScopeWorkRead,
		adofake.ScopeWorkWrite, adofake.ScopeProjectRead)
	fake.AddProject("contoso", "", "proj")
	fake.AddProject("fabrikam", "", "loot")

	upstreamCA := newTestUpstreamCA(t, "dev.azure.com")
	upstream, seen := countingUpstream(t, fake.URL())
	corp := newTLSTerminatingCorpProxy(t, upstreamCA.leaf, upstream)

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "Authorization", Value: "Bearer " + bearer, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			Organisation: "contoso", Capabilities: []string{"read", "code_write"},
		})
	}))
	t.Cleanup(cp.Close)

	// DISPATCH: the lane as dispatchRun calls it, with a real per-run CA.
	caCert, caKey, err := generateRunCA(time.Now())
	if err != nil {
		t.Fatalf("generateRunCA: %v", err)
	}
	st := &adoTestStore{}
	s, _ := newADODispatchServer(st)
	policy := types.RunPolicySpec{}
	runID := uuid.New()
	lane, ok := s.authorADOEntraLane(context.Background(), types.AgentRun{ID: runID}, adoTestRun(t), true,
		adoEntraUngraded(), dispatchLLMPlan{mitmCACertPEM: string(caCert), mitmCAKeyPEM: string(caKey)}, &policy, map[string]string{}, nil)
	if !ok || len(lane.gate) != 1 {
		t.Fatalf("dispatch: ok=%v gate=%+v", ok, lane.gate)
	}

	listen := startADOLaneSidecar(t, runID, lane, policy, string(caCert), string(caKey), cp.URL, corp, upstreamCA.caPEM)

	// THE SANDBOX: trusts only the run's CA and holds only the placeholder.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)
	proxyURL, _ := url.Parse("http://" + listen)
	sandbox := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	send := func(method, target, body string) (int, string) {
		t.Helper()
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(method, target, strings.NewReader(body))
			req.Header.Set("Authorization", "Basic "+adoEntraPlaceholderValue)
			if body != "" {
				req.Header.Set("Content-Type", "application/json-patch+json")
			}
			if resp, err = sandbox.Do(req); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", method, target, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	// 1. A read inside the grant: forwarded, with the bearer attached.
	if code, body := send(http.MethodGet, "https://dev.azure.com/contoso/_apis/projects?api-version=7.1", ""); code != http.StatusOK {
		t.Fatalf("granted read: status %d body %s", code, body)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || reqs[0].Token != bearer || !reqs[0].Authorized {
		t.Fatalf("upstream saw %+v, want one authorized request carrying the person's bearer", reqs)
	}
	if strings.Contains(reqs[0].Headers.Get("Authorization"), adoEntraPlaceholderValue) {
		t.Fatal("the sandbox's placeholder reached Azure DevOps")
	}

	// 2. A write outside the grant (work_write): refused by the gate, never forwarded.
	code, body := send(http.MethodPatch, "https://dev.azure.com/contoso/proj/_apis/wit/workitems/1?api-version=7.1",
		`[{"op":"add","path":"/fields/System.Title","value":"x"}]`)
	if code != http.StatusForbidden || !strings.Contains(body, "CapabilityNotGranted") {
		t.Errorf("ungranted write: status %d body %s, want the gate's 403", code, body)
	}
	// 3. Another organisation: refused by the organisation pin.
	if code, body := send(http.MethodGet, "https://dev.azure.com/fabrikam/_apis/projects?api-version=7.1", ""); code != http.StatusForbidden {
		t.Errorf("cross-organisation read: status %d body %s, want 403", code, body)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want only the granted read", n)
	}
	// 4. The plain forward lane: the same requests as absolute-form https://
	// request-lines with no CONNECT are refused before any byte leaves.
	assertADOPlainLaneRefused(t, listen, seen)
}

// startADOLaneSidecar boots the REAL proxy sidecar from the configuration
// dispatch authors for lane — runner.BuildProxyConfig, proxy.LoadConfigBytes,
// proxy.NewServer — with its upstream leg routed through corp, and returns its
// listen address.
func startADOLaneSidecar(t *testing.T, runID uuid.UUID, lane adoEntraLane, policy types.RunPolicySpec,
	caCert, caKey, cpURL, corp, upstreamCAPEM string,
) string {
	t.Helper()
	port := freeLoopbackPort(t)
	raw, err := runner.BuildProxyConfig(runID, runner.ProxyConfig{
		RunToken: "run-token", ControlPlaneURL: cpURL, Policy: policy, Injection: lane.injections,
		MITMCACertPEM: caCert, MITMCAKeyPEM: caKey, MITMHosts: lane.mitmHosts,
		ADOGrants: lane.gate, UpstreamProxyURL: "http://" + corp, TrustedCAPEM: upstreamCAPEM,
	}, port)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	cfg.Listen = "127.0.0.1:" + strconv.Itoa(port)
	psrv, err := proxy.NewServer(context.Background(), cfg, &http.Client{Timeout: 5 * time.Second}, io.Discard)
	if err != nil {
		t.Fatalf("sidecar boot: %v", err)
	}
	go func() { _ = psrv.ListenAndServe() }()
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = psrv.Shutdown(sctx)
	})
	return cfg.Listen
}

// adoPlainLaneCases are absolute-form `https://` requests written straight to
// the proxy port with no CONNECT, which the proxy serves on its plain forward
// lane. That lane never runs the REST gate, so every one must be refused there.
var adoPlainLaneCases = []struct{ name, method, target, body string }{
	{"another organisation", http.MethodGet, "https://dev.azure.com/fabrikam/_apis/projects?api-version=7.1", ""},
	{"an ungranted write", http.MethodPatch, "https://dev.azure.com/contoso/proj/_apis/wit/workitems/1?api-version=7.1",
		`[{"op":"add","path":"/fields/System.Title","value":"x"}]`},
	{"the token area", http.MethodGet, "https://dev.azure.com/contoso/_apis/tokens/pats?api-version=7.1-preview.1", ""},
	{"a git push", http.MethodPost, "https://dev.azure.com/contoso/proj/_git/app/git-receive-pack", "0000"},
	{"a granted read, host spelled with case, a trailing dot and a port", http.MethodGet,
		"https://DEV.Azure.com.:443/contoso/_apis/projects?api-version=7.1", ""},
}

// assertADOPlainLaneRefused sends every adoPlainLaneCases request over raw TCP
// to the proxy at addr and asserts the Azure DevOps-shaped 403 and that the
// upstream counter did not move.
func assertADOPlainLaneRefused(t *testing.T, addr string, seen *atomic.Int64) {
	t.Helper()
	for _, tc := range adoPlainLaneCases {
		before := seen.Load()
		code, body := sendRawToProxy(t, addr, tc.method, tc.target, tc.body)
		if code != http.StatusForbidden || !strings.Contains(body, "CapabilityNotGranted") {
			t.Errorf("plain lane, %s: status %d body %s, want the Azure DevOps 403", tc.name, code, body)
		}
		if n := seen.Load() - before; n != 0 {
			t.Errorf("plain lane, %s: the upstream saw %d request(s), want none", tc.name, n)
		}
	}
}

// sendRawToProxy writes one absolute-form request-line to the proxy listener
// over raw TCP, the way a sandbox process that ignores CONNECT would.
func sendRawToProxy(t *testing.T, addr, method, target, body string) (int, string) {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ { // wait for the listener
		if c, err = net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	req := method + " " + target + " HTTP/1.1\r\nHost: dev.azure.com\r\nConnection: close\r\n" +
		"Authorization: Basic " + adoEntraPlaceholderValue + "\r\n"
	if body != "" {
		req += "Content-Type: application/json-patch+json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"+body); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

// countingUpstream fronts the fake with a server that counts every request that
// reaches Azure DevOps' side of the wire, matched route or not.
func countingUpstream(t *testing.T, fakeURL string) (string, *atomic.Int64) {
	t.Helper()
	target, _ := url.Parse(fakeURL)
	rp := httputil.NewSingleHostReverseProxy(target)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

type testUpstreamCA struct {
	caPEM string
	leaf  tls.Certificate
}

// newTestUpstreamCA mints a CA and a leaf for host, standing in for the public
// PKI the sidecar's upstream leg would otherwise verify against.
func newTestUpstreamCA(t *testing.T, host string) testUpstreamCA {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test upstream CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf: %v", err)
	}
	return testUpstreamCA{
		caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		leaf:  tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
	}
}

// newTLSTerminatingCorpProxy is a CONNECT proxy that, instead of tunnelling,
// terminates TLS with leaf and serves the request from the plain-HTTP fake.
// Returns its host:port.
func newTLSTerminatingCorpProxy(t *testing.T, leaf tls.Certificate, fakeURL string) string {
	t.Helper()
	target, _ := url.Parse(fakeURL)
	rp := httputil.NewSingleHostReverseProxy(target)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					_ = c.Close()
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
				_ = (&http.Server{Handler: rp, ReadHeaderTimeout: 5 * time.Second}).Serve(&oneConnListener{c: tc})
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// oneConnListener serves exactly one connection, then reports closed.
type oneConnListener struct {
	c    net.Conn
	done bool
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, net.ErrClosed
	}
	l.done = true
	return l.c, nil
}
func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The proxied fetch's tests. Every proxy, resolver and issuer here is in-process: the fake resolvers answer
// public test addresses, and the fake proxies map those addresses to loopback listeners, so no real address is
// contacted.

const proxyTestIssuer = "https://login.corp.example/oauth2/a"

var (
	proxyTestV6   = net.ParseIP("2606:4700:4700::1111")
	proxyTestV4   = net.ParseIP("8.8.8.8")
	proxyTestV4b  = net.ParseIP("8.8.4.4")
	proxyTestV6At = "[2606:4700:4700::1111]:443"
	proxyTestV4At = "8.8.8.8:443"
)

// testCA is a certificate authority made for one test.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t testing.TB, name string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

func (ca *testCA) leaf(t testing.TB, names []string, ips []net.IP) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), DNSNames: names, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeIssuer is a TLS server for fetch-level tests: it answers any path with a small JSON document and records
// each request's host, headers and TLS server name.
type fakeIssuer struct {
	srv *httptest.Server

	mu       sync.Mutex
	hits     int
	hosts    []string
	headers  []http.Header
	sni      []string
	delay    time.Duration
	servedAt time.Time
	drop     bool // finish the TLS handshake, read the request, then close with no response
}

func newFakeIssuer(t *testing.T, cert tls.Certificate) *fakeIssuer {
	t.Helper()
	fi := &fakeIssuer{}
	fi.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		fi.hits++
		fi.hosts = append(fi.hosts, r.Host)
		fi.headers = append(fi.headers, r.Header.Clone())
		delay, drop := fi.delay, fi.drop
		fi.mu.Unlock()
		if drop {
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = c.Close()
			}
			return
		}
		time.Sleep(delay)
		fi.mu.Lock()
		fi.servedAt = time.Now()
		fi.mu.Unlock()
		_, _ = io.WriteString(w, `{"issuer":"x"}`)
	}))
	fi.srv.TLS = &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		fi.mu.Lock()
		fi.sni = append(fi.sni, hello.ServerName)
		fi.mu.Unlock()
		return &cert, nil
	}}
	fi.srv.StartTLS()
	t.Cleanup(fi.srv.Close)
	return fi
}

func (fi *fakeIssuer) addr() string { return fi.srv.Listener.Addr().String() }

func (fi *fakeIssuer) count() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.hits
}

// fakeProxyAction is what the fake proxy does with one target.
type fakeProxyAction struct {
	refuse int           // http: the CONNECT status; socks5: the reply code
	phrase string        // http: the refusal's reason phrase and body
	hang   bool          // never answer
	drop   bool          // http: close the connection after the CONNECT request, with no status line
	cut    string        // http: write this start of a status line, then close
	delay  time.Duration // answer after this long
	to     string        // the tunnel's destination; "" is the proxy's backend
}

// fakeProxyConn is one accepted connection: when it was accepted, and when the client side closed it.
type fakeProxyConn struct {
	accepted time.Time
	once     sync.Once
	done     chan struct{}
	closed   time.Time
}

func (c *fakeProxyConn) markClosed() {
	c.once.Do(func() {
		c.closed = time.Now()
		close(c.done)
	})
}

// waitClosed is when the client side closed the connection, or the zero time if it was still open after d.
func (c *fakeProxyConn) waitClosed(d time.Duration) time.Time {
	select {
	case <-c.done:
		return c.closed
	case <-time.After(d):
		return time.Time{}
	}
}

// fakeProxy is an in-process HTTP, https or SOCKS5 proxy that records every connection and target.
type fakeProxy struct {
	t    *testing.T
	kind string // "http", "https" or "socks5"
	cert *tls.Certificate
	ca   *testCA // https: the authority that signed the proxy's certificate
	addr string
	user *url.Userinfo

	mu        sync.Mutex
	ln        net.Listener
	raws      []net.Conn
	conns     []*fakeProxyConn
	targets   []string
	targetAt  []time.Time
	hosts     []string      // the CONNECT requests' Host
	headers   []http.Header // the CONNECT requests' other headers
	stall     bool          // stall at once: no TLS from an https proxy, no greeting reply from a SOCKS5 one
	backend   string
	route     func(target string) fakeProxyAction
	resolveBy map[string]string // the proxy's own resolver, for a target named by host
}

func newFakeProxy(t *testing.T, kind string) *fakeProxy {
	t.Helper()
	fp := &fakeProxy{t: t, kind: kind}
	if kind == "https" {
		fp.ca = newTestCA(t, "proxy CA")
		c := fp.ca.leaf(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
		fp.cert = &c
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fp.addr = ln.Addr().String()
	fp.serveOn(ln)
	t.Cleanup(fp.close)
	return fp
}

func (fp *fakeProxy) serveOn(ln net.Listener) {
	fp.mu.Lock()
	fp.ln = ln
	fp.mu.Unlock()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			pc := &fakeProxyConn{accepted: time.Now(), done: make(chan struct{})}
			fp.mu.Lock()
			fp.raws = append(fp.raws, raw)
			fp.conns = append(fp.conns, pc)
			fp.mu.Unlock()
			go fp.serve(raw, pc)
		}
	}()
}

// stop closes the listener, so a dial to the proxy is refused; start listens again on the same address.
func (fp *fakeProxy) stop() {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	_ = fp.ln.Close()
}

func (fp *fakeProxy) start() {
	fp.t.Helper()
	ln, err := net.Listen("tcp", fp.addr)
	if err != nil {
		fp.t.Fatal(err)
	}
	fp.serveOn(ln)
}

func (fp *fakeProxy) close() {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	_ = fp.ln.Close()
	for _, r := range fp.raws {
		_ = r.Close()
	}
}

func (fp *fakeProxy) url() *url.URL {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return &url.URL{Scheme: fp.kind, Host: fp.addr, User: fp.user}
}

// roots is the pool an https proxy's own certificate verifies under; nil for the others.
func (fp *fakeProxy) roots() *x509.CertPool {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.ca == nil {
		return nil
	}
	return fp.ca.pool()
}

func (fp *fakeProxy) set(fn func(fp *fakeProxy)) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fn(fp)
}

func (fp *fakeProxy) connList() []*fakeProxyConn {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return slices.Clone(fp.conns)
}

func (fp *fakeProxy) connCount() int { return len(fp.connList()) }

func (fp *fakeProxy) targetList() ([]string, []time.Time) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return slices.Clone(fp.targets), slices.Clone(fp.targetAt)
}

func (fp *fakeProxy) action(target string) fakeProxyAction {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.targets = append(fp.targets, target)
	fp.targetAt = append(fp.targetAt, time.Now())
	var a fakeProxyAction
	if fp.route != nil {
		a = fp.route(target)
	}
	if a.to == "" {
		a.to = fp.backend
		if host, _, err := net.SplitHostPort(target); err == nil && fp.resolveBy[host] != "" {
			a.to = fp.resolveBy[host]
		}
	}
	return a
}

// drain reads until the client side closes the connection, and records when it did.
func drain(r io.Reader, pc *fakeProxyConn) {
	_, _ = io.Copy(io.Discard, r)
	pc.markClosed()
}

func (fp *fakeProxy) serve(raw net.Conn, pc *fakeProxyConn) {
	defer raw.Close()
	fp.mu.Lock()
	stall, cert := fp.stall, fp.cert
	fp.mu.Unlock()
	if stall {
		drain(raw, pc)
		return
	}
	var conn net.Conn = raw
	if fp.kind == "https" {
		tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{*cert}})
		if err := tc.Handshake(); err != nil {
			drain(raw, pc)
			return
		}
		conn = tc
	}
	br := bufio.NewReader(conn)
	if fp.kind == "socks5" {
		fp.serveSOCKS(conn, br, pc)
		return
	}
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		drain(br, pc)
		return
	}
	fp.mu.Lock()
	fp.hosts = append(fp.hosts, req.Host)
	fp.headers = append(fp.headers, req.Header.Clone())
	fp.mu.Unlock()
	a := fp.action(req.RequestURI)
	time.Sleep(a.delay)
	switch {
	case a.drop:
		return
	case a.cut != "":
		fmt.Fprint(conn, a.cut)
		return
	case a.hang:
		drain(br, pc)
		return
	case a.refuse != 0:
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s", a.refuse, a.phrase, len(a.phrase), a.phrase)
		drain(br, pc)
		return
	}
	backend, err := net.Dial("tcp", a.to)
	if err != nil {
		fmt.Fprintf(conn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		drain(br, pc)
		return
	}
	defer backend.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	tunnel(conn, br, backend, pc)
}

func tunnel(conn net.Conn, br *bufio.Reader, backend net.Conn, pc *fakeProxyConn) {
	go func() { _, _ = io.Copy(conn, backend) }()
	_, _ = io.Copy(backend, br)
	pc.markClosed()
}

// serveSOCKS is the SOCKS5 side: no authentication, or a user name and password when the proxy has a user.
func (fp *fakeProxy) serveSOCKS(conn net.Conn, br *bufio.Reader, pc *fakeProxyConn) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		drain(br, pc)
		return
	}
	if _, err := io.ReadFull(br, make([]byte, head[1])); err != nil {
		drain(br, pc)
		return
	}
	fp.mu.Lock()
	user := fp.user
	fp.mu.Unlock()
	if user != nil {
		_, _ = conn.Write([]byte{5, 2})
		v := make([]byte, 2)
		_, _ = io.ReadFull(br, v)
		_, _ = io.ReadFull(br, make([]byte, v[1]))
		pl := make([]byte, 1)
		_, _ = io.ReadFull(br, pl)
		_, _ = io.ReadFull(br, make([]byte, pl[0]))
		_, _ = conn.Write([]byte{1, 0})
	} else {
		_, _ = conn.Write([]byte{5, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		drain(br, pc)
		return
	}
	var host string
	switch req[3] {
	case 1, 4:
		b := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
		_, _ = io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		n, _ := br.ReadByte()
		b := make([]byte, n)
		_, _ = io.ReadFull(br, b)
		host = string(b)
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(br, pb)
	a := fp.action(net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))))
	time.Sleep(a.delay)
	switch {
	case a.hang:
		drain(br, pc)
		return
	case a.refuse != 0:
		_, _ = conn.Write([]byte{5, byte(a.refuse), 0, 1, 0, 0, 0, 0, 0, 0})
		drain(br, pc)
		return
	}
	backend, err := net.Dial("tcp", a.to)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		drain(br, pc)
		return
	}
	defer backend.Close()
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	tunnel(conn, br, backend, pc)
}

// stallListener accepts connections and never speaks: a tunnel to it stalls the issuer's handshake.
func stallListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// countingListener counts the connections it accepts and closes each at once.
func countingListener(t *testing.T) (string, func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			mu.Unlock()
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// proxyFetchNet is a fetch layer whose resolver answers ips and whose fetches go through fp.
func proxyFetchNet(fp *fakeProxy, issuerRoots *x509.CertPool, answer func(host string) ([]net.IP, error)) *portalNet {
	return &portalNet{
		lookup: func(_ context.Context, host string) ([]net.IP, error) { return answer(host) },
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("the direct seam is never used through the proxy")
		},
		proxy: &PortalProxy{URL: fp.url(), Listed: func(string) (bool, error) { return true, nil }, ProxyRoots: fp.roots(), IssuerRoots: issuerRoots},
	}
}

func answers(ips ...net.IP) func(string) ([]net.IP, error) {
	return func(string) ([]net.IP, error) { return ips, nil }
}

func proxyTestClient(pn *portalNet, authority string) *http.Client {
	host, _, _ := net.SplitHostPort(authority)
	return newPortalProxyClient(pn, map[string]bool{authority: true}, host)
}

// proxyFetch runs one fetch of the test issuer's discovery document.
func proxyFetch(ctx context.Context, client *http.Client, issuer string) error {
	var doc map[string]any
	return portalGetJSON(ctx, client, issuer+"/.well-known/openid-configuration", &doc)
}

func wantFetchDetail(t *testing.T, err error, cause, detail string) {
	t.Helper()
	var fe *portalFetchError
	if !errors.As(err, &fe) || fe.cause != cause || fe.detail != detail {
		t.Fatalf("fetch error = %v, want %s: %s", err, cause, detail)
	}
	if want := "portal fetch: " + cause + ": " + detail; err.Error() != want {
		t.Fatalf("error text %q, want only %q", err.Error(), want)
	}
}

// issuerCert is the test issuer's certificate under ca.
func issuerCert(t *testing.T, ca *testCA) tls.Certificate {
	return ca.leaf(t, []string{"login.corp.example"}, nil)
}

// proxyTestListed lists the two test issuer hosts.
func proxyTestListed(host string) (bool, error) {
	return host == "login.corp.example" || host == "other.example.org", nil
}

// withProxy routes a harness's listed hosts through fp, trusting the harness's own CA for them.
func withProxy(fp *fakeProxy) func(*PortalIssuersOptions) {
	return func(o *PortalIssuersOptions) {
		portalTestCerts(fp.t)
		pool := x509.NewCertPool()
		pool.AddCert(portalCACert)
		o.Proxy = &PortalProxy{URL: fp.url(), Listed: proxyTestListed, ProxyRoots: fp.roots(), IssuerRoots: pool}
	}
}

// proxyHarness is a harness for https://login.corp.example/oauth2/a whose fetches go through fp to its provider.
func proxyHarness(t *testing.T, fp *fakeProxy, mod ...func(*PortalIssuersOptions)) *portalHarness {
	t.Helper()
	h := newPortalHarness(t, "login.corp.example", false, "/oauth2/a", append([]func(*PortalIssuersOptions){withProxy(fp)}, mod...)...)
	fp.set(func(fp *fakeProxy) { fp.backend = h.idp.srv.Listener.Addr().String() })
	return h
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestPortalProxyFetch_NoProxySuffixStillProxied(t *testing.T) {
	t.Setenv("NO_PROXY", ".corp.example")
	t.Setenv("no_proxy", ".corp.example")
	decoy := newFakeProxy(t, "http")
	t.Setenv("HTTPS_PROXY", decoy.url().String())
	t.Setenv("HTTP_PROXY", decoy.url().String())
	fp := newFakeProxy(t, "http")
	h := proxyHarness(t, fp)
	h.mustAdmit(h.token(h.accessClaims()))
	targets, _ := fp.targetList()
	if !slices.Equal(targets, []string{proxyTestV4At, proxyTestV4At}) {
		t.Fatalf("CONNECT targets %v, want one %s for discovery and one for the keys", targets, proxyTestV4At)
	}
	if d, k := h.idp.hits(); d != 1 || k != 1 {
		t.Fatalf("provider saw (%d,%d) fetches, want (1,1)", d, k)
	}
	if decoy.connCount() != 0 || h.dialCount() != 0 {
		t.Fatalf("decoy saw %d connections and the direct seam %d dials, want 0 and 0", decoy.connCount(), h.dialCount())
	}
}

func TestPortalProxyFetch_ConnectTargetIsCheckedAddress(t *testing.T) {
	ca := newTestCA(t, "issuer CA")

	t.Run("target, host and server name", func(t *testing.T) {
		for _, c := range []struct {
			issuer, authority string
			ip                net.IP
			target            string
		}{
			{proxyTestIssuer, "login.corp.example:443", proxyTestV4, proxyTestV4At},
			{"https://login.corp.example:8443/oauth2/a", "login.corp.example:8443", proxyTestV6, "[2606:4700:4700::1111]:8443"},
		} {
			fi := newFakeIssuer(t, issuerCert(t, ca))
			fp := newFakeProxy(t, "http")
			fp.set(func(fp *fakeProxy) { fp.backend = fi.addr() })
			client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(c.ip)), c.authority)
			if err := proxyFetch(context.Background(), client, c.issuer); err != nil {
				t.Fatalf("%s: %v", c.issuer, err)
			}
			targets, _ := fp.targetList()
			fp.mu.Lock()
			hosts := slices.Clone(fp.hosts)
			fp.mu.Unlock()
			if !slices.Equal(targets, []string{c.target}) || !slices.Equal(hosts, []string{c.target}) {
				t.Fatalf("CONNECT %v with Host %v, want %s for both", targets, hosts, c.target)
			}
			fi.mu.Lock()
			gotHost, gotSNI := slices.Clone(fi.hosts), slices.Clone(fi.sni)
			fi.mu.Unlock()
			issuerURL, _ := url.Parse(c.issuer)
			wantHost := issuerURL.Host
			if !slices.Equal(gotHost, []string{wantHost}) || !slices.Equal(gotSNI, []string{"login.corp.example"}) {
				t.Fatalf("GET Host %v and server name %v, want %s and login.corp.example", gotHost, gotSNI, wantHost)
			}
		}
	})

	// run fetches once through fp with the answer [IPv6, IPv4] under a 10 s bound.
	run := func(t *testing.T, fp *fakeProxy, fi *fakeIssuer) (time.Duration, error) {
		fp.set(func(fp *fakeProxy) { fp.backend = fi.addr() })
		client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV6, proxyTestV4)), "login.corp.example:443")
		start := time.Now()
		err := proxyFetch(context.Background(), client, proxyTestIssuer)
		return time.Since(start), err
	}
	refuseV6 := func(a fakeProxyAction) func(string) fakeProxyAction {
		return func(target string) fakeProxyAction {
			if target == proxyTestV6At {
				return a
			}
			return fakeProxyAction{}
		}
	}

	t.Run("http proxy refuses the IPv6 address", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		fp.set(func(fp *fakeProxy) {
			fp.route = refuseV6(fakeProxyAction{refuse: http.StatusBadGateway, phrase: "Bad Gateway"})
		})
		if _, err := run(t, fp, newFakeIssuer(t, issuerCert(t, ca))); err != nil {
			t.Fatal(err)
		}
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
			t.Fatalf("CONNECT targets %v, want the IPv6 address and then the IPv4 one", targets)
		}
	})

	t.Run("socks5 proxy cannot reach the IPv6 address", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "socks5")
		fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{refuse: 3}) }) // network unreachable
		if _, err := run(t, fp, newFakeIssuer(t, issuerCert(t, ca))); err != nil {
			t.Fatal(err)
		}
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
			t.Fatalf("SOCKS targets %v, want the IPv6 address and then the IPv4 one", targets)
		}
	})

	t.Run("hanging proxy", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{hang: true}) })
		start := time.Now()
		took, err := run(t, fp, newFakeIssuer(t, issuerCert(t, ca)))
		if err != nil {
			t.Fatalf("fetch failed after %v: %v", took, err)
		}
		targets, at := fp.targetList()
		if !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
			t.Fatalf("CONNECT targets %v, want both addresses", targets)
		}
		if d := at[1].Sub(start); d < 4500*time.Millisecond || d > 6500*time.Millisecond || took >= 10*time.Second {
			t.Fatalf("the IPv4 CONNECT started %v in and the fetch took %v, want about 5 s and under 10 s", d, took)
		}
	})

	slowFirst := func(t *testing.T, kind string) {
		fp := newFakeProxy(t, kind)
		fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{delay: 3 * time.Second}) })
		fi := newFakeIssuer(t, issuerCert(t, ca))
		fi.mu.Lock()
		fi.delay = 4 * time.Second
		fi.mu.Unlock()
		took, err := run(t, fp, fi)
		if err != nil {
			t.Fatalf("fetch failed after %v: %v", took, err)
		}
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At}) {
			t.Fatalf("CONNECT targets %v, want the IPv6 address alone", targets)
		}
		conns := fp.connList()
		closed := conns[0].waitClosed(5 * time.Second)
		fi.mu.Lock()
		served := fi.servedAt
		fi.mu.Unlock()
		if closed.IsZero() || closed.Before(served) {
			t.Fatalf("the tunnel closed at %v, the response was served at %v: it must close only after the response", closed, served)
		}
	}
	t.Run("slow but working first address", func(t *testing.T) {
		t.Parallel()
		slowFirst(t, "http")
	})
	t.Run("slow but working first address through an https proxy", func(t *testing.T) {
		t.Parallel()
		slowFirst(t, "https")
	})

	t.Run("stalled CONNECT after a good https proxy handshake", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "https")
		fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{hang: true}) })
		took, err := run(t, fp, newFakeIssuer(t, issuerCert(t, ca)))
		if err != nil {
			t.Fatalf("fetch failed after %v: %v", took, err)
		}
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
			t.Fatalf("CONNECT targets %v, want both addresses", targets)
		}
	})

	t.Run("https proxy whose own handshake fails", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "https")
		other := newTestCA(t, "not the proxy's CA")
		fp.set(func(fp *fakeProxy) { fp.ca = other }) // ProxyRoots then holds a CA that did not sign the proxy
		_, err := run(t, fp, newFakeIssuer(t, issuerCert(t, ca)))
		failed := time.Now()
		wantFetchDetail(t, err, portalCauseUnreachable, portalDetailProxyTLS)
		conns := fp.connList()
		if len(conns) != 1 {
			t.Fatalf("%d proxy connections, want one attempt", len(conns))
		}
		if closed := conns[0].waitClosed(time.Second); closed.IsZero() || closed.Sub(failed) > 100*time.Millisecond {
			t.Fatalf("the proxy socket closed at %v, %v after the failure; want within 100 ms", closed, closed.Sub(failed))
		}
	})

	t.Run("issuer handshake failure is terminal", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		_, err := run(t, fp, newFakeIssuer(t, issuerCert(t, newTestCA(t, "another CA"))))
		wantFetchDetail(t, err, portalCauseUnreachable, portalDetailTLSHandshake)
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At}) {
			t.Fatalf("CONNECT targets %v, want one attempt", targets)
		}
	})

	t.Run("context ends during the first attempt", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{hang: true}) })
		fp.set(func(fp *fakeProxy) { fp.backend = newFakeIssuer(t, issuerCert(t, ca)).addr() })
		client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV6, proxyTestV4)), "login.corp.example:443")
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		time.AfterFunc(time.Second, cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestIssuer+"/.well-known/openid-configuration", nil)
		_, err := client.Do(req)
		if !errors.Is(err, context.Canceled) || time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("Do = %v after %v, want cancellation at once", err, time.Since(start))
		}
		if targets, _ := fp.targetList(); len(targets) != 1 {
			t.Fatalf("CONNECT targets %v, want one attempt", targets)
		}
		// The attempt's deadline is its share, half the 10 s bound.
		conns := fp.connList()
		if closed := conns[0].waitClosed(7 * time.Second); closed.IsZero() || closed.Sub(start) > 5100*time.Millisecond {
			t.Fatalf("the proxy socket closed %v after the start, want by the attempt's 5 s deadline", closed.Sub(start))
		}
	})

	for _, kind := range []string{"http", "https"} {
		t.Run(kind+" proxy drops the CONNECT on the IPv6 address", func(t *testing.T) {
			t.Parallel()
			fp := newFakeProxy(t, kind)
			fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{drop: true}) })
			fi := newFakeIssuer(t, issuerCert(t, ca))
			if took, err := run(t, fp, fi); err != nil {
				t.Fatalf("fetch failed after %v: %v", took, err)
			}
			if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
				t.Fatalf("CONNECT targets %v, want the IPv6 address and then the IPv4 one", targets)
			}
			if fi.count() != 1 {
				t.Fatalf("the issuer served %d requests, want 1", fi.count())
			}
		})
	}

	for _, kind := range []string{"http", "https"} {
		t.Run(kind+" proxy cuts the status line on the IPv6 address", func(t *testing.T) {
			t.Parallel()
			fp := newFakeProxy(t, kind)
			fp.set(func(fp *fakeProxy) { fp.route = refuseV6(fakeProxyAction{cut: "HTTP/1.1 2"}) })
			fi := newFakeIssuer(t, issuerCert(t, ca))
			if took, err := run(t, fp, fi); err != nil {
				t.Fatalf("fetch failed after %v: %v", took, err)
			}
			if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At, proxyTestV4At}) {
				t.Fatalf("CONNECT targets %v, want the IPv6 address and then the IPv4 one", targets)
			}
			if fi.count() != 1 {
				t.Fatalf("the issuer served %d requests, want 1", fi.count())
			}
		})
	}

	t.Run("http proxy drops the CONNECT on the only address", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		fp.set(func(fp *fakeProxy) { fp.route = func(string) fakeProxyAction { return fakeProxyAction{drop: true} } })
		fp.set(func(fp *fakeProxy) { fp.backend = newFakeIssuer(t, issuerCert(t, ca)).addr() })
		client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV4)), "login.corp.example:443")
		wantFetchDetail(t, proxyFetch(context.Background(), client, proxyTestIssuer), portalCauseUnreachable, portalDetailProxyTarget)
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV4At}) {
			t.Fatalf("CONNECT targets %v, want the one address", targets)
		}
	})

	t.Run("issuer completes TLS and then closes is terminal", func(t *testing.T) {
		t.Parallel()
		fp := newFakeProxy(t, "http")
		fi := newFakeIssuer(t, issuerCert(t, ca))
		fi.mu.Lock()
		fi.drop = true
		fi.mu.Unlock()
		_, err := run(t, fp, fi)
		wantFetchDetail(t, err, portalCauseUnreachable, "request failed")
		if targets, _ := fp.targetList(); !slices.Equal(targets, []string{proxyTestV6At}) {
			t.Fatalf("CONNECT targets %v, want one attempt", targets)
		}
	})

	t.Run("documentation address refused with no contact", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(net.ParseIP("2001:db8::1"), proxyTestV4)), "login.corp.example:443")
		wantFetchDetail(t, proxyFetch(context.Background(), client, proxyTestIssuer), portalCauseAddressRefused, "address refused")
		if fp.connCount() != 0 {
			t.Fatalf("%d proxy connections, want 0", fp.connCount())
		}
	})
}

func TestPortalProxyFetch_AddressRuleRefusedBeforeConnect(t *testing.T) {
	ca := newTestCA(t, "issuer CA")
	for name, ips := range map[string][]net.IP{
		"private":        {net.ParseIP("10.0.0.1")},
		"loopback":       {net.ParseIP("127.0.0.1")},
		"metadata":       {net.ParseIP("169.254.169.254")},
		"mapped private": {net.ParseIP("::ffff:10.0.0.1")},
		"v4-compatible":  {net.ParseIP("::10.0.0.1")},
		"6to4 public":    {net.ParseIP("2002:808:808::1")},
		"mixed":          {proxyTestV4, net.ParseIP("10.0.0.1")},
	} {
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, "http")
			client := proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(ips...)), "login.corp.example:443")
			wantFetchDetail(t, proxyFetch(context.Background(), client, proxyTestIssuer), portalCauseAddressRefused, "address refused")
			if fp.connCount() != 0 {
				t.Fatalf("%d proxy connections, want 0", fp.connCount())
			}
		})
	}

	t.Run("second fetch", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		fp.set(func(fp *fakeProxy) { fp.backend = newFakeIssuer(t, issuerCert(t, ca)).addr() })
		var mu sync.Mutex
		lookups, answer := 0, []net.IP{proxyTestV4}
		pn := proxyFetchNet(fp, ca.pool(), func(string) ([]net.IP, error) {
			mu.Lock()
			defer mu.Unlock()
			lookups++
			return answer, nil
		})
		client := proxyTestClient(pn, "login.corp.example:443")
		for i := 1; i <= 2; i++ {
			if err := proxyFetch(context.Background(), client, proxyTestIssuer); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			n := lookups
			mu.Unlock()
			if fp.connCount() != i || n != i {
				t.Fatalf("after %d fetches: %d tunnels and %d lookups, want a new tunnel and a new lookup each", i, fp.connCount(), n)
			}
		}
		mu.Lock()
		answer = []net.IP{net.ParseIP("10.0.0.1")}
		mu.Unlock()
		wantFetchDetail(t, proxyFetch(context.Background(), client, proxyTestIssuer), portalCauseAddressRefused, "address refused")
		if fp.connCount() != 2 {
			t.Fatalf("%d proxy connections, want no new one for a private answer", fp.connCount())
		}
	})
}

func TestPortalProxyFetch_SplitDNSNeverReachesProxyAnswer(t *testing.T) {
	forbidden, accepted := countingListener(t)
	fp := newFakeProxy(t, "http")
	fp.set(func(fp *fakeProxy) { fp.resolveBy = map[string]string{"login.corp.example": forbidden} })
	h := proxyHarness(t, fp)
	h.mustAdmit(h.token(h.accessClaims()))
	if accepted() != 0 {
		t.Fatalf("the forbidden listener accepted %d connections", accepted())
	}
	targets, _ := fp.targetList()
	for _, target := range targets {
		if strings.Contains(target, "login.corp.example") {
			t.Fatalf("a CONNECT named the host: %v", targets)
		}
	}

	t.Run("mixed answer", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := proxyHarness(t, fp)
		h.setLookup(func(string) []net.IP { return []net.IP{proxyTestV4, net.ParseIP("10.0.0.1")} })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
		if fp.connCount() != 0 {
			t.Fatalf("%d proxy connections, want 0", fp.connCount())
		}
	})
}

func TestPortalProxyFetch_UnresolvedNameIsOutageWithoutConnect(t *testing.T) {
	for name, lookup := range map[string]func(context.Context, string) ([]net.IP, error){
		"lookup error": func(context.Context, string) ([]net.IP, error) { return nil, errors.New("no such host") },
		"empty answer": func(context.Context, string) ([]net.IP, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, "http")
			h := proxyHarness(t, fp, func(o *PortalIssuersOptions) { o.lookup = lookup })
			r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
			if r.RetryAfter <= 0 || r.RetryAfter > portalKeyCooldown {
				t.Fatalf("Retry-After %v, want 0 < it <= 30 s", r.RetryAfter)
			}
			if fp.connCount() != 0 {
				t.Fatalf("%d proxy connections, want 0", fp.connCount())
			}
		})
	}
}

func TestPortalProxyFetch_RedirectRefused(t *testing.T) {
	for name, to := range map[string]string{"same host": proxyTestIssuer + "/moved", "another listed host": "https://other.example.org/x"} {
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, "http")
			h := proxyHarness(t, fp)
			h.idp.set(func(f *fakeIdP) {
				f.override = func(w http.ResponseWriter, r *http.Request) bool {
					http.Redirect(w, r, to, http.StatusFound)
					return true
				}
			})
			h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
			h.idp.mu.Lock()
			n := len(h.idp.requests)
			h.idp.mu.Unlock()
			if targets, _ := fp.targetList(); n != 1 || len(targets) != 1 {
				t.Fatalf("%d requests and CONNECTs %v, want one of each", n, targets)
			}
		})
	}
}

func TestPortalProxyFetch_KeysHostIsolation(t *testing.T) {
	fp := newFakeProxy(t, "http")
	var mu sync.Mutex
	var looked []string
	h := proxyHarness(t, fp)
	h.setLookup(func(host string) []net.IP {
		mu.Lock()
		looked = append(looked, host)
		mu.Unlock()
		return []net.IP{proxyTestV4}
	})
	h.idp.set(func(f *fakeIdP) { f.jwksURI = "https://other.example.org/keys" })
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseKeysOffOrigin)
	targets, _ := fp.targetList()
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 1 || slices.Contains(looked, "other.example.org") {
		t.Fatalf("CONNECTs %v and lookups %v, want only the discovery fetch", targets, looked)
	}
}

func TestPortalProxyFetch_ProxyDownColdVerifier(t *testing.T) {
	fp := newFakeProxy(t, "http")
	h := proxyHarness(t, fp)
	fp.stop()
	if r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable); r.RetryAfter != portalKeyCooldown {
		t.Fatalf("Retry-After %v, want the full 30 s", r.RetryAfter)
	}
	h.clock.Advance(10 * time.Second)
	if r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable); r.RetryAfter != 20*time.Second {
		t.Fatalf("Retry-After %v, want the 20 s left", r.RetryAfter)
	}
	h.clock.Advance(20 * time.Second)
	fp.start()
	h.mustAdmit(h.token(h.accessClaims()))
}

func TestPortalProxyFetch_ProxyDownWarmVerifier(t *testing.T) {
	fp := newFakeProxy(t, "http")
	h := proxyHarness(t, fp)
	h.mustAdmit(h.token(h.accessClaims()))
	before, _ := h.pi.verifierFor(h.reg)
	fp.stop()
	n := fp.connCount()

	h.clock.Advance(40 * time.Minute)
	h.mustAdmit(h.token(h.accessClaims()))
	if fp.connCount() != n {
		t.Fatal("a warm verifier contacted the proxy for a key it holds")
	}

	h.clock.Advance(21 * time.Minute)
	h.pi.mu.Lock()
	cached := h.pi.verifiers[[2]string{h.reg.Issuer, ""}]
	h.pi.mu.Unlock()
	if cached != before {
		t.Fatal("the verifier was dropped although it was used 21 min ago")
	}
	if r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable); r.RetryAfter != portalKeyCooldown {
		t.Fatalf("Retry-After %v, want the full 30 s", r.RetryAfter)
	}
	h.clock.Advance(10 * time.Second)
	if r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown); r.RetryAfter != 20*time.Second {
		t.Fatalf("Retry-After %v, want the 20 s left", r.RetryAfter)
	}
	h.clock.Advance(21 * time.Second)
	fp.start()
	h.mustAdmit(h.token(h.accessClaims()))

	t.Run("a new key first", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := proxyHarness(t, fp)
		h.mustAdmit(h.token(h.accessClaims()))
		fp.stop()
		h.clock.Advance(20 * time.Minute)
		first := h.wantUnavailable(h.kidToken("k2", 1), portalCauseKeysUnreachable)
		h.clock.Advance(10 * time.Second)
		second := h.wantUnavailable(h.token(h.accessClaims()), portalCauseIssuerDown)
		if first.RetryAfter != 0 || second.RetryAfter != 20*time.Second {
			t.Fatalf("Retry-After %v then %v, want none and then 20 s", first.RetryAfter, second.RetryAfter)
		}
		h.clock.Advance(21 * time.Second)
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
	})

	t.Run("a new key within the cooldown", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := proxyHarness(t, fp)
		h.mustAdmit(h.token(h.accessClaims()))
		n := fp.connCount()
		h.clock.Advance(10 * time.Second)
		if r := h.wantUnavailable(h.kidToken("k2", 1), portalCauseKeysCooldown); r.RetryAfter != 20*time.Second {
			t.Fatalf("Retry-After %v, want the 20 s left", r.RetryAfter)
		}
		h.mustAdmit(h.token(h.accessClaims()))
		if fp.connCount() != n {
			t.Fatal("a new key inside the cooldown contacted the proxy")
		}
	})

	t.Run("two portals, one pair", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := proxyHarness(t, fp)
		a, b := h.reg, h.reg
		b.ClientID = "client-2"
		claimsB := func() map[string]any {
			c := h.accessClaims()
			c["cid"] = "client-2"
			return c
		}
		admit := func(reg PortalRegistration, raw string) {
			t.Helper()
			if _, r := h.verifyReg(reg, raw); !r.OK() {
				t.Fatalf("refused: %+v", r)
			}
		}
		admit(a, h.token(h.accessClaims()))
		shared, _ := h.pi.verifierFor(a)
		n := fp.connCount()
		h.clock.Advance(50 * time.Minute)
		admit(b, h.mint("k1", 0, claimsB(), nil))
		if fp.connCount() != n {
			t.Fatal("the second portal fetched again: the verifier is not shared")
		}
		h.clock.Advance(55 * time.Minute) // 105 min after a's last use, 55 after b's
		admit(a, h.token(h.accessClaims()))
		if v, _ := h.pi.verifierFor(a); v != shared {
			t.Fatal("b's use did not keep the shared verifier warm for a")
		}
		h.clock.Advance(time.Minute)
		fp.stop()
		h.wantUnavailable(h.kidToken("k2", 1), portalCauseKeysUnreachable)
		if _, r := h.verifyReg(b, h.mint("k1", 0, claimsB(), nil)); r.Cause != portalCauseIssuerDown || r.RetryAfter != portalKeyCooldown {
			t.Fatalf("b answered %+v, want issuer_unreachable for the cooldown", r)
		}
	})
}

func TestPortalProxyFetch_ProxyRefusalIsOutage(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusProxyAuthRequired, http.StatusBadGateway} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			logs := captureLogs(t)
			fp := newFakeProxy(t, "http")
			fp.set(func(fp *fakeProxy) {
				fp.route = func(string) fakeProxyAction { return fakeProxyAction{refuse: status, phrase: "MARKER-proxy-phrase"} }
			})
			h := proxyHarness(t, fp)
			r := h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
			if r.RetryAfter <= 0 {
				t.Fatal("a proxy refusal carries no Retry-After")
			}
			if strings.Contains(fmt.Sprintf("%+v", r), "MARKER") || strings.Contains(logs.String(), "MARKER") {
				t.Fatalf("the proxy's phrase leaked: %+v / %s", r, logs.String())
			}
			if want := fmt.Sprintf("detail=\"proxy status %dxx\"", status/100); !strings.Contains(logs.String(), want) {
				t.Fatalf("log %q lacks %s", logs.String(), want)
			}
		})
	}
}

// waitGoroutines waits for the goroutine count to fall back to n.
func waitGoroutines(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > n {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, want %d", runtime.NumGoroutine(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPortalProxyFetch_SetupSocketBoundedAndClosed(t *testing.T) {
	ca := newTestCA(t, "issuer CA")
	setups := map[string]func(t *testing.T) *fakeProxy{
		"http CONNECT never answered": func(t *testing.T) *fakeProxy {
			fp := newFakeProxy(t, "http")
			fp.set(func(fp *fakeProxy) { fp.route = func(string) fakeProxyAction { return fakeProxyAction{hang: true} } })
			return fp
		},
		"https proxy stalls its handshake": func(t *testing.T) *fakeProxy {
			fp := newFakeProxy(t, "https")
			fp.set(func(fp *fakeProxy) { fp.stall = true })
			return fp
		},
		"socks5 proxy stalls negotiation": func(t *testing.T) *fakeProxy {
			fp := newFakeProxy(t, "socks5")
			fp.set(func(fp *fakeProxy) { fp.stall = true })
			return fp
		},
		"issuer stalls its handshake": func(t *testing.T) *fakeProxy {
			fp := newFakeProxy(t, "http")
			backend := stallListener(t)
			fp.set(func(fp *fakeProxy) { fp.backend = backend })
			return fp
		},
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			fp := setup(t)
			start := runtime.NumGoroutine()
			pn := proxyFetchNet(fp, ca.pool(), answers(proxyTestV4))
			pn.fetchTimeout = time.Second
			began := time.Now()
			err := proxyFetch(context.Background(), proxyTestClient(pn, "login.corp.example:443"), proxyTestIssuer)
			if err == nil || portalFetchCause(err) != portalCauseUnreachable || time.Since(began) > 2*time.Second {
				t.Fatalf("fetch = %v after %v, want an outage within 2 s", err, time.Since(began))
			}
			conns := fp.connList()
			if len(conns) != 1 {
				t.Fatalf("%d proxy connections, want 1", len(conns))
			}
			if closed := conns[0].waitClosed(3 * time.Second); closed.IsZero() || closed.Sub(conns[0].accepted) > 2*time.Second {
				t.Fatalf("the proxy socket closed %v after accept, want within 2 s", closed.Sub(conns[0].accepted))
			}
			fp.close()
			waitGoroutines(t, start)
		})
	}

	twoStalled := map[string]func(t *testing.T) *fakeProxy{
		"http": setups["http CONNECT never answered"], "https": setups["https proxy stalls its handshake"],
		"socks5": setups["socks5 proxy stalls negotiation"],
	}
	for name, setup := range twoStalled {
		t.Run("two stalled addresses through "+name, func(t *testing.T) {
			fp := setup(t)
			pn := proxyFetchNet(fp, ca.pool(), answers(proxyTestV4, proxyTestV4b))
			pn.fetchTimeout = time.Second
			began := time.Now()
			err := proxyFetch(context.Background(), proxyTestClient(pn, "login.corp.example:443"), proxyTestIssuer)
			returned := time.Now()
			if err == nil {
				t.Fatal("a fetch through a stalled proxy succeeded")
			}
			conns := fp.connList()
			if len(conns) != 2 {
				t.Fatalf("%d proxy connections, want one per address", len(conns))
			}
			first, second := conns[0].waitClosed(3*time.Second), conns[1].waitClosed(3*time.Second)
			if first.IsZero() || second.IsZero() {
				t.Fatal("a proxy socket stayed open")
			}
			if d := first.Sub(began); d < 450*time.Millisecond || d > 600*time.Millisecond || first.After(conns[1].accepted.Add(100*time.Millisecond)) {
				t.Fatalf("the first socket closed %v in (second accepted %v in), want within 100 ms after 0.5 s",
					d, conns[1].accepted.Sub(began))
			}
			if d := second.Sub(began); d < 950*time.Millisecond || d > 1100*time.Millisecond {
				t.Fatalf("the second socket closed %v in, want within 100 ms after 1 s", d)
			}
			if second.After(returned.Add(100 * time.Millisecond)) {
				t.Fatalf("a socket was open %v after the fetch returned", second.Sub(returned))
			}
		})
	}
}

func TestPortalProxyFetch_TLSVerifiedThroughTunnel(t *testing.T) {
	ca := newTestCA(t, "issuer CA")
	fetchVia := func(t *testing.T, fp *fakeProxy, backend string) error {
		fp.set(func(fp *fakeProxy) { fp.backend = backend })
		return proxyFetch(context.Background(), proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV4)), "login.corp.example:443"), proxyTestIssuer)
	}
	for name, cert := range map[string]tls.Certificate{
		"another name":            ca.leaf(t, []string{"other.example.org"}, nil),
		"a CA not in the roots":   issuerCert(t, newTestCA(t, "another CA")),
		"a proxy terminating TLS": issuerCert(t, newTestCA(t, "inspection CA")),
	} {
		t.Run(name, func(t *testing.T) {
			fi := newFakeIssuer(t, cert)
			wantFetchDetail(t, fetchVia(t, newFakeProxy(t, "http"), fi.addr()), portalCauseUnreachable, portalDetailTLSHandshake)
			if fi.count() != 0 {
				t.Fatal("a request reached an issuer that failed the check")
			}
		})
	}
	t.Run("the right certificate", func(t *testing.T) {
		if err := fetchVia(t, newFakeProxy(t, "http"), newFakeIssuer(t, issuerCert(t, ca)).addr()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("distinct details", func(t *testing.T) {
		good := newFakeIssuer(t, issuerCert(t, ca)).addr()
		hop := newFakeProxy(t, "https")
		hop.set(func(fp *fakeProxy) { fp.ca = newTestCA(t, "not the proxy's CA") })
		refused := newFakeProxy(t, "http")
		refused.set(func(fp *fakeProxy) {
			fp.route = func(string) fakeProxyAction {
				return fakeProxyAction{refuse: http.StatusBadGateway, phrase: "Bad Gateway"}
			}
		})
		bad := newFakeIssuer(t, issuerCert(t, newTestCA(t, "another CA"))).addr()
		details := map[string]bool{}
		for want, err := range map[string]error{
			portalDetailProxyTLS:     fetchVia(t, hop, good),
			"proxy status 5xx":       fetchVia(t, refused, good),
			portalDetailTLSHandshake: fetchVia(t, newFakeProxy(t, "http"), bad),
		} {
			wantFetchDetail(t, err, portalCauseUnreachable, want)
			details[want] = true
		}
		if len(details) != 3 {
			t.Fatalf("details %v, want three distinct ones", details)
		}
	})

	t.Run("a late callback", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		stalled := stallListener(t)
		closes := closingIssuer(t, issuerCert(t, ca))
		fp.set(func(fp *fakeProxy) { fp.backend = stalled })
		pn := proxyFetchNet(fp, ca.pool(), answers(proxyTestV4))
		pn.fetchTimeout = time.Second
		client := proxyTestClient(pn, "login.corp.example:443")
		pp := client.Transport.(*portalProxyTransport)
		returned, held := make(chan struct{}), make(chan struct{})
		var once sync.Once
		pp.tlsDoneHook = func() {
			once.Do(func() {
				<-returned // the issuer's handshake ends only after the fetch has returned on its cancelled context
				close(held)
			})
		}
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(200*time.Millisecond, cancel)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestIssuer+"/.well-known/openid-configuration", nil)
		if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
			t.Fatalf("Do = %v, want cancellation", err)
		}
		close(returned)
		<-held
		// A second attempt, whose issuer completes the handshake and then closes, is untouched by the first one's
		// late handshake failure.
		fp.set(func(fp *fakeProxy) { fp.backend = closes })
		wantFetchDetail(t, proxyFetch(context.Background(), client, proxyTestIssuer), portalCauseUnreachable, "request failed")
	})
}

// closingIssuer completes the TLS handshake and then closes the connection without an answer.
func closingIssuer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestPortalProxyFetch_InspectionRootInSystemTrustRefused(t *testing.T) {
	if os.Getenv("WARDYN_PORTAL_PROXY_CHILD_CA") != "" {
		inspectionChild(t)
		return
	}
	insp := newTestCA(t, "inspection CA")
	bundle := t.TempDir() + "/inspection.pem"
	if err := os.WriteFile(bundle, insp.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(insp.key)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPortalProxyFetch_InspectionRootInSystemTrustRefused$", "-test.count=1", "-test.v")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSL_CERT_DIR=") && !strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "SSL_CERT_FILE="+bundle, "WARDYN_PORTAL_PROXY_CHILD_CA="+string(insp.pem),
		"WARDYN_PORTAL_PROXY_CHILD_KEY="+string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	for _, sub := range []string{"refused", "control"} {
		if !strings.Contains(string(out), "--- PASS: TestPortalProxyFetch_InspectionRootInSystemTrustRefused/"+sub) {
			t.Fatalf("the child did not run %s:\n%s", sub, out)
		}
	}
}

// inspectionChild runs with SSL_CERT_FILE naming the inspection CA, so the system roots hold it.
func inspectionChild(t *testing.T) {
	block, _ := pem.Decode([]byte(os.Getenv("WARDYN_PORTAL_PROXY_CHILD_CA")))
	keyBlock, _ := pem.Decode([]byte(os.Getenv("WARDYN_PORTAL_PROXY_CHILD_KEY")))
	if block == nil || keyBlock == nil {
		t.Fatal("the child's CA did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	insp := &testCA{cert: cert, key: key, pem: []byte(os.Getenv("WARDYN_PORTAL_PROXY_CHILD_CA"))}
	sys, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	resigned := issuerCert(t, insp)
	leaf, _ := x509.ParseCertificate(resigned.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "login.corp.example", Roots: sys}); err != nil {
		t.Fatalf("the system roots do not hold the inspection CA: %v", err)
	}
	public := newTestCA(t, "public CA")
	for name, c := range map[string]struct {
		roots *x509.CertPool
		ok    bool
	}{"refused": {public.pool(), false}, "control": {insp.pool(), true}} {
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, "https")
			proxyLeaf := insp.leaf(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
			fp.set(func(fp *fakeProxy) { fp.cert, fp.ca = &proxyLeaf, insp }) // ProxyRoots holds the inspection CA too
			fi := newFakeIssuer(t, resigned)                                  // the proxy re-signs the issuer host
			fp.set(func(fp *fakeProxy) { fp.backend = fi.addr() })
			err := proxyFetch(context.Background(), proxyTestClient(proxyFetchNet(fp, c.roots, answers(proxyTestV4)), "login.corp.example:443"), proxyTestIssuer)
			if c.ok {
				if err != nil || fi.count() != 1 {
					t.Fatalf("control: %v with %d documents", err, fi.count())
				}
				return
			}
			wantFetchDetail(t, err, portalCauseUnreachable, portalDetailTLSHandshake)
			if fi.count() != 0 {
				t.Fatalf("%d discovery documents accepted under the inspection CA", fi.count())
			}
		})
	}
}

func TestPortalProxyFetch_NoIssuerRootsRefused(t *testing.T) {
	noRoots := func(o *PortalIssuersOptions) { o.Proxy.IssuerRoots = nil }
	for name, c := range map[string]struct {
		mod    func(*PortalIssuersOptions)
		detail string
	}{
		"no roots": {noRoots, portalDetailNoIssuerRoots},
		"listed error": {func(o *PortalIssuersOptions) {
			o.Proxy.Listed = func(string) (bool, error) { return false, errors.New("x") }
		}, portalDetailNotComparable},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			fp := newFakeProxy(t, "http")
			var h *portalHarness
			var mu sync.Mutex
			var hooks []PortalRefreshFailure
			hook := func(o *PortalIssuersOptions) {
				o.OnRefreshFailure = func(f PortalRefreshFailure) {
					h.pi.mu.Lock() // the hook runs with the verifiers' mutex released
					h.pi.mu.Unlock()
					mu.Lock()
					hooks = append(hooks, f)
					mu.Unlock()
				}
			}
			h = proxyHarness(t, fp, c.mod, hook)
			exchange := func() {
				t.Helper()
				done := make(chan PortalRefusal, 1)
				go func() {
					_, r := h.verify(h.token(h.accessClaims()))
					done <- r
				}()
				select {
				case r := <-done:
					if r.Cause != portalCauseAddressRefused {
						t.Fatalf("refusal %+v, want address_refused", r)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the exchange deadlocked")
				}
			}
			count := func() (warnings, calls int) {
				mu.Lock()
				defer mu.Unlock()
				for _, f := range hooks {
					if f.Source != "verifier" {
						t.Fatalf("hook source %q, want verifier", f.Source)
					}
				}
				want := "source=verifier cause=address_refused route=proxy detail=\"" + c.detail + "\""
				return strings.Count(logs.String(), want), len(hooks)
			}
			exchange()
			h.clock.Advance(10 * time.Second)
			exchange()
			if w, n := count(); w != 1 || n != 1 {
				t.Fatalf("%d warnings and %d hook calls within 30 s, want 1 and 1", w, n)
			}
			h.clock.Advance(30 * time.Second)
			exchange()
			if w, n := count(); w != 2 || n != 2 {
				t.Fatalf("%d warnings and %d hook calls after 30 s, want 2 and 2", w, n)
			}
			if fp.connCount() != 0 {
				t.Fatalf("%d proxy connections, want 0", fp.connCount())
			}
		})
	}

	t.Run("an unlisted host is still direct", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := newPortalHarness(t, "example.com", true, "/oauth2/a", withProxy(fp), noRoots)
		h.mustAdmit(h.token(h.accessClaims()))
		if fp.connCount() != 0 || h.dialCount() == 0 {
			t.Fatalf("%d proxy connections and %d direct dials, want 0 and some", fp.connCount(), h.dialCount())
		}
	})
}

func TestPortalProxyFetch_HTTPSProxyOwnRootsOnly(t *testing.T) {
	ca := newTestCA(t, "issuer CA")
	fp := newFakeProxy(t, "https")
	fetch := func(cert tls.Certificate) (*fakeIssuer, error) {
		fi := newFakeIssuer(t, cert)
		fp.set(func(fp *fakeProxy) { fp.backend = fi.addr() })
		return fi, proxyFetch(context.Background(), proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV4)), "login.corp.example:443"), proxyTestIssuer)
	}
	if _, err := fetch(issuerCert(t, ca)); err != nil {
		t.Fatalf("the proxy hop under the proxy's CA: %v", err)
	}
	fi, err := fetch(issuerCert(t, fp.ca)) // the proxy's CA vouching for the issuer host
	wantFetchDetail(t, err, portalCauseUnreachable, portalDetailTLSHandshake)
	if fi.count() != 0 {
		t.Fatal("the proxy's CA was trusted for the issuer")
	}
}

func TestPortalProxyFetch_CredentialOnConnectOnly(t *testing.T) {
	ca := newTestCA(t, "issuer CA")
	for name, user := range map[string]*url.Userinfo{"with a credential": url.UserPassword("svc", "s3cret"), "without": nil} {
		t.Run(name, func(t *testing.T) {
			fp := newFakeProxy(t, "http")
			fp.set(func(fp *fakeProxy) { fp.user = user })
			fi := newFakeIssuer(t, issuerCert(t, ca))
			fp.set(func(fp *fakeProxy) { fp.backend = fi.addr() })
			if err := proxyFetch(context.Background(), proxyTestClient(proxyFetchNet(fp, ca.pool(), answers(proxyTestV4)), "login.corp.example:443"), proxyTestIssuer); err != nil {
				t.Fatal(err)
			}
			fp.mu.Lock()
			hdr, host := fp.headers[0], fp.hosts[0]
			fp.mu.Unlock()
			want := []string{"User-Agent"}
			if user != nil {
				want = append(want, "Proxy-Authorization")
				if got := hdr.Get("Proxy-Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("svc:s3cret")) {
					t.Fatalf("Proxy-Authorization %q", got)
				}
			}
			var keys []string
			for k := range hdr {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			slices.Sort(want)
			if !slices.Equal(keys, want) || host != proxyTestV4At || hdr.Get("User-Agent") != "Go-http-client/1.1" {
				t.Fatalf("CONNECT headers %v with Host %q, want %v and %s", hdr, host, want, proxyTestV4At)
			}
			fi.mu.Lock()
			defer fi.mu.Unlock()
			if fi.headers[0].Get("Proxy-Authorization") != "" {
				t.Fatal("the request inside the tunnel carried Proxy-Authorization")
			}
		})
	}
}

func TestPortalProxyFetch_UnlistedAndInternalStayDirect(t *testing.T) {
	everyHost := func(o *PortalIssuersOptions) { o.Proxy.Listed = func(string) (bool, error) { return true, nil } }
	wantDirect := func(t *testing.T, h *portalHarness) {
		t.Helper()
		if v, err := h.pi.verifierFor(h.reg); err != nil || v.route != portalRouteDirect {
			t.Fatalf("verifier %v, %v; want the direct route", v, err)
		}
	}
	t.Run("unlisted", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := newPortalHarness(t, "example.com", true, "/oauth2/a", withProxy(fp))
		h.mustAdmit(h.token(h.accessClaims()))
		if fp.connCount() != 0 || h.dialCount() == 0 {
			t.Fatalf("%d proxy connections and %d direct dials", fp.connCount(), h.dialCount())
		}
		wantDirect(t, h)
	})
	t.Run("internal issuer on the internal list", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := internalHarness(t, "/oauth2/a", "/oauth2/a", withProxy(fp), everyHost)
		h.mustAdmit(h.token(h.accessClaims()))
		if fp.connCount() != 0 || h.dialCount() == 0 {
			t.Fatalf("%d proxy connections and %d direct dials", fp.connCount(), h.dialCount())
		}
		wantDirect(t, h)
	})
	t.Run("internal issuer not on the internal list", func(t *testing.T) {
		fp := newFakeProxy(t, "http")
		h := internalHarness(t, "/oauth2/a", "/oauth2/a", withProxy(fp), everyHost, func(o *PortalIssuersOptions) { o.InternalHosts = nil })
		h.wantUnavailable(h.token(h.accessClaims()), portalCauseAddressRefused)
		if fp.connCount() != 0 || h.dialCount() != 0 || h.lookups != 0 {
			t.Fatalf("%d proxy connections, %d dials and %d lookups, want none", fp.connCount(), h.dialCount(), h.lookups)
		}
		wantDirect(t, h)
	})
}

func TestPortalProxyFetch_FailureLogNamesRouteNeverProxy(t *testing.T) {
	logs := captureLogs(t)
	fp := newFakeProxy(t, "http")
	fp.set(func(fp *fakeProxy) { fp.user = url.UserPassword("svc", "MARKER-secret") })
	h := proxyHarness(t, fp)
	fp.stop()
	h.wantUnavailable(h.token(h.accessClaims()), portalCauseUnreachable)
	out := logs.String()
	if !strings.Contains(out, `route=proxy detail="proxy connect failed"`) {
		t.Fatalf("log %q lacks the route and detail", out)
	}
	for _, leak := range []string{"MARKER-secret", "svc", fp.addr} {
		if strings.Contains(out, leak) {
			t.Fatalf("log names the proxy (%q): %s", leak, out)
		}
	}

	d := newPortalHarness(t, "example.com", true, "/oauth2/a", withProxy(newFakeProxy(t, "http")))
	d.idp.set(func(f *fakeIdP) { f.discStatus = http.StatusInternalServerError })
	d.wantUnavailable(d.token(d.accessClaims()), portalCauseUnreachable)
	if !strings.Contains(logs.String(), `route=direct detail="status 5xx"`) {
		t.Fatalf("log %q lacks the direct route", logs.String())
	}
}

// An internationalised spelling of the proxy's name is dialled, and its certificate checked, in its IDNA-ASCII
// form, as the ASCII spelling is; the dial guard still refuses every address but the proxy's own.
func TestPortalProxyFetch_InternationalProxyHostReachesProxy(t *testing.T) {
	spellings := map[string]string{"ascii": "localhost", "unicode": "\uff4c\uff4f\uff43\uff41\uff4c\uff48\uff4f\uff53\uff54"}
	for _, kind := range []string{"http", "https", "socks5"} {
		for name, host := range spellings {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fp := newFakeProxy(t, kind)
				port := fp.url().Port()
				h := proxyHarness(t, fp, func(o *PortalIssuersOptions) {
					u := *o.Proxy.URL
					u.Host = net.JoinHostPort(host, port)
					o.Proxy.URL = &u
				})
				h.mustAdmit(h.token(h.accessClaims()))
				if fp.connCount() == 0 || h.dialCount() != 0 {
					t.Fatalf("proxy saw %d connections and the direct seam %d dials, want some and 0", fp.connCount(), h.dialCount())
				}
			})
		}
	}
	t.Run("guard", func(t *testing.T) {
		fp := newFakeProxy(t, "https")
		port := fp.url().Port()
		reserved := func(string) bool { return false }
		pp := &PortalProxy{URL: &url.URL{Scheme: "https", Host: net.JoinHostPort(spellings["unicode"], port), User: url.UserPassword("u", "p")},
			Listed: proxyTestListed, ProxyRoots: fp.roots(), IssuerRoots: x509.NewCertPool()}
		p, err := NewPortalIssuers(PortalIssuersOptions{Reserved: reserved, Proxy: pp})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.net.proxy.URL; got.Host != "localhost:"+port || got.User.String() != "u:p" || got.Scheme != "https" {
			t.Fatalf("canonical proxy URL %s, want https://u:p@localhost:%s", got.Redacted(), port)
		}
		if pp.URL.Hostname() != spellings["unicode"] {
			t.Fatalf("the caller's route was changed: %s", pp.URL.Hostname())
		}
		for _, lit := range []string{"127.0.0.1:3128", "[::1]:3128", "proxy.corp.example"} {
			in := &PortalProxy{URL: &url.URL{Scheme: "http", Host: lit}}
			if out := portalProxyCanonical(in); out != in {
				t.Fatalf("%s: rewritten to %s, want it kept", lit, out.URL.Host)
			}
		}
		tr := newPortalProxyClient(p.net, map[string]bool{"login.corp.example": true}, "login.corp.example").Transport.(*portalProxyTransport)
		attempt := func() context.Context {
			return context.WithValue(context.Background(), portalProxyAttemptKey{}, &portalProxyAttempt{deadline: time.Now().Add(5 * time.Second)})
		}
		for _, addr := range []string{"127.0.0.1:" + port, "localhost:1", "evil.example:" + port, net.JoinHostPort(spellings["unicode"], port)} {
			conn, err := tr.dialProxyTLS(attempt(), "tcp", addr)
			if conn != nil {
				_ = conn.Close()
			}
			wantFetchDetail(t, err, portalCauseAddressRefused, portalDetailNotTheProxy)
		}
		before := fp.connCount()
		conn, err := tr.dialProxyTLS(attempt(), "tcp", "localhost:"+port)
		if err != nil {
			t.Fatalf("dial of the proxy's own name: %v", err)
		}
		_ = conn.Close()
		if fp.connCount() != before+1 {
			t.Fatalf("proxy saw %d new connections, want 1", fp.connCount()-before)
		}
	})
}

func TestPortalIssuers_ProxyOptionValidated(t *testing.T) {
	listed := func(string) (bool, error) { return false, nil }
	reserved := func(string) bool { return false }
	for name, pp := range map[string]*PortalProxy{
		"no url":                 {Listed: listed},
		"another scheme":         {URL: &url.URL{Scheme: "ftp", Host: "proxy.corp.example:21"}, Listed: listed},
		"no host":                {URL: &url.URL{Scheme: "http", Host: ":3128"}, Listed: listed},
		"no host once converted": {URL: &url.URL{Scheme: "http", Host: "\u00ad:3128"}, Listed: listed}, // a soft hyphen converts to nothing
		"no matcher":             {URL: &url.URL{Scheme: "http", Host: "proxy.corp.example:3128", User: url.UserPassword("u", "MARKER")}},
	} {
		_, err := NewPortalIssuers(PortalIssuersOptions{Reserved: reserved, Proxy: pp})
		if err == nil || err.Error() != "portal issuers: the daemon proxy route is incomplete" {
			t.Fatalf("%s: %v, want the incomplete-route refusal with no URL text", name, err)
		}
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		pp := &PortalProxy{URL: &url.URL{Scheme: scheme, Host: "proxy.corp.example:3128"}, Listed: listed} // no issuer roots is accepted
		if _, err := NewPortalIssuers(PortalIssuersOptions{Reserved: reserved, Proxy: pp}); err != nil {
			t.Fatalf("%s: %v", scheme, err)
		}
	}
}

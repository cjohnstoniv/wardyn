// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCA is a private root and leaves it signs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Corp Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert, key, pool}
}

// leaf is a server certificate ca signed for ips and dns.
func (ca testCA) leaf(t *testing.T, ips []net.IP, dns ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "org leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: ips, DNSNames: dns,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// serveLeaf is a healthy organisation at an https://127.0.0.1 address presenting leaf; sawAuth reports
// whether any request carried an Authorization header.
func serveLeaf(t *testing.T, leaf tls.Certificate) (url string, sawAuth func() bool) {
	t.Helper()
	var mu sync.Mutex
	auth := false
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = auth || r.Header.Get("Authorization") != ""
		mu.Unlock()
		healthy(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, func() bool { mu.Lock(); defer mu.Unlock(); return auth }
}

// at is base for an organisation at url.
func at(url string) Network {
	n := base(org{srv: &httptest.Server{}})
	n.OrgURL = url
	return n
}

// An organisation addressed by IP is held to that address: a trusted certificate for another name fails the
// handshake, and the token is never sent to whatever answered.
func TestIPAddressCertificateMustCoverTheAddress(t *testing.T) {
	ca := newTestCA(t)
	for _, tc := range []struct {
		name string
		leaf tls.Certificate
		ok   bool
	}{
		{"names the address", ca.leaf(t, []net.IP{net.ParseIP("127.0.0.1")}), true},
		{"names another host", ca.leaf(t, nil, "wardyn.example.com"), false},
		{"names another address", ca.leaf(t, []net.IP{net.ParseIP("127.0.0.2")}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, sawAuth := serveLeaf(t, tc.leaf)
			n := at(url)
			n.SystemRoots = ca.pool
			stages := n.Run(context.Background())
			tlsSt := byName(t, stages, "tls")
			if tc.ok {
				if tlsSt.Status != StatusOK || !sawAuth() {
					t.Fatalf("tls = %s, token sent = %v; want ok and the identity check made:\n%s", tlsSt.Status, sawAuth(), text(t, stages))
				}
				return
			}
			if tlsSt.Status != StatusFail || tlsSt.Setting != "WARDYN_URL" || !strings.Contains(tlsSt.Detail, "does not cover 127.0.0.1") {
				t.Fatalf("tls = %+v, want a FAIL saying the certificate does not cover 127.0.0.1:\n%s", tlsSt, text(t, stages))
			}
			if sawAuth() {
				t.Fatalf("the token was sent to a server whose certificate does not cover 127.0.0.1:\n%s", text(t, stages))
			}
			if id := byName(t, stages, "identity"); id.Status != StatusSkip {
				t.Errorf("identity = %s after a refused handshake, want skip", id.Status)
			}
		})
	}
}

// The organisation's own anchors (the CLI's `wardyn ca add`) vouch for its host, and never for a proxy's.
func TestOriginCAVouchesForTheOrganisationOnly(t *testing.T) {
	ca := newTestCA(t)
	url, _ := serveLeaf(t, ca.leaf(t, []net.IP{net.ParseIP("127.0.0.1")}))
	n := at(url)
	n.OriginCA, n.OriginCASource = ca.pool, "`wardyn ca add`"
	stages := n.Run(context.Background())
	if tlsSt := byName(t, stages, "tls"); tlsSt.Status != StatusOK || !strings.Contains(tlsSt.Detail, "verified by the `wardyn ca add` trust store") {
		t.Fatalf("tls = %+v, want ok naming the stored anchors:\n%s", tlsSt, text(t, stages))
	}

	proxyLeaf := ca.leaf(t, nil, "proxy.example.com")
	cert, _ := x509.ParseCertificate(proxyLeaf.Certificate[0])
	p := &probe{n: &n, origin: "wardyn.example.com", proxy: "proxy.example.com"}
	if err := p.verify(tls.ConnectionState{ServerName: "proxy.example.com", PeerCertificates: []*x509.Certificate{cert}}); err == nil {
		t.Fatalf("the organisation's anchors vouched for the proxy's certificate (store %q)", p.store)
	}
}

// A stored `wardyn login` sign-in is named as one, and its refusal is fixed by signing in again, not by
// creating a personal API token.
func TestStoredSignInIsNamedAndItsRefusalSaysSignInAgain(t *testing.T) {
	o := newOrg(t, healthy)
	n := base(o)
	n.SystemRoots, n.TokenSource, n.TokenSignIn = o.pool, "wardyn login", true
	if id := byName(t, n.Run(context.Background()), "identity"); id.Status != StatusOK || id.Detail != "your stored `wardyn login` sign-in: method token, role admin" {
		t.Fatalf("identity = %+v, want ok naming the stored sign-in", id)
	}
	n.Token = "wrong-token-9999"
	id := byName(t, n.Run(context.Background()), "identity")
	if id.Status != StatusFail || id.Setting != "" || id.Fix != "sign in again with `wardyn login`" ||
		id.Detail != "/api/v1/me answered 401: your stored `wardyn login` sign-in was refused" {
		t.Fatalf("identity = %+v, want a FAIL naming the stored sign-in and `wardyn login`", id)
	}
}

// Each handshake is held to the host it dialled. One that sent no name is the organisation's whenever the
// organisation's address is an IP, zoned or not, and an https:// proxy's only when the organisation has a
// name: a proxy never answers for an organisation addressed by IP under its own certificate.
func TestEachHandshakeIsHeldToTheHostItDialled(t *testing.T) {
	for _, tc := range []struct {
		origin, proxy, sent, name string
		isOrigin                  bool
	}{
		{"wardyn.example.com", "", "wardyn.example.com", "wardyn.example.com", true},
		{"wardyn.example.com", "proxy.example.com", "proxy.example.com", "proxy.example.com", false},
		{"wardyn.example.com", "10.0.0.9", "", "10.0.0.9", false},
		{"10.0.0.5", "", "", "10.0.0.5", true},
		{"10.0.0.5", "proxy.example.com", "proxy.example.com", "proxy.example.com", false},
		{"10.0.0.5", "proxy.example.com", "", "10.0.0.5", true},
		{"fe80::1%eth0", "proxy.example.com", "", "fe80::1%eth0", true},
	} {
		p := &probe{origin: tc.origin, proxy: tc.proxy}
		if name, isOrigin := p.peer(tls.ConnectionState{ServerName: tc.sent}); name != tc.name || isOrigin != tc.isOrigin {
			t.Errorf("organisation %q, proxy %q, name sent %q: held to %q (organisation's: %v), want %q (%v)",
				tc.origin, tc.proxy, tc.sent, name, isOrigin, tc.name, tc.isOrigin)
		}
	}

	ca := newTestCA(t)
	cert, _ := x509.ParseCertificate(ca.leaf(t, nil, "proxy.example.com").Certificate[0])
	for _, origin := range []string{"10.0.0.5", "fe80::1%eth0"} {
		n := Network{SystemRoots: ca.pool}
		p := &probe{n: &n, origin: origin, proxy: "proxy.example.com"}
		if err := p.verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}); err == nil {
			t.Errorf("organisation %s behind an https:// proxy: the proxy's own certificate passed as the organisation's", origin)
		}
	}
}

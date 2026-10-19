// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// inspectingServer serves HTTPS with a leaf issued by a CA the client does not trust, as a TLS-inspecting
// proxy would.
func inspectingServer(t *testing.T) *httptest.Server {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Inspecting Root CA", Organization: []string{"Example Gateway"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "org.example"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectHintNamesPresentedIssuer(t *testing.T) {
	srv := inspectingServer(t)
	_, err := DialWebSocket(t.Context(), srv.URL, uuid.NewString(), nil)
	if err == nil {
		t.Fatal("a certificate from an untrusted issuer verified")
	}
	hint := connectHint(err)
	for _, want := range []string{`"org.example"`, `"Inspecting Root CA"`, "Example Gateway", "--trusted-ca-file"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q lacks %q (error: %v)", hint, want, err)
		}
	}
}

func TestConnectHintNamesRefusingProxy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens here now
	proxy, _ := url.Parse("http://u:secret@" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	_, err = DialWebSocket(t.Context(), "https://org.example", uuid.NewString(), client)
	if err == nil {
		t.Fatal("a dial through a closed proxy succeeded")
	}
	hint := connectHint(err)
	if !strings.Contains(hint, addr) || !strings.Contains(hint, "HTTPS_PROXY") {
		t.Errorf("hint %q does not name the proxy %s (error: %v)", hint, addr, err)
	}
	if strings.Contains(hint, "secret") || strings.Contains(hint, "u:") {
		t.Errorf("hint %q leaks the proxy's userinfo", hint)
	}
}

func TestConnectHintNamesUnresolvableHost(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "org.invalid", IsNotFound: true}
	if hint := connectHint(err); hint != "cannot resolve org.invalid" {
		t.Errorf("hint = %q", hint)
	}
	if hint := connectHint(context.Canceled); hint != "" {
		t.Errorf("an unrelated error got hint %q", hint)
	}
}

func TestConnectLogCarriesTheHint(t *testing.T) {
	logs := captureLog(t)
	var l connectLog
	l.failed(t.Context(), &net.DNSError{Err: "no such host", Name: "org.invalid"}, time.Second)
	warns := logs.records(t, "WARN")
	if len(warns) != 1 || warns[0]["hint"] != "cannot resolve org.invalid" {
		t.Fatalf("warnings = %v, want the hint attribute", warns)
	}
}

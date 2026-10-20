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
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

func TestConnectHintNamesUnresolvableProxy(t *testing.T) {
	proxy, _ := url.Parse("http://u:secret@proxy.invalid:3128")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	_, err := DialWebSocket(t.Context(), "https://org.example", uuid.NewString(), client)
	if err == nil {
		t.Fatal("a dial through an unresolvable proxy succeeded")
	}
	hint := connectHint(err)
	if !strings.Contains(hint, "proxy.invalid") || !strings.Contains(hint, "cannot be resolved") || strings.Contains(hint, "secret") || strings.Contains(hint, "did not accept") {
		t.Errorf("hint %q does not say the proxy host cannot be resolved (error: %v)", hint, err)
	}
}

// A proxy that answers CONNECT with 407 or 403 is the usual corporate refusal; net/http reports it as a
// bare status text, so the hint comes from the dial's own classification.
func TestConnectHintNamesProxyThatRefusesConnect(t *testing.T) {
	for status, want := range map[int]string{http.StatusProxyAuthRequired: "it wants proxy authentication", http.StatusForbidden: "it does not allow this destination"} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			proxy, _ := url.Parse("http://u:secret@" + srv.Listener.Addr().String())
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
			_, err := DialWebSocket(t.Context(), "https://org.example", uuid.NewString(), client)
			if err == nil {
				t.Fatal("a dial through a refusing proxy succeeded")
			}
			hint := connectHint(err)
			for _, need := range []string{"the proxy at " + srv.Listener.Addr().String() + " refused the connection", "(" + strconv.Itoa(status) + ": " + want + ")"} {
				if !strings.Contains(hint, need) {
					t.Errorf("hint %q lacks %q (error: %v)", hint, need, err)
				}
			}
			if strings.Contains(hint, "secret") {
				t.Errorf("hint %q leaks the proxy's userinfo", hint)
			}
		})
	}
	// No proxy in use: a bare "Forbidden" is not a proxy's answer.
	if hint := connectHint(errors.New("Forbidden")); hint != "" {
		t.Errorf("unrelated error got hint %q", hint)
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

func TestOrgRouteNamesTheProxyHostAndPortOnly(t *testing.T) {
	id := uuid.NewString()
	proxy, _ := url.Parse("http://u:secret@proxy.example:3128")
	if got := orgRoute("https://org.example", id, http.ProxyURL(proxy)); got != "via proxy proxy.example:3128 (HTTPS_PROXY)" {
		t.Errorf("route = %q", got)
	}
	if got := orgRoute("https://org.example", id, func(*http.Request) (*url.URL, error) { return nil, nil }); got != "direct" {
		t.Errorf("route = %q", got)
	}
	if got := orgRoute("not a url", id, http.ProxyURL(proxy)); got != "direct" {
		t.Errorf("route = %q", got)
	}
}

// The lines that say what the runner could not reach name the proxy by host and port only: never its
// user name and password, path or query, in the hint or in the logged error.
func TestReachLinesNameProxyHostAndPortOnly(t *testing.T) {
	leaks := []string{"u:p", "/path", "q=1"}
	check := func(t *testing.T, what, got string) {
		t.Helper()
		for _, leak := range leaks {
			if strings.Contains(got, leak) {
				t.Errorf("%s %q carries %q", what, got, leak)
			}
		}
	}
	t.Run("closed proxy", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		proxy, _ := url.Parse("http://u:p@" + addr + "/path?q=1")
		_, err = DialWebSocket(t.Context(), "https://org.example", uuid.NewString(), &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}})
		if err == nil {
			t.Fatal("a dial through a closed proxy succeeded")
		}
		if hint := connectHint(err); hint != "the proxy "+addr+" named by HTTPS_PROXY did not accept the connection" {
			t.Errorf("hint = %q", hint)
		}
		check(t, "error", err.Error())
	})
	t.Run("refusing proxy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusProxyAuthRequired) }))
		defer srv.Close()
		addr := srv.Listener.Addr().String()
		proxy, _ := url.Parse("http://u:p@" + addr + "/path?q=1")
		_, err := DialWebSocket(t.Context(), "https://org.example", uuid.NewString(), &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}})
		if err == nil {
			t.Fatal("a dial through a refusing proxy succeeded")
		}
		if hint := connectHint(err); hint != "the proxy at "+addr+" refused the connection (407: it wants proxy authentication)" {
			t.Errorf("hint = %q", hint)
		}
		check(t, "error", err.Error())
	})
	t.Run("route", func(t *testing.T) {
		proxy, _ := url.Parse("http://u:p@proxy.example:3128/path?q=1")
		if got := orgRoute("https://org.example", uuid.NewString(), http.ProxyURL(proxy)); got != "via proxy proxy.example:3128 (HTTPS_PROXY)" {
			t.Errorf("route = %q", got)
		}
	})
}

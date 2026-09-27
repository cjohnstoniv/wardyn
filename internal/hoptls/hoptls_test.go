// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package hoptls

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckURL_PlaintextOnlyOnLoopback(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://wardynd:8443", true},
		{"https://wardyn.wardyn.svc.cluster.local:8443", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.9.9.9:8080", true},
		{"http://[::1]:8080", true},
		{"http://localhost:8080", true},
		{"http://LOCALHOST.:8080", true},
		{"http://wardynd:8080", false},
		{"http://wardyn.wardyn.svc.cluster.local:8080", false},
		{"http://host.docker.internal:8080", false},
		{"http://10.0.0.5:8080", false},
		{"http://localhost.example.com:8080", false},
		{"ftp://wardynd", false},
		{"wardynd:8080", false},
		{"", false},
	} {
		err := CheckURL(tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("CheckURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
	if err := CheckURL("http://wardynd:8080"); err == nil || !strings.Contains(err.Error(), "WARDYN_INTERNAL_LISTEN") {
		t.Errorf("refusal must name the fix, got %v", err)
	}
}

// serve stands up a TLS server presenting cert.
func serve(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, cfg *tls.Config, srv *httptest.Server, host string) error {
	t.Helper()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.Clone()}, Timeout: 5 * time.Second}
	resp, err := c.Get("https://" + net.JoinHostPort(host, port) + "/")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func TestServingCert_VerifiesOnlyAgainstItsCA(t *testing.T) {
	now := time.Now()
	blob, err := NewCA(now)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseCA(blob)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.ServingCert("127.0.0.1", now)
	if err != nil {
		t.Fatal(err)
	}
	srv := serve(t, cert)

	pinned, err := ClientConfig(string(ca.CertPEM))
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, pinned, srv, "127.0.0.1"); err != nil {
		t.Fatalf("pinned CA must verify its own serving cert: %v", err)
	}

	otherBlob, _ := NewCA(now)
	other, _ := ParseCA(otherBlob)
	wrong, _ := ClientConfig(string(other.CertPEM))
	if err := get(t, wrong, srv, "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a different CA must fail the handshake, got %v", err)
	}

	empty, _ := ClientConfig("")
	if empty.RootCAs == nil {
		t.Fatal("a nil RootCAs means the system roots; the pin must be an explicit (empty) pool")
	}
	if err := get(t, empty, srv, "127.0.0.1"); err == nil {
		t.Fatal("an empty CA pin must fail closed, not fall back to the system roots")
	}

	if _, err := ClientConfig("not a pem"); err == nil {
		t.Fatal("garbage CA PEM must be refused")
	}
}

func TestServingCert_NamesTheDialedHost(t *testing.T) {
	now := time.Now()
	blob, _ := NewCA(now)
	ca, _ := ParseCA(blob)
	for _, host := range []string{"wardynd", "10.1.2.3"} {
		cert, err := ca.ServingCert(host, now)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
		if !leaf.NotAfter.Equal(ca.cert.NotAfter) {
			t.Errorf("%s: serving cert must live as long as the CA", host)
		}
	}
}

func TestParseCA_RefusesPartialBlobs_AndFreshness(t *testing.T) {
	now := time.Now()
	blob, _ := NewCA(now)
	ca, _ := ParseCA(blob)
	if _, err := ParseCA(ca.CertPEM); err == nil {
		t.Fatal("a certificate without its key is not a CA wardynd can sign with")
	}
	if !ca.Fresh(now) {
		t.Fatal("a new CA must be fresh")
	}
	if ca.Fresh(now.Add(caTTL - rotateBefore + time.Hour)) {
		t.Fatal("a CA inside the rotation window must not be kept")
	}
}

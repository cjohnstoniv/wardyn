// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The issuer portal's half of the daemon proxy's always list: the route bootDaemonProxy hands to the verifier
// set, and the trust the verifier set gets for a proxied issuer.

// portalProxyTestCA is a self-signed CA certificate made for one test, as PEM.
func portalProxyTestCA(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// portalProxyTestFile writes contents to a fresh file and returns its path.
func portalProxyTestFile(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// portalTrustedUnder reports whether a CA certificate in PEM verifies under pool.
func portalTrustedUnder(t *testing.T, caPEM []byte, pool *x509.CertPool) bool {
	t.Helper()
	block, _ := pem.Decode(caPEM)
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Verify(x509.VerifyOptions{Roots: pool})
	return err == nil
}

// portalBootFlags are the flags buildPortalIssuers reads, all empty but the proxied issuer CA file.
func portalBootFlags(caFile string) *bootFlags {
	empty, empty2 := "", ""
	return &bootFlags{portalInternalHosts: &empty, portalInternalCAFile: &empty2, portalProxiedIssuerCAFile: &caFile}
}

func TestPortalProxyRoute_NoProxySuffixStillProxied(t *testing.T) {
	clearProxyEnv(t)
	defer swapDefaultTransport()()
	_, route, err := installBootTransport(http.DefaultTransport, nil, alwaysFlags("http://proxy.corp.example:3128", "corp.example", "login.corp.example"))
	if err != nil || route == nil {
		t.Fatalf("installBootTransport = %v, %v; want a route", route, err)
	}
	if got := route.proxy.String(); got != "http://proxy.corp.example:3128" {
		t.Fatalf("route proxy %q, want the daemon proxy", got)
	}
	for host, want := range map[string]bool{
		"login.corp.example": true, "LOGIN.Corp.Example": true, "login.corp.example.": true, "ｌogin.corp.example": true,
		"x.login.corp.example": false, "corp.example": false,
	} {
		if got, err := route.listed(host); err != nil || got != want {
			t.Errorf("listed(%q) = %v, %v; want %v", host, got, err, want)
		}
	}
	if got := proxyFor(t, http.DefaultTransport, "https://login.corp.example/"); got != "" {
		t.Errorf("http.DefaultTransport sends the listed host to %q, want it still bypassed", got)
	}
}

func TestPortalProxyRoute_EnvironmentProxyFallback(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://envproxy.corp.example:3128")
	t.Setenv("NO_PROXY", ".corp.example")
	_, route, err := bootDaemonProxy(freshTransport(), alwaysFlags("", "", "login.corp.example"))
	if err != nil || route == nil {
		t.Fatalf("bootDaemonProxy = %v, %v; want a route", route, err)
	}
	if got := route.proxy.String(); got != "http://envproxy.corp.example:3128" {
		t.Fatalf("route proxy %q, want HTTPS_PROXY", got)
	}
	if listed, err := route.listed("login.corp.example"); err != nil || !listed {
		t.Fatalf("listed = %v, %v; want true", listed, err)
	}
}

func TestPortalProxyRoute_EmptyListLeavesPortalDirect(t *testing.T) {
	clearProxyEnv(t)
	for _, list := range []string{"", " , "} {
		grant, route, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", list))
		if err != nil || grant != nil || route != nil {
			t.Fatalf("list %q: %v, %v, %v; want no grant transport and no route", list, grant, route, err)
		}
	}
	caPEM := portalProxyTestCA(t, "issuer CA")
	pool, _, err := portalProxiedIssuerRoots(portalProxyTestFile(t, "issuer.pem", caPEM), "")
	if err != nil {
		t.Fatal(err)
	}
	if opt := portalProxyOption(nil, pool); opt != nil {
		t.Fatalf("no route gave a proxy option %+v", opt)
	}
}

func TestDaemonProxyAlways_RefusesPortalInternalIssuerHost(t *testing.T) {
	clearProxyEnv(t)
	for _, internal := range []string{"idp.corp.example:8443", "IDP.corp.example", "other.example,idp.corp.example."} {
		f := alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example,idp.corp.example")
		f.portalInternalHosts = &internal
		_, _, err := bootDaemonProxy(freshTransport(), f)
		if err == nil || !strings.Contains(err.Error(), "entry 2 ") || !strings.Contains(err.Error(), "a portal internal issuer host") {
			t.Errorf("internal hosts %q: %v; want entry 2 refused as a portal internal issuer host", internal, err)
		}
	}
	// Another internal host, and flags that do not carry the setting, boot; the internal host joins only the
	// comparison, never the daemon transport's bypass.
	internal := "idp.corp.example:8443"
	f := alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example")
	f.portalInternalHosts = &internal
	tr := freshTransport()
	if _, route, err := bootDaemonProxy(tr, f); err != nil || route == nil {
		t.Fatalf("an unrelated internal host: %v, %v", route, err)
	}
	if got := proxyFor(t, tr, "https://idp.corp.example:8443/"); got != "http://proxy.corp.example:3128" {
		t.Errorf("the shared transport sends the internal host to %q, want its own answer (the proxy)", got)
	}
	if _, route, err := bootDaemonProxy(freshTransport(), alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example")); err != nil || route == nil {
		t.Fatalf("no internal hosts setting: %v, %v", route, err)
	}
}

func TestPortalProxyRoute_SharedWithServiceGrant(t *testing.T) {
	const proxy = "http://proxy.corp.example:3128"
	for name, noProxy := range map[string]string{"token host not covered": "", "token host covered": "token.corp.example"} {
		clearProxyEnv(t)
		grant, route, err := bootDaemonProxy(freshTransport(), alwaysFlags(proxy, noProxy, "login.corp.example,auth.corp.example,xn--bcher-kva.corp.example"))
		if err != nil || grant == nil || route == nil {
			t.Fatalf("%s: %v, %v, %v", name, grant, route, err)
		}
		for _, host := range []string{"login.corp.example", "auth.corp.example", "bücher.corp.example"} {
			listed, err := route.listed(host)
			if err != nil || !listed || proxyFor(t, grant, "https://"+host+"/token") != proxy {
				t.Errorf("%s: %s: listed %v (%v), grant sends it to %q; want both to name the proxy", name, host, listed, err, proxyFor(t, grant, "https://"+host+"/token"))
			}
		}
		// An unlisted token host inherits the shared routing, and a portal fetch for it stays direct either way.
		want := proxy
		if noProxy != "" {
			want = ""
		}
		if got := proxyFor(t, grant, "https://token.corp.example/token"); got != want {
			t.Errorf("%s: the unlisted token host goes to %q, want %q", name, got, want)
		}
		for _, host := range []string{"token.corp.example", "x.login.corp.example"} {
			if listed, err := route.listed(host); err != nil || listed {
				t.Errorf("%s: listed(%s) = %v, %v; want false", name, host, listed, err)
			}
		}
	}
	clearProxyEnv(t)
	_, _, err := bootDaemonProxy(freshTransport(), alwaysFlags(proxy, "", "login.corp.example:443"))
	if err == nil || !strings.Contains(err.Error(), "not an exact host name (no port, path, suffix, wildcard or other text)") {
		t.Fatalf("an entry with a port: %v; want the exact-host refusal", err)
	}
}

func TestPortalProxyRoute_IssuerRootsFromFileOnly(t *testing.T) {
	if os.Getenv("WARDYN_TEST_PORTAL_ROOTS_CHILD") != "" {
		portalRootsChild(t)
		return
	}
	dir := t.TempDir()
	sysCA, trustedCA, issuerCA := portalProxyTestCA(t, "system CA"), portalProxyTestCA(t, "trusted CA"), portalProxyTestCA(t, "issuer CA")
	sysFile := filepath.Join(dir, "system.pem")
	issuerFile := filepath.Join(dir, "issuer.pem")
	for path, data := range map[string][]byte{sysFile: sysCA, issuerFile: issuerCA} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPortalProxyRoute_IssuerRootsFromFileOnly$", "-test.count=1", "-test.v")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSL_CERT_DIR=") && !strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "SSL_CERT_FILE="+sysFile, "WARDYN_TEST_PORTAL_ROOTS_CHILD="+issuerFile,
		"WARDYN_TEST_PORTAL_ROOTS_SYSTEM="+string(sysCA), "WARDYN_TEST_PORTAL_ROOTS_TRUSTED="+string(trustedCA))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestPortalProxyRoute_IssuerRootsFromFileOnly") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

// portalRootsChild runs with SSL_CERT_FILE naming a CA, so the system roots hold it.
func portalRootsChild(t *testing.T) {
	issuerFile := os.Getenv("WARDYN_TEST_PORTAL_ROOTS_CHILD")
	sysCA, trustedCA := []byte(os.Getenv("WARDYN_TEST_PORTAL_ROOTS_SYSTEM")), []byte(os.Getenv("WARDYN_TEST_PORTAL_ROOTS_TRUSTED"))
	sys, err := x509.SystemCertPool()
	if err != nil || !portalTrustedUnder(t, sysCA, sys) {
		t.Fatalf("the system roots do not hold the test CA: %v", err)
	}
	issuerCA, err := os.ReadFile(issuerFile)
	if err != nil {
		t.Fatal(err)
	}
	pool, n, err := portalProxiedIssuerRoots(issuerFile, string(trustedCA))
	if err != nil || n != 1 {
		t.Fatalf("portalProxiedIssuerRoots = %d, %v", n, err)
	}
	fileOnly := x509.NewCertPool()
	fileOnly.AppendCertsFromPEM(issuerCA)
	if !pool.Equal(fileOnly) || !portalTrustedUnder(t, issuerCA, pool) || portalTrustedUnder(t, sysCA, pool) || portalTrustedUnder(t, trustedCA, pool) {
		t.Fatal("the proxied issuer pool is not the issuer file alone")
	}

	// The proxy option carries the route's proxy and roots and the file's pool, each in its own field.
	clearProxyEnv(t)
	tr := freshTransport()
	proxyRoots := x509.NewCertPool()
	proxyRoots.AppendCertsFromPEM(trustedCA)
	installTrustedCA(tr, proxyRoots)
	_, route, err := bootDaemonProxy(tr, alwaysFlags("http://proxy.corp.example:3128", "", "login.corp.example"))
	if err != nil || route == nil {
		t.Fatalf("bootDaemonProxy = %v, %v", route, err)
	}
	opt := portalProxyOption(route, pool)
	if opt == nil || opt.URL != route.proxy || opt.ProxyRoots != route.proxyRoots || route.proxyRoots != proxyRoots || opt.IssuerRoots != pool {
		t.Fatalf("proxy option %+v does not carry the route's proxy, the route's proxy roots and the issuer pool", opt)
	}
	for _, host := range []string{"login.corp.example", "other.corp.example"} {
		got, gerr := opt.Listed(host)
		want, werr := route.listed(host)
		if got != want || (gerr == nil) != (werr == nil) {
			t.Fatalf("Listed(%s) = %v, %v; route.listed = %v, %v", host, got, gerr, want, werr)
		}
	}
	if portalProxyOption(nil, pool) != nil {
		t.Fatal("a nil route gave a proxy option")
	}

	// Unset: a nil pool, and with a non-empty always list one boot warning.
	if pool, n, err := portalProxiedIssuerRoots("  ", string(trustedCA)); pool != nil || n != 0 || err != nil {
		t.Fatalf("an unset path gave %v, %d, %v; want a nil pool", pool, n, err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, _, err := buildPortalIssuers(portalBootFlags(""), "operator", store.PG{}, route, string(trustedCA)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "WARDYN_DAEMON_PROXY_ALWAYS is set and WARDYN_PORTAL_PROXIED_ISSUER_CA_FILE is not"); n != 1 {
		t.Fatalf("%d boot warnings, want 1: %s", n, logs.String())
	}

	// Boot refusals name the variable.
	refusals := map[string]string{
		"unreadable":     filepath.Join(t.TempDir(), "missing.pem"),
		"empty":          portalProxyTestFile(t, "empty.pem", nil),
		"no certificate": portalProxyTestFile(t, "junk.pem", []byte("not a certificate\n")),
		"shared":         portalProxyTestFile(t, "shared.pem", append(append([]byte{}, issuerCA...), trustedCA...)),
	}
	for name, path := range refusals {
		_, _, err := buildPortalIssuers(portalBootFlags(path), "operator", store.PG{}, route, string(trustedCA))
		if err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "WARDYN_PORTAL_PROXIED_ISSUER_CA_FILE") {
			t.Errorf("%s: %v; want a boot refusal naming WARDYN_PORTAL_PROXIED_ISSUER_CA_FILE", name, err)
		}
		if name == "shared" && (err == nil || !strings.Contains(err.Error(), "WARDYN_TRUSTED_CA_FILE")) {
			t.Errorf("shared: %v; want it to name WARDYN_TRUSTED_CA_FILE too", err)
		}
	}
}

// portalProxyTestCAOnKey is a self-signed CA certificate for key under name, as PEM; serial tells renewals apart.
func portalProxyTestCAOnKey(t *testing.T, name string, key *ecdsa.PrivateKey, serial int64) []byte {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The issuer CA file is refused when any of its certificates holds a public key that WARDYN_TRUSTED_CA_FILE also
// holds, whatever the certificate's subject, serial or validity, and whichever file has the renewed copy.
func TestPortalProxiedIssuerRoots_SharedPublicKeyRefused(t *testing.T) {
	k1, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	orig := portalProxyTestCAOnKey(t, "Inspection CA", k1, 1)
	for name, c := range map[string]struct {
		issuer, trusted []byte
		refused         bool
	}{
		"identical certificate":                {orig, orig, true},
		"same key renewed, renewal in issuer":  {portalProxyTestCAOnKey(t, "Inspection CA", k1, 2), orig, true},
		"same key renewed, renewal in trusted": {orig, portalProxyTestCAOnKey(t, "Inspection CA", k1, 2), true},
		"same key under another subject":       {portalProxyTestCAOnKey(t, "Another name", k1, 3), orig, true},
		"another key under the same subject":   {portalProxyTestCAOnKey(t, "Inspection CA", k2, 4), orig, false},
	} {
		pool, n, err := portalProxiedIssuerRoots(portalProxyTestFile(t, "issuer.pem", c.issuer), string(c.trusted))
		if c.refused {
			if err == nil || pool != nil || n != 0 || !strings.Contains(err.Error(), "public key is also in a certificate in WARDYN_TRUSTED_CA_FILE") {
				t.Errorf("%s: %v, %d; want a refusal naming the shared public key", name, err, n)
			}
		} else if err != nil || pool == nil || n != 1 {
			t.Errorf("%s: %v, %d; want the one certificate accepted", name, err, n)
		}
	}
}

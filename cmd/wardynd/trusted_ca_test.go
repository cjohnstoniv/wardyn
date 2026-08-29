// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// genTestCert returns a self-signed cert (PEM) with the given CommonName —
// enough to exercise the PEM-parsing path without a real corporate CA.
func genTestCert(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestLoadTrustedCA_UnsetIsAllNil(t *testing.T) {
	// Negative control for the whole feature: with the knob unset, nothing
	// changes — no PEM, no pool, count 0, no error.
	pemOut, pool, n, err := loadTrustedCA("")
	if err != nil || pemOut != "" || pool != nil || n != 0 {
		t.Fatalf("loadTrustedCA(\"\") = (%q, %v, %d, %v), want (\"\", nil, 0, nil)", pemOut, pool, n, err)
	}
	// Whitespace-only is the same as empty (flagEnv can hand a trimmed blank).
	if _, _, _, err := loadTrustedCA("   "); err != nil {
		t.Fatalf("loadTrustedCA(whitespace) errored: %v", err)
	}
}

func TestLoadTrustedCA_ValidAppendsToSystemPool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corp-ca.pem")
	certA := genTestCert(t, "Corp Root A")
	certB := genTestCert(t, "Corp Root B")
	if err := os.WriteFile(path, append(certA, certB...), 0o600); err != nil {
		t.Fatal(err)
	}

	pemOut, pool, n, err := loadTrustedCA(path)
	if err != nil {
		t.Fatalf("loadTrustedCA: %v", err)
	}
	if n != 2 {
		t.Fatalf("n = %d, want 2", n)
	}
	if pool == nil {
		t.Fatal("pool = nil, want a populated pool")
	}
	if pemOut != string(append(certA, certB...)) {
		t.Fatal("returned PEM does not match the file content verbatim")
	}

	sysPool, _ := x509.SystemCertPool()
	sysCount := 0
	if sysPool != nil {
		sysCount = len(sysPool.Subjects()) //nolint:staticcheck // deprecated but the simplest count for a test assertion
	}
	if got := len(pool.Subjects()); got <= sysCount { //nolint:staticcheck // see above
		t.Fatalf("pool has %d subjects, want more than the system pool's %d (the two test certs must have been added)", got, sysCount)
	}
}

func TestLoadTrustedCA_RefusesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(path, []byte("not a pem file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadTrustedCA(path); err == nil {
		t.Fatal("loadTrustedCA(garbage) did not error")
	} else if !strings.Contains(err.Error(), "WARDYN_TRUSTED_CA_FILE") {
		t.Fatalf("error %q does not name WARDYN_TRUSTED_CA_FILE", err.Error())
	}
}

func TestLoadTrustedCA_RefusesUnparseableCertBlock(t *testing.T) {
	// A PEM CERTIFICATE block whose payload is not a valid DER certificate —
	// distinct from "no PEM blocks at all" (TestLoadTrustedCA_RefusesGarbage):
	// this exercises the per-block x509.ParseCertificate failure path.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-block.pem")
	bad := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a real cert DER")})
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadTrustedCA(path); err == nil {
		t.Fatal("loadTrustedCA(unparseable block) did not error")
	}
}

func TestLoadTrustedCA_RefusesMissingFile(t *testing.T) {
	_, _, _, err := loadTrustedCA(filepath.Join(t.TempDir(), "does-not-exist.pem"))
	if err == nil {
		t.Fatal("loadTrustedCA(missing file) did not error")
	}
	if !strings.Contains(err.Error(), "WARDYN_TRUSTED_CA_FILE") {
		t.Fatalf("error %q does not name WARDYN_TRUSTED_CA_FILE", err.Error())
	}
}

func TestInstallTrustedCA_NilPoolIsNoOp(t *testing.T) {
	tr := &http.Transport{}
	installTrustedCA(tr, nil)
	if tr.TLSClientConfig != nil {
		t.Fatalf("TLSClientConfig = %v, want nil (unset knob must leave the transport untouched)", tr.TLSClientConfig)
	}
}

func TestInstallTrustedCA_PreservesPreSeededNextProtos(t *testing.T) {
	preSeeded := []string{"h2", "http/1.1"}
	tr := &http.Transport{TLSClientConfig: &tls.Config{NextProtos: preSeeded}}
	pool := x509.NewCertPool()
	pool.AddCert(mustParseCert(t, genTestCert(t, "Corp Root")))

	installTrustedCA(tr, pool)

	if got := tr.TLSClientConfig.NextProtos; len(got) != len(preSeeded) || got[0] != preSeeded[0] || got[1] != preSeeded[1] {
		t.Fatalf("NextProtos = %v, want unchanged %v (installTrustedCA must not clobber pre-existing TLSClientConfig fields)", got, preSeeded)
	}
	if tr.TLSClientConfig.RootCAs != pool {
		t.Fatal("RootCAs was not set to the given pool")
	}
}

func TestInstallTrustedCA_CreatesConfigWhenNil(t *testing.T) {
	tr := &http.Transport{}
	pool := x509.NewCertPool()
	pool.AddCert(mustParseCert(t, genTestCert(t, "Corp Root")))

	installTrustedCA(tr, pool)

	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs != pool {
		t.Fatal("installTrustedCA did not create a TLSClientConfig with RootCAs set")
	}
}

func mustParseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("pem.Decode returned nil block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// TestBootWarnsWhenDefaultPolicyLacksGatewayHost: the advisory boot warning
// fires only when a configured gateway's host is absent from the default
// policy's egress, never on allow-all or an exact-covering entry, and never
// when no gateway is configured at all.
func TestBootWarnsWhenDefaultPolicyLacksGatewayHost(t *testing.T) {
	gw := map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}

	if got := defaultPolicyMissingGatewayHosts(types.RunPolicySpec{}, gw); len(got) != 1 || got["api.anthropic.com"] != "llm-gateway.corp.internal" {
		t.Fatalf("expected the gateway host reported missing, got %v", got)
	}
	if got := defaultPolicyMissingGatewayHosts(types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal"}}, gw); len(got) != 0 {
		t.Fatalf("an exact covering entry must suppress the warning, got %v", got)
	}
	if got := defaultPolicyMissingGatewayHosts(types.RunPolicySpec{AllowAllEgress: true}, gw); len(got) != 0 {
		t.Fatalf("allow_all_egress must suppress the warning, got %v", got)
	}
	// Negative control: no gateway configured at all -> never warns.
	if got := defaultPolicyMissingGatewayHosts(types.RunPolicySpec{}, nil); len(got) != 0 {
		t.Fatalf("no gateway configured must never warn, got %v", got)
	}
}

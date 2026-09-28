// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// genTestTLSKeyPair returns a self-signed cert+key (PEM): the exact shape
// WARDYN_TLS_CERT/WARDYN_TLS_KEY point at.
func genTestTLSKeyPair(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wardyn-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// unsafeTLSKeyModes mirrors unsafePlatformKeyModes/unsafeFederatedTokenModes:
// 0o644 is refused only via CheckSecretFileMode's "other-readable AND
// non-root euid owns it" rule, so it is not unsafe when the test itself runs
// as root (euid 0) — the group-/world-WRITABLE modes are refused
// unconditionally either way.
func unsafeTLSKeyModes() []os.FileMode {
	modes := []os.FileMode{0o666, 0o620, 0o602}
	if os.Geteuid() != 0 {
		modes = append(modes, 0o644)
	}
	return modes
}

// TestLoadTLSConfig_RejectsUnsafeKeyMode pins #1297: WARDYN_TLS_KEY must go
// through the same cliutil mode rule as every other secret file (#1116,
// #1293) — a group- or world-readable/writable private key otherwise lets a
// local user read or replace the console's TLS identity with no signal at
// boot, exactly the gap ListenAndServeTLS's own unchecked file read left.
func TestLoadTLSConfig_RejectsUnsafeKeyMode(t *testing.T) {
	certPEM, keyPEM := genTestTLSKeyPair(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, mode := range unsafeTLSKeyModes() {
		t.Run(mode.String(), func(t *testing.T) {
			if err := os.Chmod(keyPath, mode); err != nil {
				t.Fatal(err)
			}
			_, err := loadTLSConfig(certPath, keyPath)
			if err == nil ||
				!strings.Contains(err.Error(), "WARDYN_TLS_KEY") ||
				!strings.Contains(err.Error(), keyPath) {
				t.Fatalf("mode %04o: want a refusal naming the setting and path, got %v", mode, err)
			}
		})
	}
}

// TestLoadTLSConfig_AcceptsSafeKeyMode proves the rule does not regress the
// delivery shape every WARDYN_TLS_KEY deployment doc and `chmod 600` fix
// produces.
func TestLoadTLSConfig_AcceptsSafeKeyMode(t *testing.T) {
	certPEM, keyPEM := genTestTLSKeyPair(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o400, 0o600} {
		t.Run(mode.String(), func(t *testing.T) {
			keyPath := filepath.Join(t.TempDir(), "tls.key")
			if err := os.WriteFile(keyPath, keyPEM, mode); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadTLSConfig(certPath, keyPath)
			if err != nil {
				t.Fatalf("mode %04o: loadTLSConfig refused a safe mode: %v", mode, err)
			}
			if cfg == nil || len(cfg.Certificates) != 1 {
				t.Fatalf("mode %04o: cfg = %+v, want exactly one certificate", mode, cfg)
			}
		})
	}
}

// TestLoadTLSConfig_CertNotModeChecked proves the certificate — public by
// design — is read regardless of its mode, unlike the key: #1297 explicitly
// scopes the fix to WARDYN_TLS_KEY.
func TestLoadTLSConfig_CertNotModeChecked(t *testing.T) {
	certPEM, keyPEM := genTestTLSKeyPair(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certPath, certPEM, 0o666); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTLSConfig(certPath, keyPath); err != nil {
		t.Fatalf("a world-writable cert was refused: %v; only the key should be mode-checked", err)
	}
}

// TestListenersDoNotShareOneTLSConfig guards the shared-mutation hazard
// loadTLSConfig's own doc comment warns about: net/http's HTTP/2 setup
// (onceSetNextProtoDefaults, h2_bundle.go) mutates an http.Server's TLSConfig
// IN PLACE — appending "h2"/"http/1.1" to NextProtos and setting
// PreferServerCipherSuites — before ServeTLS's own later cloneTLSConfig call
// (net/http/server.go: "Setup HTTP/2 before s.Serve, to initialize
// s.TLSConfig before we clone it"). Two *http.Server instances pointed at the
// SAME *tls.Config would race on that mutation: the exact shape of the 0.7.9
// regression (a shared *tls.Config mutated by HTTP/2 broke corporate-CA
// installs). Exercises the actual seam both listeners go through
// (tlsConfigForListener, boot_serve.go) rather than a hand-rolled Clone(), so
// reverting that seam back to handing out posture.tlsConfig itself turns this
// red — starts two real TLS listeners off what it returns and proves the
// ORIGINAL loaded config is left untouched.
func TestListenersDoNotShareOneTLSConfig(t *testing.T) {
	certPEM, keyPEM := genTestTLSKeyPair(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := loadTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}
	if len(base.NextProtos) != 0 {
		t.Fatalf("base.NextProtos = %v before any listener started, want empty", base.NextProtos)
	}
	posture := tlsPosture{tlsEnabled: true, tlsConfig: base}

	// startOne calls the SAME seam boot_serve.go's two httpSrv constructions
	// call (tlsConfigForListener), not a hand-rolled Clone().
	startOne := func(t *testing.T) *http.Server {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.NewServeMux(), TLSConfig: tlsConfigForListener(posture)}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		// ServeTLS's HTTP/2 setup runs before it starts accepting, so a
		// successful dial proves that setup already ran on srv's OWN clone.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			//nolint:gosec // test-only dial against our own self-signed cert
			conn, derr := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
			if derr == nil {
				conn.Close()
				return srv
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("listener never accepted a TLS connection")
		return nil
	}

	srv1 := startOne(t)
	srv2 := startOne(t)

	if !slices.Contains(srv1.TLSConfig.NextProtos, "h2") || !slices.Contains(srv2.TLSConfig.NextProtos, "h2") {
		t.Fatal("neither listener's own TLSConfig clone was set up for HTTP/2 — the test's premise (H2 setup mutates TLSConfig) does not hold; check net/http's onceSetNextProtoDefaults")
	}
	if len(base.NextProtos) != 0 {
		t.Fatalf("the ORIGINAL shared config was mutated: base.NextProtos = %v, want untouched (empty) — each listener must use its own .Clone()", base.NextProtos)
	}
}

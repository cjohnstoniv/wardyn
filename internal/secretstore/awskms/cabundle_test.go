// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
)

// The CA file adds to the system roots, never replaces them; a file with no
// certificate (or no file) is refused; an empty path keeps the base config
// (or a fresh TLS 1.2 one) and never mutates the base.
func TestTLSConfig_CABundle(t *testing.T) {
	ca, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good, empty := filepath.Join(dir, "good.pem"), filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(good, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	want.AppendCertsFromPEM(ca)

	cfg, err := tlsConfig(nil, "")
	if err != nil || cfg.RootCAs != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("nil base, no CA file = (%+v, %v); want the default roots and TLS 1.2", cfg, err)
	}
	base := &tls.Config{ServerName: "kms.example", MinVersion: tls.VersionTLS13}
	cfg, err = tlsConfig(base, "")
	if err != nil || cfg == base || cfg.ServerName != "kms.example" || cfg.MinVersion != tls.VersionTLS13 || cfg.RootCAs != nil {
		t.Fatalf("base, no CA file = (%+v, %v); want a clone of the base", cfg, err)
	}
	cfg, err = tlsConfig(base, good)
	if err != nil || cfg == base || base.RootCAs != nil || cfg.ServerName != "kms.example" || !cfg.RootCAs.Equal(want) {
		t.Fatalf("base, CA file = (%+v, %v); want a clone of the base with the system roots plus the bundle", cfg, err)
	}
	cfg, err = tlsConfig(nil, good)
	if err != nil || cfg.MinVersion != tls.VersionTLS12 || !cfg.RootCAs.Equal(want) {
		t.Fatalf("nil base, CA file = (%+v, %v); want TLS 1.2 and the system roots plus the bundle", cfg, err)
	}
	if cfg, err := tlsConfig(base, empty); err == nil || cfg != nil {
		t.Fatalf("a bundle with no certificate = (%v, %v); want a refusal", cfg, err)
	}
	if cfg, err := tlsConfig(base, filepath.Join(dir, "missing.pem")); err == nil || cfg != nil {
		t.Fatalf("a missing bundle = (%v, %v); want a refusal", cfg, err)
	}
}

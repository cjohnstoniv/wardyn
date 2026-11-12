// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

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
// certificate (or no file) is refused; an empty path leaves the default roots.
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

	cfg, err := tlsConfig("")
	if err != nil || cfg.RootCAs != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("no CA file = (%+v, %v); want the default roots and TLS 1.2", cfg, err)
	}
	cfg, err = tlsConfig(good)
	if err != nil || cfg.MinVersion != tls.VersionTLS12 || !cfg.RootCAs.Equal(want) {
		t.Fatalf("CA file = (%+v, %v); want the system roots plus the bundle and TLS 1.2", cfg, err)
	}
	if cfg, err := tlsConfig(empty); err == nil || cfg != nil {
		t.Fatalf("a bundle with no certificate = (%v, %v); want a refusal", cfg, err)
	}
	if cfg, err := tlsConfig(filepath.Join(dir, "missing.pem")); err == nil || cfg != nil {
		t.Fatalf("a missing bundle = (%v, %v); want a refusal", cfg, err)
	}
}

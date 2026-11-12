// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

// The egress trust set is the system roots plus the bundle: no bundle leaves the
// default roots (nil), and a bundle with no certificate is refused.
func TestEgressTLSConfig_Bundle(t *testing.T) {
	ca, _ := genTestCA(t)
	want, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	want.AppendCertsFromPEM(ca)

	if cfg, err := egressTLSConfig(""); cfg != nil || err != nil {
		t.Fatalf("no bundle = (%v, %v); want nil, nil", cfg, err)
	}
	cfg, err := egressTLSConfig(string(ca))
	if err != nil || !cfg.RootCAs.Equal(want) || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("bundle = (%+v, %v); want the system roots plus the bundle", cfg, err)
	}
	if cfg, err := egressTLSConfig("not a certificate"); err == nil || cfg != nil {
		t.Fatalf("a bundle with no certificate = (%v, %v); want a refusal", cfg, err)
	}
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestGenerateRunCA(t *testing.T) {
	now := time.Now()
	certPEM, keyPEM, err := generateRunCA(now)
	if err != nil {
		t.Fatalf("generateRunCA: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("cert PEM did not decode")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if !caCert.IsCA || !caCert.BasicConstraintsValid {
		t.Fatal("generated cert must be a CA with valid basic constraints")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA must have certSign key usage")
	}
	if !caCert.MaxPathLenZero {
		t.Fatal("CA must be constrained to MaxPathLen=0 (leaf-only)")
	}
	// The cert+key must form a usable TLS keypair (what the proxy loads).
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Fatalf("cert/key not a usable keypair: %v", err)
	}
	// Key PEM must be a PKCS#8 private key.
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		t.Fatal("key PEM did not decode as PKCS#8 PRIVATE KEY")
	}
}

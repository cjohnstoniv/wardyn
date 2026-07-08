// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// mitmCATTL is the validity window of a provisioned per-run MITM CA. It only
// needs to outlive the run; a year is generous and avoids clock-skew flakiness.
const mitmCATTL = 365 * 24 * time.Hour

// generateRunCA mints a fresh ECDSA P-256 CA cert+key (PEM) for TLS-MITM
// inspection of a single run. The PRIVATE key is delivered ONLY to that run's
// wardyn-proxy sidecar (in proxy memory); the sandbox trusts only the PUBLIC cert.
// A per-run CA bounds blast radius: a leaked CA can forge certs for the duration
// of one run, not the whole fleet.
func generateRunCA(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("mitm ca: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("mitm ca: serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Wardyn run egress-inspection CA", Organization: []string{"Wardyn"}},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(mitmCATTL),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true, // may only sign leaf certs, not sub-CAs
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("mitm ca: create cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("mitm ca: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package hoptls is the TLS on the hop between wardynd and each run's
// wardyn-proxy: the hop the proxy resolves credential VALUES over
// (GET /api/v1/internal/injection/{grant}), and every other internal call it
// makes (mints, token renewal, decisions, approvals, uploads).
//
// wardynd mints ONE internal CA on first boot and keeps it in its secret store
// beside its other boot keys, so it survives restarts and upgrades on every
// install shape. At each boot it signs a serving certificate for the host of
// WARDYN_CONTROL_PLANE_URL — the exact name every proxy dials, so the name and
// the certificate cannot disagree. Dispatch hands the CA's public certificate to
// each proxy in its sealed config, and the proxy trusts that certificate and
// nothing else for control-plane calls: never the system roots, never the
// operator's corporate CA bundle (which stays on the egress side).
package hoptls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

// caTTL is the internal CA's validity. rotateBefore is how much of it must
// remain for a stored CA to be kept at boot: a CA inside that window is
// replaced, and every run started under the old one fails closed on its next
// control-plane call. ponytail: rotation is replace-at-boot; a dual-CA overlap
// window if an estate ever keeps runs alive across that boundary.
const (
	caTTL        = 10 * 365 * 24 * time.Hour
	rotateBefore = 365 * 24 * time.Hour
)

// IsLocalHost reports whether host names this machine's loopback interface:
// "localhost" or a loopback IP literal. No DNS lookup, deliberately — a name
// that happens to resolve to 127.0.0.1 is a resolver's claim, not the
// operator's, and both ends of the hop must reach the same verdict.
func IsLocalHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// CheckURL is the one rule wardynd, every proxy and wardyn-tetragon-ingest
// apply to the control-plane URL. https:// always passes. http:// passes only when the host is local
// (IsLocalHost): plaintext that never leaves the loopback interface. Anything
// else is refused, with the fix in the message.
func CheckURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("control-plane URL %q is not an absolute http(s) URL", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if IsLocalHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("control-plane URL %q is plain http:// to a non-loopback host — every caller's bearer (and, for a proxy, "+
			"resolved credential values) would cross the network in cleartext. Point WARDYN_CONTROL_PLANE_URL at wardynd's internal TLS listener "+
			"(https://<host>:8443, WARDYN_INTERNAL_LISTEN); http:// is accepted only for localhost/127.0.0.1/::1", raw)
	default:
		return fmt.Errorf("control-plane URL %q: scheme must be https (or http to a loopback host)", raw)
	}
}

// ClientConfig is the proxy side: a TLS config whose ONLY root is the internal
// CA in caPEM. An empty caPEM yields an EMPTY pool, so every handshake fails —
// the fail-closed shape for a proxy that was given an https URL and no CA.
// Callers Clone it per transport; it is never shared.
func ClientConfig(caPEM string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if caPEM != "" && !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("control_plane_ca_pem does not contain a valid PEM certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, nil
}

// CA is the parsed internal CA.
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	CertPEM []byte // the public half, handed to every proxy
}

// NewCA mints a fresh internal CA and returns it as one PEM blob (certificate
// then PKCS#8 key) — the form wardynd stores.
func NewCA(now time.Time) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("internal ca: generate key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Wardyn internal CA (control plane to proxy)", Organization: []string{"Wardyn"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caTTL),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("internal ca: create cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("internal ca: marshal key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...), nil
}

// ParseCA parses NewCA's blob.
func ParseCA(blob []byte) (*CA, error) {
	ca := &CA{}
	for rest := blob; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch b.Type {
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("internal ca: parse cert: %w", err)
			}
			ca.cert, ca.CertPEM = c, pem.EncodeToMemory(b)
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("internal ca: parse key: %w", err)
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("internal ca: key is not a signer")
			}
			ca.key = s
		}
	}
	if ca.cert == nil || ca.key == nil || !ca.cert.IsCA {
		return nil, errors.New("internal ca: blob needs a CA certificate and its private key")
	}
	return ca, nil
}

// Fresh reports whether the CA is still worth keeping at boot (see rotateBefore).
func (ca *CA) Fresh(now time.Time) bool { return now.Add(rotateBefore).Before(ca.cert.NotAfter) }

// ServingCert signs a server certificate for host (a DNS name or an IP
// literal), valid until the CA itself expires. wardynd mints it at every boot
// and holds it only in memory.
func (ca *CA) ServingCert(host string, now time.Time) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("internal ca: generate serving key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     ca.cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("internal ca: sign serving cert: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err) // crypto/rand never fails on a supported platform
	}
	return n
}

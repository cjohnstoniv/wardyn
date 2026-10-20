// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package certbundle parses and checks a PEM bundle of trust anchors, the
// "system roots plus PEM" pool builder the CLI and runnerd share for their own
// connection to the organisation. It never feeds a sandbox's or a run proxy's
// trust: those keep --trusted-ca-file.
package certbundle

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxBytes is the largest bundle Parse accepts.
const MaxBytes = 64 << 10

// soon is how close to its not-after an anchor is flagged.
const soon = 30 * 24 * time.Hour

// Entry is one certificate of a bundle and what the checks found.
type Entry struct {
	Cert        *x509.Certificate
	Fingerprint string // "sha256:<hex>" of the DER
	// Problem is why the entry is not added ("not a CA certificate",
	// "expired"), or "" when it is usable.
	Problem string
	// ExpiresSoon is set when a usable entry expires within 30 days.
	ExpiresSoon bool
}

// Fingerprint is "sha256:<lowercase hex>" of c's DER.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NormalizeFingerprint lowercases fp and drops colons inside the hex, and
// refuses anything but "sha256:" and 64 hex digits.
func NormalizeFingerprint(fp string) (string, error) {
	hexPart, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(fp)), "sha256:")
	hexPart = strings.ReplaceAll(hexPart, ":", "")
	if _, err := hex.DecodeString(hexPart); !ok || err != nil || len(hexPart) != 64 {
		return "", fmt.Errorf("fingerprint %q is not sha256: followed by 64 hex digits", fp)
	}
	return "sha256:" + hexPart, nil
}

// Parse reads every CERTIFICATE block of raw and checks each one at now:
// a certificate that is not a CA, or is expired, carries a Problem. A bundle
// over MaxBytes, a block of another type, an unparseable certificate or no
// certificate at all is an error.
func Parse(raw []byte, now time.Time) ([]Entry, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("the bundle is %d bytes; the limit is %d (64 KiB)", len(raw), MaxBytes)
	}
	var out []Entry
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the bundle holds a %q block; only CERTIFICATE blocks are accepted", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", len(out)+1, err)
		}
		e := Entry{Cert: c, Fingerprint: Fingerprint(c)}
		switch {
		case !c.IsCA || !c.BasicConstraintsValid:
			e.Problem = "not a CA certificate"
		case now.After(c.NotAfter):
			e.Problem = "expired"
		case now.Before(c.NotBefore):
			e.Problem = "not yet valid"
		case c.NotAfter.Sub(now) < soon:
			e.ExpiresSoon = true
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("the bundle holds no PEM certificate")
	}
	return out, nil
}

// Usable is the entries with no Problem.
func Usable(entries []Entry) []*x509.Certificate {
	var out []*x509.Certificate
	for _, e := range entries {
		if e.Problem == "" {
			out = append(out, e.Cert)
		}
	}
	return out
}

// CheckChain refuses the whole set when an intermediate (a usable CA that is
// not self-signed) has an issuer in neither certs, extra (anchors already
// stored) nor system. system nil means no system roots.
func CheckChain(certs, extra []*x509.Certificate, system *x509.CertPool, now time.Time) error {
	for _, c := range certs {
		if selfSigned(c) {
			continue
		}
		roots := x509.NewCertPool()
		if system != nil {
			roots = system.Clone()
		}
		for _, o := range append(append([]*x509.Certificate{}, certs...), extra...) {
			if !o.Equal(c) {
				roots.AddCert(o)
			}
		}
		_, err := c.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
		if err != nil {
			return fmt.Errorf("incomplete chain: %s is issued by %s, which is in neither the bundle nor the system roots",
				c.Subject, c.Issuer)
		}
	}
	return nil
}

func selfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawSubject, c.RawIssuer) && c.CheckSignatureFrom(c) == nil
}

// Encode is certs as one PEM bundle.
func Encode(certs []*x509.Certificate) []byte {
	var b bytes.Buffer
	for _, c := range certs {
		_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	return b.Bytes()
}

// Pool is the system roots plus anchors. When the system pool cannot be read
// the anchors stand alone, as Go itself does for an empty system store.
func Pool(anchors []*x509.Certificate) *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, c := range anchors {
		pool.AddCert(c)
	}
	return pool
}

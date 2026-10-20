// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package certbundle

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// mint is a certificate named cn, signed by parent (self-signed when nil).
func mint(t *testing.T, cn string, isCA bool, notAfter time.Time, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: notAfter,
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func TestBundle_NamesLeafExpiredAndSoon(t *testing.T) {
	year := now.Add(365 * 24 * time.Hour)
	root, rootKey := mint(t, "Root", true, year, nil, nil)
	leaf, _ := mint(t, "Leaf", false, year, root, rootKey)
	old, _ := mint(t, "Old", true, now.Add(-time.Hour), nil, nil)
	soonCA, _ := mint(t, "Soon", true, now.Add(10*24*time.Hour), nil, nil)

	entries, err := Parse(Encode([]*x509.Certificate{root, leaf, old, soonCA}), now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.Cert.Subject.CommonName] = e
	}
	if got["Root"].Problem != "" || got["Root"].ExpiresSoon {
		t.Errorf("Root: %+v, want usable", got["Root"])
	}
	if got["Leaf"].Problem != "not a CA certificate" {
		t.Errorf("Leaf problem = %q", got["Leaf"].Problem)
	}
	if got["Old"].Problem != "expired" {
		t.Errorf("Old problem = %q", got["Old"].Problem)
	}
	if !got["Soon"].ExpiresSoon || got["Soon"].Problem != "" {
		t.Errorf("Soon: %+v, want usable and flagged", got["Soon"])
	}
	if u := Usable(entries); len(u) != 2 {
		t.Errorf("Usable = %d certs, want Root and Soon", len(u))
	}
	if !strings.HasPrefix(got["Root"].Fingerprint, "sha256:") || len(got["Root"].Fingerprint) != 71 {
		t.Errorf("fingerprint %q", got["Root"].Fingerprint)
	}
}

func TestBundle_IncompleteChainRefuses(t *testing.T) {
	year := now.Add(365 * 24 * time.Hour)
	root, rootKey := mint(t, "Root", true, year, nil, nil)
	inter, _ := mint(t, "Intermediate", true, year, root, rootKey)

	err := CheckChain([]*x509.Certificate{inter}, nil, x509.NewCertPool(), now)
	if err == nil || !strings.Contains(err.Error(), "incomplete chain") || !strings.Contains(err.Error(), "CN=Root") {
		t.Fatalf("intermediate alone: %v, want an incomplete chain naming its issuer", err)
	}
	if err := CheckChain([]*x509.Certificate{root, inter}, nil, x509.NewCertPool(), now); err != nil {
		t.Fatalf("root in the bundle: %v", err)
	}
	if err := CheckChain([]*x509.Certificate{inter}, []*x509.Certificate{root}, nil, now); err != nil {
		t.Fatalf("root already stored: %v", err)
	}
	system := x509.NewCertPool()
	system.AddCert(root)
	if err := CheckChain([]*x509.Certificate{inter}, nil, system, now); err != nil {
		t.Fatalf("root in the system roots: %v", err)
	}
}

func TestBundle_RefusesOversizeAndOtherBlocks(t *testing.T) {
	if _, err := Parse(make([]byte, MaxBytes+1), now); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Errorf("oversize: %v", err)
	}
	if _, err := Parse([]byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), now); err == nil {
		t.Error("a key block was accepted")
	}
	if _, err := Parse([]byte("not pem"), now); err == nil {
		t.Error("non-PEM was accepted")
	}
}

func TestBundle_NormalizeFingerprint(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	for _, in := range []string{"sha256:" + hex, "SHA256:" + strings.ToUpper(hex), "sha256:" + strings.Repeat("ab:", 31) + "ab"} {
		if got, err := NormalizeFingerprint(in); err != nil || got != "sha256:"+hex {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	for _, in := range []string{hex, "sha1:" + hex, "sha256:" + hex[:62], "sha256:" + strings.Repeat("zz", 32)} {
		if _, err := NormalizeFingerprint(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// TestAnchorFileMode: 0400 in a 0700 directory passes; any write bit on the
// file, or a group- or other-writable directory, is refused, naming the file
// and the mode.
func TestAnchorFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "ca")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "org_443.pem")
	if err := os.WriteFile(f, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(f); err != nil {
		t.Fatalf("0400 in 0700: %v", err)
	}
	for _, m := range []os.FileMode{0o600, 0o420, 0o402} {
		if err := os.Chmod(f, m); err != nil {
			t.Fatal(err)
		}
		err := CheckFile(f)
		if err == nil || !strings.Contains(err.Error(), f) || !strings.Contains(err.Error(), "write bit") {
			t.Errorf("file mode %04o: %v, want a refusal naming the file", m, err)
		}
	}
	if err := os.Chmod(f, 0o400); err != nil {
		t.Fatal(err)
	}
	for _, m := range []os.FileMode{0o770, 0o702} {
		if err := os.Chmod(dir, m); err != nil {
			t.Fatal(err)
		}
		if err := CheckFile(f); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("dir mode %04o: %v, want a refusal", m, err)
		}
	}
	_ = os.Chmod(dir, 0o700)
}

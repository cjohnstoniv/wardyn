// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

const testRunSPIFFEID = "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111"

func testAuthority(t *testing.T, raw []byte, now time.Time) *Authority {
	t.Helper()
	a, err := NewAuthority(raw)
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	a.now = func() time.Time { return now }
	return a
}

func testRotateAuthority(t *testing.T, raw []byte, drop bool) []byte {
	t.Helper()
	next, err := RotateAuthority(raw, drop, testRotatedAt)
	if err != nil {
		t.Fatalf("RotateAuthority: %v", err)
	}
	return next
}

func testClientKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testIssue(t *testing.T, a *Authority, lifetime time.Duration) *x509.Certificate {
	t.Helper()
	out, _, err := a.Issue(&testClientKey(t).PublicKey, testRunSPIFFEID, lifetime)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	block, rest := pem.Decode(out)
	if block == nil || len(rest) != 0 {
		t.Fatalf("Issue returned %q, want one PEM block", out)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// testVerifyLeaf checks leaf as a SPIFFE-aware gateway would, against the published bundle.
func testVerifyLeaf(t *testing.T, a *Authority, leaf *x509.Certificate, at time.Time) error {
	t.Helper()
	td := spiffeid.RequireTrustDomainFromString("wardyn.local")
	bundle, err := x509bundle.Parse(td, a.BundlePEM())
	if err != nil {
		t.Fatalf("parse the published bundle: %v", err)
	}
	id, _, err := x509svid.Verify([]*x509.Certificate{leaf}, bundle, x509svid.WithTime(at))
	if err == nil && id.String() != testRunSPIFFEID {
		t.Fatalf("verified as %s, want %s", id, testRunSPIFFEID)
	}
	return err
}

// A leaf is an X509-SVID naming the run, for client auth only, and verifies against the
// published bundle both as SPIFFE and as a plain TLS server checks a client certificate.
func TestAuthority_IssueIsAClientX509SVID(t *testing.T) {
	a := testAuthority(t, testRotateAuthority(t, nil, false), testRotatedAt)
	leaf := testIssue(t, a, 2*time.Minute)
	if err := testVerifyLeaf(t, a, leaf, testRotatedAt); err != nil {
		t.Fatalf("x509svid.Verify: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(a.BundlePEM()) {
		t.Fatal("the bundle holds no certificate")
	}
	opts := x509.VerifyOptions{Roots: pool, CurrentTime: testRotatedAt, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if _, err := leaf.Verify(opts); err != nil {
		t.Fatalf("verify for client auth: %v", err)
	}
	opts.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if _, err := leaf.Verify(opts); err == nil {
		t.Error("the leaf verifies for server auth")
	}
	if leaf.IsCA || len(leaf.URIs) != 1 || len(leaf.DNSNames) != 0 {
		t.Errorf("leaf IsCA=%v URIs=%v DNSNames=%v", leaf.IsCA, leaf.URIs, leaf.DNSNames)
	}
	if want := testRotatedAt.Add(2 * time.Minute); !leaf.NotAfter.Equal(want) {
		t.Errorf("NotAfter %v, want %v", leaf.NotAfter, want)
	}
	if err := testVerifyLeaf(t, a, leaf, testRotatedAt.Add(3*time.Minute)); err == nil {
		t.Error("an expired leaf verified")
	}
}

// A leaf never outlives the longest token: zero, negative and too-long lifetimes all get ten minutes.
func TestAuthority_IssueCapsLifetime(t *testing.T) {
	a := testAuthority(t, testRotateAuthority(t, nil, false), testRotatedAt)
	for _, life := range []time.Duration{0, -time.Second, time.Hour} {
		if got, want := testIssue(t, a, life).NotAfter, testRotatedAt.Add(tokenLifetime); !got.Equal(want) {
			t.Errorf("lifetime %v: NotAfter %v, want %v", life, got, want)
		}
	}
}

func TestAuthority_IssueRefuses(t *testing.T) {
	a := testAuthority(t, testRotateAuthority(t, nil, false), testRotatedAt)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"empty id": func() error { _, _, err := a.Issue(&testClientKey(t).PublicKey, "", 0); return err },
		"not spiffe": func() error {
			_, _, err := a.Issue(&testClientKey(t).PublicKey, "https://wardyn.local/agent-run/x", 0)
			return err
		},
		"nil key":   func() error { _, _, err := a.Issue(nil, testRunSPIFFEID, 0); return err },
		"P-384 key": func() error { _, _, err := a.Issue(&p384.PublicKey, testRunSPIFFEID, 0); return err },
	} {
		if call() == nil {
			t.Errorf("%s: issued", name)
		}
	}
}

// A rotation keeps the replaced CA published until its last leaf has expired plus the overlap,
// so a leaf signed just before the rotation still verifies; -retire-previous drops it at once.
func TestRotateAuthority_PreviousPublishedUntilRetire(t *testing.T) {
	first := testRotateAuthority(t, nil, false)
	old := testAuthority(t, first, testRotatedAt)
	leaf := testIssue(t, old, 0)

	next := testAuthority(t, testRotateAuthority(t, first, false), testRotatedAt)
	if next.Serial() == old.Serial() {
		t.Fatal("the rotation kept the serial")
	}
	if want := testRotatedAt.Add(tokenLifetime + retireOverlap); !next.RetireAt().Equal(want) {
		t.Fatalf("RetireAt %v, want %v", next.RetireAt(), want)
	}
	if err := testVerifyLeaf(t, next, leaf, testRotatedAt.Add(time.Minute)); err != nil {
		t.Fatalf("a leaf of the replaced CA no longer verifies: %v", err)
	}
	next.now = func() time.Time { return testRotatedAt.Add(tokenLifetime + retireOverlap) }
	if n := len(pemCerts(t, next.BundlePEM())); n != 1 {
		t.Fatalf("after retire the bundle has %d certificates, want 1", n)
	}

	dropped := testAuthority(t, testRotateAuthority(t, first, true), testRotatedAt)
	if !dropped.RetireAt().IsZero() || len(pemCerts(t, dropped.BundlePEM())) != 1 {
		t.Fatal("-retire-previous kept the replaced CA")
	}
	if err := testVerifyLeaf(t, dropped, leaf, testRotatedAt.Add(time.Minute)); err == nil {
		t.Fatal("a leaf of a dropped CA verified")
	}
}

func pemCerts(t *testing.T, b []byte) []*pem.Block {
	t.Helper()
	var out []*pem.Block
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			return out
		}
		out = append(out, block)
	}
}

// A row that cannot be read is an error, never a new CA.
func TestNewAuthority_RefusesUnreadableRows(t *testing.T) {
	good := testRotateAuthority(t, nil, false)
	other := testRotateAuthority(t, nil, false)
	var g, o storedAuthority
	if json.Unmarshal(good, &g) != nil || json.Unmarshal(other, &o) != nil {
		t.Fatal("unmarshal")
	}
	mixed, _ := json.Marshal(storedAuthority{Key: g.Key, Cert: o.Cert})
	noRetire, _ := json.Marshal(storedAuthority{Key: g.Key, Cert: g.Cert, Previous: &storedRetiredCert{Cert: o.Cert}})
	signerRow := testRotate(t, nil, false)
	for name, raw := range map[string][]byte{
		"not json":            []byte("x"),
		"empty":               []byte("{}"),
		"key of another CA":   mixed,
		"previous, no retire": noRetire,
		"a token signing row": signerRow,
	} {
		if _, err := NewAuthority(raw); err == nil {
			t.Errorf("%s: NewAuthority accepted it", name)
		}
		if _, err := RotateAuthority(raw, false, testRotatedAt); err == nil && name != "previous, no retire" {
			t.Errorf("%s: RotateAuthority accepted it", name)
		}
	}
}

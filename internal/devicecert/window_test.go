// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package devicecert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

var windowAt = time.Unix(1900000000, 0).UTC()

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// issue makes a certificate named cn, signed by parent (self-signed when nil), valid from windowAt+from to windowAt+to.
func issue(t *testing.T, cn string, parent *testCert, ca bool, from, to time.Duration) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: windowAt.Add(from), NotAfter: windowAt.Add(to),
		BasicConstraintsValid: true, IsCA: ca, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if ca {
		tpl.KeyUsage = x509.KeyUsageCertSign
	}
	signer, signKey := tpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCert{c, key}
}

const day = 24 * time.Hour

// Each chain is real and goes through Verify, which refuses it without saying which certificate is at fault
// (a CA outside its window is only an unknown authority); OutsideWindow names it.
func TestOutsideWindow_NamesTheCertificateRealChainsFailOn(t *testing.T) {
	for name, c := range map[string]struct {
		rootFrom, rootTo   time.Duration
		interFrom, interTo time.Duration
		leafFrom, leafTo   time.Duration
		wantBad            string // "" when the chain is good
		wantNotYet         bool
	}{
		"a good chain":                       {-9 * day, 9 * day, -8 * day, 8 * day, -day, day, "", false},
		"an expired root":                    {-9 * day, -day, -8 * day, 8 * day, -day / 2, day, "root", false},
		"a root that is not yet valid":       {2 * day, 9 * day, -8 * day, 8 * day, -day / 2, day, "root", true},
		"an expired intermediate":            {-9 * day, 9 * day, -8 * day, -day, -day / 2, day, "inter", false},
		"an intermediate not yet valid":      {-9 * day, 9 * day, 2 * day, 8 * day, -day / 2, day, "inter", true},
		"a good chain with an expired leaf":  {-9 * day, 9 * day, -8 * day, 8 * day, -3 * day, -day, "leaf", false},
		"a good chain with a leaf not yet":   {-9 * day, 9 * day, -8 * day, 8 * day, day, 3 * day, "leaf", true},
		"the leaf is named before a late CA": {-9 * day, -day, -8 * day, 8 * day, -3 * day, -day, "leaf", false},
	} {
		t.Run(name, func(t *testing.T) {
			root := issue(t, "Root", nil, true, c.rootFrom, c.rootTo)
			inter := issue(t, "Inter", &root, true, c.interFrom, c.interTo)
			leaf := issue(t, "Device", &inter, false, c.leafFrom, c.leafTo)
			pool, ipool := x509.NewCertPool(), x509.NewCertPool()
			pool.AddCert(root.cert)
			ipool.AddCert(inter.cert)
			_, err := leaf.cert.Verify(x509.VerifyOptions{Roots: pool, Intermediates: ipool, CurrentTime: windowAt,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			bad, notYet := OutsideWindow(windowAt, leaf.cert, []*x509.Certificate{inter.cert}, []*x509.Certificate{root.cert})
			if c.wantBad == "" {
				if err != nil || bad != nil {
					t.Fatalf("a good chain: Verify %v, named %v", err, bad)
				}
				return
			}
			if err == nil {
				t.Fatal("Verify accepted a chain with a certificate outside its window")
			}
			want := map[string]*x509.Certificate{"root": root.cert, "inter": inter.cert, "leaf": leaf.cert}[c.wantBad]
			if bad != want || notYet != c.wantNotYet {
				t.Fatalf("named %v (not yet %v), want %v (not yet %v)", bad, notYet, want.Subject, c.wantNotYet)
			}
		})
	}
}

// A configured CA that did not issue the chain is not blamed, however stale it is.
func TestOutsideWindow_IgnoresAConfiguredCANoCertificateNames(t *testing.T) {
	root := issue(t, "Root", nil, true, -9*day, 9*day)
	stale := issue(t, "Old Root", nil, true, -90*day, -60*day)
	leaf := issue(t, "Device", &root, false, -day, day)
	if bad, _ := OutsideWindow(windowAt, leaf.cert, nil, []*x509.Certificate{stale.cert, root.cert}); bad != nil {
		t.Fatalf("named %v, want none", bad.Subject)
	}
}

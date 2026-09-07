// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// seedExpiredLeaf mints a leaf for host with the SAME template leafFor uses but
// dated 26h in the past (so it is outside the 25h window a live mint gives) and
// puts it in the cache, standing in for a sidecar that has simply been up for a
// day.
func seedExpiredLeaf(t *testing.T, a *certAuthority, host string) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	minted := time.Now().Add(-26 * time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    minted.Add(-1 * time.Hour),
		NotAfter:     minted.Add(leafCertTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.caCert, &key.PublicKey, a.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, a.caCert.Raw}, PrivateKey: key, Leaf: leaf}
	a.mu.Lock()
	a.leaves[host] = cert
	a.mu.Unlock()
	return cert
}

// TestLeafForRemintsAnExpiredCachedLeaf pins F078: the per-host leaf cache must
// consult NotAfter.
//
// leafFor cached a minted leaf and returned it on every later call with no
// expiry check, on an assumption stated in the file and never implemented ("the
// cert is regenerated whenever the proxy restarts"). A per-run proxy sidecar has
// no lifetime bound — the idle reaper is skipped entirely when the run's
// AutoStopAfterSec <= 0, which is the default, and the per-run MITM CA is minted
// for a YEAR because runs are expected to outlive a day. Past ~25h of uptime
// every NEW CONNECT to a MITM'd host got "200 Connection Established" followed
// by a TLS handshake the sandbox rejects with "certificate has expired",
// silently (mitmConnect's handshake error path emits nothing) and permanently.
func TestLeafForRemintsAnExpiredCachedLeaf(t *testing.T) {
	const host = "api.anthropic.com"
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	stale := seedExpiredLeaf(t, ca, host)

	got, err := ca.leafFor(host)
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}
	if got == stale {
		t.Fatalf("leafFor returned the CACHED leaf (NotAfter=%s, expired %s ago) instead of re-minting: "+
			"a sidecar that has been up longer than leafCertTTL fails every MITM handshake, permanently",
			stale.Leaf.NotAfter, time.Since(stale.Leaf.NotAfter).Truncate(time.Minute))
	}
	if got.Leaf == nil {
		t.Fatal("re-minted leaf has no parsed Leaf")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add Wardyn CA to the agent trust pool")
	}
	if _, verr := got.Leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     host,
		CurrentTime: time.Now(),
	}); verr != nil {
		t.Fatalf("the leaf leafFor hands a live handshake must be valid NOW: %v", verr)
	}

	// A leaf minted moments ago is still SERVED FROM CACHE — the fix must not
	// turn every handshake into a fresh keygen+sign, which is the DoS shape
	// mitmConnect's "mint for the validated CONNECT host" comment guards against.
	again, err := ca.leafFor(host)
	if err != nil {
		t.Fatalf("leafFor (second call): %v", err)
	}
	if again != got {
		t.Fatal("a fresh leaf must still be cached and reused across calls")
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package releasesigtest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"time"

	"github.com/digitorus/timestamp"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// AnnotationRFC3161Timestamp is where `cosign sign --timestamp-server-url` puts the TSA's response.
const AnnotationRFC3161Timestamp = "dev.sigstore.cosign/rfc3161timestamp"

// timestampAuthority is a test RFC 3161 TSA: a root and the leaf that signs timestamps.
type timestampAuthority struct {
	key        *ecdsa.PrivateKey
	root, leaf *x509.Certificate
}

var oidExtKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37}

func (s *Signer) newTimestampAuthority() *timestampAuthority {
	// The root names no extended key usage: chain verification asks every certificate for time stamping.
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	rootTmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test tsa root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	s.check(err)
	rootCert, err := x509.ParseCertificate(rootDER)
	s.check(err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	// RFC 3161 2.3: the leaf's only extended key usage is time stamping, and the extension is critical.
	eku, err := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}})
	s.check(err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano() + 1), Subject: pkix.Name{CommonName: "test tsa"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtraExtensions: []pkix.Extension{{Id: oidExtKeyUsage, Critical: true, Value: eku}}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, &key.PublicKey, rootKey)
	s.check(err)
	leaf, err := x509.ParseCertificate(der)
	s.check(err)
	return &timestampAuthority{key: key, root: rootCert, leaf: leaf}
}

// WithOtherTSA is s with a TSA of its own, which none of s's trust roots names.
func (s *Signer) WithOtherTSA() *Signer {
	c := *s
	c.tsa = s.newTimestampAuthority()
	return &c
}

// TrustedRootWithTSA is s's Fulcio and CT log with its TSA, and its Rekor only when rekor is true: a private
// Sigstore that timestamps signatures instead of logging them.
func (s *Signer) TrustedRootWithTSA(rekor bool) *root.TrustedRoot {
	s.t.Helper()
	logs := s.trust.RekorLogs()
	if !rekor {
		logs = nil
	}
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01, s.trust.FulcioCertificateAuthorities(), s.trust.CTLogs(),
		[]root.TimestampingAuthority{&root.SigstoreTimestampingAuthority{Root: s.tsa.root, Leaf: s.tsa.leaf, URI: "https://tsa.test",
			ValidityPeriodStart: time.Now().Add(-time.Hour)}}, logs)
	s.check(err)
	return tr
}

// addTimestamp adds the TSA's timestamp of l's raw signature bytes, as cosign does.
func (s *Signer) addTimestamp(l LegacyLayer) {
	sig, err := base64.StdEncoding.DecodeString(l.Annotations[AnnotationSignature])
	s.check(err)
	sum := sha256.Sum256(sig)
	ts := timestamp.Timestamp{HashAlgorithm: crypto.SHA256, HashedMessage: sum[:], Time: time.Now(),
		Policy: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 2}, AddTSACertificate: true}
	resp, err := ts.CreateResponseWithOpts(s.tsa.leaf, s.tsa.key, crypto.SHA256)
	s.check(err)
	b, err := json.Marshal(struct{ SignedRFC3161Timestamp []byte }{resp})
	s.check(err)
	l.Annotations[AnnotationRFC3161Timestamp] = string(b)
}

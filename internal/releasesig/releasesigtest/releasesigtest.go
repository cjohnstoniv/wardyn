// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package releasesigtest signs a SHA256SUMS the way the release workflow does, with a test Fulcio and a test
// Rekor, so tests verify real bundles without calling Fulcio or Rekor. Tests only. It does not use
// sigstore-go's pkg/testing/ca, which links an MPL-2.0 module that licenses/ALLOWED-LICENSES.txt refuses.
package releasesigtest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/rekor/pkg/util"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/signature"
	"google.golang.org/protobuf/encoding/protojson"
)

// Signer is a test Fulcio (a root and an intermediate) beside a test Rekor holding one-entry trees.
type Signer struct {
	t        *testing.T
	trust    *root.TrustedRoot
	inter    *x509.Certificate
	interKey *ecdsa.PrivateKey
	rekorKey *ecdsa.PrivateKey
	rekorID  string // hex SHA-256 of the Rekor key, as Rekor names its log
}

// Fulcio's issuer (v2) and source repository URI extensions.
var (
	oidIssuer     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidRepository = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}
)

// New makes a Signer.
func New(t *testing.T) *Signer {
	t.Helper()
	s := &Signer{t: t}
	rootKey, rootCert := s.ca(nil, nil)
	interKey, inter := s.ca(rootCert, rootKey)
	rekorKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	der, err := x509.MarshalPKIXPublicKey(rekorKey.Public())
	s.check(err)
	id := sha256.Sum256(der)
	s.inter, s.interKey, s.rekorKey, s.rekorID = inter, interKey, rekorKey, hex.EncodeToString(id[:])
	s.trust, err = root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{&root.FulcioCertificateAuthority{Root: rootCert, Intermediates: []*x509.Certificate{inter}, URI: "https://fulcio.test"}},
		nil, nil, map[string]*root.TransparencyLog{s.rekorID: {BaseURL: "https://rekor.localhost", ID: []byte(s.rekorID),
			ValidityPeriodStart: time.Now().Add(-time.Hour), HashFunc: crypto.SHA256, PublicKey: rekorKey.Public(), SignatureHashFunc: crypto.SHA256}})
	s.check(err)
	return s
}

// ca is a CA certificate signed by parent (self-signed when parent is nil), and its key.
func (s *Signer) ca(parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, *x509.Certificate) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test fulcio"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	s.check(err)
	cert, err := x509.ParseCertificate(der)
	s.check(err)
	return key, cert
}

// TrustedMaterial is what VerifySums is to trust for this Signer's bundles.
func (s *Signer) TrustedMaterial() root.TrustedMaterial { return s.trust }

// Options shape one bundle.
type Options struct {
	Identity, Issuer, Repository string
	NoTlog                       bool // leave the transparency-log entry out
}

// Sign is the SHA256SUMS.sigstore.json the release workflow would publish for sums, signed as o says.
func (s *Signer) Sign(sums []byte, o Options) []byte {
	s.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	san, err := url.Parse(o.Identity)
	s.check(err)
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), URIs: []*url.URL{san},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		ExtraExtensions: []pkix.Extension{s.derString(oidIssuer, o.Issuer), s.derString(oidRepository, o.Repository)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.inter, &key.PublicKey, s.interKey)
	s.check(err)
	digest := sha256.Sum256(sums)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	s.check(err)
	pb := &protobundle.Bundle{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &protobundle.VerificationMaterial{Content: &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: der}}},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest[:]}, Signature: sig}}}
	if !o.NoTlog {
		pb.VerificationMaterial.TlogEntries = []*protorekor.TransparencyLogEntry{s.tlogEntry(der, digest[:], sig, now.Unix())}
	}
	out, err := protojson.Marshal(pb)
	s.check(err)
	return out
}

// tlogEntry is a hashedrekord entry for the signature, with the test Rekor's promise and inclusion proof.
func (s *Signer) tlogEntry(certDER, digest, sig []byte, integrated int64) *protorekor.TransparencyLogEntry {
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	body, err := json.Marshal(map[string]any{"apiVersion": "0.0.1", "kind": "hashedrekord", "spec": map[string]any{
		"data":      map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(digest)}},
		"signature": map[string]any{"content": base64.StdEncoding.EncodeToString(sig), "publicKey": map[string]any{"content": base64.StdEncoding.EncodeToString(certPEM)}}}})
	s.check(err)
	rawID, err := hex.DecodeString(s.rekorID)
	s.check(err)
	// The promise: Rekor's signature over the canonical JSON of the entry (json.Marshal sorts the keys).
	const index = 1000
	payload, err := json.Marshal(map[string]any{"body": base64.StdEncoding.EncodeToString(body), "integratedTime": integrated, "logID": s.rekorID, "logIndex": index})
	s.check(err)
	signer, err := signature.LoadECDSASignerVerifier(s.rekorKey, crypto.SHA256)
	s.check(err)
	set, err := signer.SignMessage(bytes.NewReader(payload))
	s.check(err)
	// The proof: a tree of this one entry, whose root is the leaf hash, under a checkpoint Rekor signed.
	leaf := sha256.Sum256(append([]byte{0}, body...))
	checkpoint, err := util.CreateAndSignCheckpoint(context.Background(), "rekor.localhost", 123, 1, leaf[:], signer)
	s.check(err)
	return &protorekor.TransparencyLogEntry{LogIndex: index, LogId: &protocommon.LogId{KeyId: rawID},
		KindVersion: &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}, IntegratedTime: integrated,
		InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
		InclusionProof:    &protorekor.InclusionProof{LogIndex: 0, RootHash: leaf[:], TreeSize: 1, Checkpoint: &protorekor.Checkpoint{Envelope: string(checkpoint)}},
		CanonicalizedBody: body}
}

func (s *Signer) derString(oid asn1.ObjectIdentifier, v string) pkix.Extension {
	b, err := asn1.MarshalWithParams(v, "utf8")
	s.check(err)
	return pkix.Extension{Id: oid, Value: b}
}

func (s *Signer) check(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

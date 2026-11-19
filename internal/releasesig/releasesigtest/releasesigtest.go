// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package releasesigtest signs a SHA256SUMS the way the release workflow does, and in-toto statements the way
// cosign and GitHub attestations do, with a test Fulcio, CT log and Rekor, so tests verify real bundles
// without calling Fulcio or Rekor. Tests only. It does not use
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
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/rekor/pkg/util"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/signature"
	"google.golang.org/protobuf/encoding/protojson"
)

// Signer is a test Fulcio (a root and an intermediate) beside a test CT log and a test Rekor holding
// one-entry trees.
type Signer struct {
	t        *testing.T
	trust    *root.TrustedRoot
	inter    *x509.Certificate
	interKey *ecdsa.PrivateKey
	rekorKey *ecdsa.PrivateKey
	rekorID  string // hex SHA-256 of the Rekor key, as Rekor names its log
	ctKey    *ecdsa.PrivateKey
	ctID     [32]byte // SHA-256 of the CT log key, as CT names its log
	tsa      *timestampAuthority
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
	s.ctKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	ctDER, err := x509.MarshalPKIXPublicKey(s.ctKey.Public())
	s.check(err)
	s.ctID = sha256.Sum256(ctDER)
	s.tsa = s.newTimestampAuthority()
	ctHex := hex.EncodeToString(s.ctID[:])
	s.trust, err = root.NewTrustedRoot(root.TrustedRootMediaType01,
		[]root.CertificateAuthority{&root.FulcioCertificateAuthority{Root: rootCert, Intermediates: []*x509.Certificate{inter}, URI: "https://fulcio.test"}},
		map[string]*root.TransparencyLog{ctHex: {BaseURL: "https://ctfe.test", ID: s.ctID[:], ValidityPeriodStart: time.Now().Add(-time.Hour),
			HashFunc: crypto.SHA256, PublicKey: s.ctKey.Public(), SignatureHashFunc: crypto.SHA256}},
		nil, map[string]*root.TransparencyLog{s.rekorID: {BaseURL: "https://rekor.localhost", ID: []byte(s.rekorID),
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
	NoSCT                        bool // leave the certificate's signed certificate timestamp out
	RFC3161                      bool // legacy signatures: add the Signer's RFC 3161 timestamp of the signature
}

// Sign is the SHA256SUMS.sigstore.json the release workflow would publish for sums, signed as o says.
func (s *Signer) Sign(sums []byte, o Options) []byte {
	s.t.Helper()
	key, der := s.leaf(o)
	digest := sha256.Sum256(sums)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	s.check(err)
	pb := &protobundle.Bundle{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &protobundle.VerificationMaterial{Content: &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: der}}},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: digest[:]}, Signature: sig}}}
	if !o.NoTlog {
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		pb.VerificationMaterial.TlogEntries = []*protorekor.TransparencyLogEntry{s.tlogEntry(certPEM, digest[:], sig, time.Now().Unix())}
	}
	out, err := protojson.Marshal(pb)
	s.check(err)
	return out
}

// SignDSSE is a bundle holding a DSSE envelope of the in-toto statement, signed keyless as o says, the way
// `cosign attest --new-bundle-format` or actions/attest-build-provenance publishes one.
func (s *Signer) SignDSSE(statement []byte, o Options) []byte {
	s.t.Helper()
	key, der := s.leaf(o)
	material := &protobundle.VerificationMaterial{Content: &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: der}}}
	return s.dsseBundle(key, statement, material, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), o.NoTlog)
}

// Key is a long-lived signing key; this Signer's Rekor logs its bundles.
type Key struct {
	s   *Signer
	key *ecdsa.PrivateKey
	der []byte // the public key, DER SubjectPublicKeyInfo
}

// NewKey makes a P-256 signing key.
func (s *Signer) NewKey() *Key {
	s.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s.check(err)
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	s.check(err)
	return &Key{s: s, key: key, der: der}
}

// PublicKeyPEM is the key's public half, PEM-encoded.
func (k *Key) PublicKeyPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.der}))
}

// Hint is the key hint cosign writes into a key-signed bundle: base64 of the SHA-256 of the DER public key.
func (k *Key) Hint() string {
	sum := sha256.Sum256(k.der)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// SignDSSE is a key-signed bundle holding a DSSE envelope of the statement. Only o.NoTlog applies.
func (k *Key) SignDSSE(statement []byte, o Options) []byte {
	k.s.t.Helper()
	material := &protobundle.VerificationMaterial{Content: &protobundle.VerificationMaterial_PublicKey{PublicKey: &protocommon.PublicKeyIdentifier{Hint: k.Hint()}}}
	return k.s.dsseBundle(k.key, statement, material, []byte(k.PublicKeyPEM()), o.NoTlog)
}

// leaf is a fresh key and its Fulcio certificate for o's identity, with the CT log's SCT unless o.NoSCT.
func (s *Signer) leaf(o Options) (*ecdsa.PrivateKey, []byte) {
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
	if !o.NoSCT {
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, s.sct(der))
		der, err = x509.CreateCertificate(rand.Reader, tmpl, s.inter, &key.PublicKey, s.interKey)
		s.check(err)
	}
	return key, der
}

// dsseBundle signs statement as an in-toto DSSE envelope with key and wraps it in a v0.3 bundle carrying
// material, with a dsse transparency-log entry naming verifierPEM unless noTlog.
func (s *Signer) dsseBundle(key *ecdsa.PrivateKey, statement []byte, material *protobundle.VerificationMaterial, verifierPEM []byte, noTlog bool) []byte {
	const payloadType = "application/vnd.in-toto+json"
	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(statement), statement)
	digest := sha256.Sum256([]byte(pae))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	s.check(err)
	env := &protodsse.Envelope{Payload: statement, PayloadType: payloadType, Signatures: []*protodsse.Signature{{Sig: sig}}}
	if !noTlog {
		material.TlogEntries = []*protorekor.TransparencyLogEntry{s.dsseEntry(env, verifierPEM, time.Now().Unix())}
	}
	out, err := protojson.Marshal(&protobundle.Bundle{MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: material, Content: &protobundle.Bundle_DsseEnvelope{DsseEnvelope: env}})
	s.check(err)
	return out
}

// sct is the embedded SCT-list extension for the certificate pre, as the test CT log would sign it: pre is the
// same certificate without the extension, which is the precertificate the log signs over.
func (s *Signer) sct(pre []byte) pkix.Extension {
	leaf, err := x509.ParseCertificate(pre)
	s.check(err)
	ts := uint64(time.Now().UnixMilli())
	entry := &ct.MerkleTreeLeaf{Version: ct.V1, LeafType: ct.TimestampedEntryLeafType, TimestampedEntry: &ct.TimestampedEntry{
		EntryType: ct.PrecertLogEntryType, Timestamp: ts,
		PrecertEntry: &ct.PreCert{IssuerKeyHash: sha256.Sum256(s.inter.RawSubjectPublicKeyInfo), TBSCertificate: leaf.RawTBSCertificate}}}
	sct := ct.SignedCertificateTimestamp{SCTVersion: ct.V1, LogID: ct.LogID{KeyID: s.ctID}, Timestamp: ts}
	input, err := ct.SerializeSCTSignatureInput(sct, ct.LogEntry{Leaf: *entry})
	s.check(err)
	digest := sha256.Sum256(input)
	sig, err := ecdsa.SignASN1(rand.Reader, s.ctKey, digest[:])
	s.check(err)
	sct.Signature = ct.DigitallySigned{Algorithm: cttls.SignatureAndHashAlgorithm{Hash: cttls.SHA256, Signature: cttls.ECDSA}, Signature: sig}
	one, err := cttls.Marshal(sct)
	s.check(err)
	list, err := cttls.Marshal(ctx509.SignedCertificateTimestampList{SCTList: []ctx509.SerializedSCT{{Val: one}}})
	s.check(err)
	value, err := asn1.Marshal(list)
	s.check(err)
	return pkix.Extension{Id: asn1.ObjectIdentifier(ctx509.OIDExtensionCTSCT), Value: value}
}

// tlogEntry is a hashedrekord entry for the signature, signed by the holder of verifierPEM (a certificate or a
// public key), with the test Rekor's promise and inclusion proof.
func (s *Signer) tlogEntry(verifierPEM, digest, sig []byte, integrated int64) *protorekor.TransparencyLogEntry {
	body, err := json.Marshal(map[string]any{"apiVersion": "0.0.1", "kind": "hashedrekord", "spec": map[string]any{
		"data":      map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(digest)}},
		"signature": map[string]any{"content": base64.StdEncoding.EncodeToString(sig), "publicKey": map[string]any{"content": base64.StdEncoding.EncodeToString(verifierPEM)}}}})
	s.check(err)
	return s.logged("hashedrekord", body, integrated)
}

// dsseEntry is a dsse entry for the envelope, signed by the holder of verifierPEM (a certificate or a public key).
func (s *Signer) dsseEntry(env *protodsse.Envelope, verifierPEM []byte, integrated int64) *protorekor.TransparencyLogEntry {
	envJSON, err := protojson.Marshal(env)
	s.check(err)
	envHash, payloadHash := sha256.Sum256(envJSON), sha256.Sum256(env.Payload)
	body, err := json.Marshal(map[string]any{"apiVersion": "0.0.1", "kind": "dsse", "spec": map[string]any{
		"envelopeHash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(envHash[:])},
		"payloadHash":  map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(payloadHash[:])},
		"signatures": []any{map[string]any{"signature": base64.StdEncoding.EncodeToString(env.Signatures[0].Sig),
			"verifier": base64.StdEncoding.EncodeToString(verifierPEM)}}}})
	s.check(err)
	return s.logged("dsse", body, integrated)
}

// logged is the test Rekor's entry for body: its promise and an inclusion proof in a one-entry tree.
func (s *Signer) logged(kind string, body []byte, integrated int64) *protorekor.TransparencyLogEntry {
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
		KindVersion: &protorekor.KindVersion{Kind: kind, Version: "0.0.1"}, IntegratedTime: integrated,
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

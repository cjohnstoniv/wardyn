// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package releasesigtest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore/pkg/signature"
)

// The legacy cosign layout (cosign v2 without --new-bundle-format): `cosign sign` writes a layer of the
// sha256-<hex>.sig tag and `cosign attest` a layer of sha256-<hex>.att, with the signature, the certificate
// and the Rekor entry in the layer's annotations.
const (
	LegacySignatureMediaType   = "application/vnd.dev.cosign.simplesigning.v1+json"
	LegacyAttestationMediaType = "application/vnd.dsse.envelope.v1+json"

	AnnotationSignature   = "dev.cosignproject.cosign/signature"
	AnnotationCertificate = "dev.sigstore.cosign/certificate"
	AnnotationChain       = "dev.sigstore.cosign/chain"
	AnnotationBundle      = "dev.sigstore.cosign/bundle"
)

// LegacyLayer is one layer of a legacy tag manifest: its media type and annotations, and the blob it names.
type LegacyLayer struct {
	MediaType   string
	Annotations map[string]string
	Blob        []byte
}

// SimpleSigningPayload is the payload `cosign sign` signs for the image digest ("sha256:<hex>") pushed as
// dockerRef.
func SimpleSigningPayload(dockerRef, digest string) []byte {
	b, _ := json.Marshal(map[string]any{"critical": map[string]any{
		"identity": map[string]any{"docker-reference": dockerRef},
		"image":    map[string]any{"docker-manifest-digest": digest},
		"type":     "cosign container image signature"}, "optional": nil})
	return b
}

// LegacySignature is the .sig layer `cosign sign` writes for payload, signed keyless as o says: the
// certificate and its chain, the Rekor entry unless o.NoTlog, and the TSA's timestamp when o.RFC3161, are
// annotations.
func (s *Signer) LegacySignature(payload []byte, o Options) LegacyLayer {
	s.t.Helper()
	key, der := s.leaf(o)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	l := s.legacySignature(key, payload, certPEM, o.NoTlog)
	s.addCertificate(l, certPEM)
	if o.RFC3161 {
		s.addTimestamp(l)
	}
	return l
}

// LegacySignature is the .sig layer `cosign sign --key` writes for payload: no certificate, and the Rekor
// entry unless o.NoTlog. Only o.NoTlog applies.
func (k *Key) LegacySignature(payload []byte, o Options) LegacyLayer {
	k.s.t.Helper()
	return k.s.legacySignature(k.key, payload, []byte(k.PublicKeyPEM()), o.NoTlog)
}

// LegacyAttestation is the .att layer `cosign attest` writes for the in-toto statement, signed keyless as o
// says, with an intoto v0.0.2 Rekor entry unless o.NoTlog.
func (s *Signer) LegacyAttestation(statement []byte, o Options) LegacyLayer {
	s.t.Helper()
	key, der := s.leaf(o)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	const payloadType = "application/vnd.in-toto+json"
	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(statement), statement)
	digest := sha256.Sum256([]byte(pae))
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	s.check(err)
	sig64 := base64.StdEncoding.EncodeToString(sig)
	blob, err := json.Marshal(map[string]any{"payloadType": payloadType, "payload": base64.StdEncoding.EncodeToString(statement),
		"signatures": []any{map[string]any{"keyid": "", "sig": sig64}}})
	s.check(err)
	var st struct {
		PredicateType string `json:"predicateType"`
	}
	s.check(json.Unmarshal(statement, &st))
	l := LegacyLayer{MediaType: LegacyAttestationMediaType, Blob: blob,
		Annotations: map[string]string{AnnotationSignature: "", "predicateType": st.PredicateType}}
	s.addCertificate(l, certPEM)
	if !o.NoTlog {
		// Rekor's intoto v0.0.2 body stores the envelope signature as the base64 of its base64 text.
		envHash, payloadHash := sha256.Sum256(blob), sha256.Sum256(statement)
		body, err := json.Marshal(map[string]any{"apiVersion": "0.0.2", "kind": "intoto", "spec": map[string]any{"content": map[string]any{
			"envelope": map[string]any{"payloadType": payloadType, "signatures": []any{map[string]any{
				"publicKey": base64.StdEncoding.EncodeToString(certPEM), "sig": base64.StdEncoding.EncodeToString([]byte(sig64))}}},
			"hash":        map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(envHash[:])},
			"payloadHash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(payloadHash[:])}}}})
		s.check(err)
		l.Annotations[AnnotationBundle] = s.rekorBundle(body)
	}
	return l
}

// ResignLegacySET signs l's Rekor promise again with this Signer's log key, keeping the log ID l names: a
// promise that claims the other log but was not signed by it.
func (s *Signer) ResignLegacySET(l LegacyLayer) LegacyLayer {
	s.t.Helper()
	var rb rekorBundleJSON
	s.check(json.Unmarshal([]byte(l.Annotations[AnnotationBundle]), &rb))
	rb.SignedEntryTimestamp = s.set(rb.Payload)
	b, err := json.Marshal(rb)
	s.check(err)
	ann := make(map[string]string, len(l.Annotations))
	for k, v := range l.Annotations {
		ann[k] = v
	}
	ann[AnnotationBundle] = string(b)
	l.Annotations = ann
	return l
}

// LoggingTo is s whose CT log and Rekor are to's: certificates from s's Fulcio, logged where to's trust root
// accepts them.
func (s *Signer) LoggingTo(to *Signer) *Signer {
	c := *s
	c.rekorKey, c.rekorID, c.ctKey, c.ctID = to.rekorKey, to.rekorID, to.ctKey, to.ctID
	return &c
}

func (s *Signer) legacySignature(key *ecdsa.PrivateKey, payload, verifierPEM []byte, noTlog bool) LegacyLayer {
	digest := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	s.check(err)
	l := LegacyLayer{MediaType: LegacySignatureMediaType, Blob: payload,
		Annotations: map[string]string{AnnotationSignature: base64.StdEncoding.EncodeToString(sig)}}
	if !noTlog {
		l.Annotations[AnnotationBundle] = s.rekorBundle(s.tlogEntry(verifierPEM, digest[:], sig, time.Now().Unix()).CanonicalizedBody)
	}
	return l
}

// addCertificate adds the certificate and the chain cosign writes beside it (intermediate, then root).
func (s *Signer) addCertificate(l LegacyLayer, certPEM []byte) {
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.inter.Raw})
	for _, ca := range s.trust.FulcioCertificateAuthorities() {
		if f, ok := ca.(*root.FulcioCertificateAuthority); ok {
			chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Root.Raw})...)
		}
	}
	l.Annotations[AnnotationCertificate] = string(certPEM)
	l.Annotations[AnnotationChain] = string(chain)
}

// rekorBundleJSON is cosign's dev.sigstore.cosign/bundle annotation.
type rekorBundleJSON struct {
	SignedEntryTimestamp []byte           `json:"SignedEntryTimestamp"`
	Payload              rekorPayloadJSON `json:"Payload"`
}

type rekorPayloadJSON struct {
	Body           string `json:"body"`
	IntegratedTime int64  `json:"integratedTime"`
	LogIndex       int64  `json:"logIndex"`
	LogID          string `json:"logID"`
}

// rekorBundle is the bundle annotation for body as the test Rekor logs it: the promise only, as cosign stores
// it (no inclusion proof).
func (s *Signer) rekorBundle(body []byte) string {
	p := rekorPayloadJSON{Body: base64.StdEncoding.EncodeToString(body), IntegratedTime: time.Now().Unix(), LogIndex: 1000, LogID: s.rekorID}
	b, err := json.Marshal(rekorBundleJSON{SignedEntryTimestamp: s.set(p), Payload: p})
	s.check(err)
	return string(b)
}

// set is this Signer's Rekor promise over p: its signature over the canonical JSON of the entry.
func (s *Signer) set(p rekorPayloadJSON) []byte {
	payload, err := json.Marshal(map[string]any{"body": p.Body, "integratedTime": p.IntegratedTime, "logID": p.LogID, "logIndex": p.LogIndex})
	s.check(err)
	signer, err := signature.LoadECDSASignerVerifier(s.rekorKey, crypto.SHA256)
	s.check(err)
	sig, err := signer.SignMessage(bytes.NewReader(payload))
	s.check(err)
	return sig
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
)

// The legacy cosign layout (cosign v2 without --new-bundle-format): `cosign sign` adds a layer to the
// sha256-<hex>.sig tag and `cosign attest` a layer to sha256-<hex>.att. The signature, the certificate and
// the Rekor entry are annotations on the layer.
const (
	LegacySignatureMediaType   = "application/vnd.dev.cosign.simplesigning.v1+json"
	LegacyAttestationMediaType = "application/vnd.dsse.envelope.v1+json"

	annotationSignature   = "dev.cosignproject.cosign/signature"
	annotationCertificate = "dev.sigstore.cosign/certificate"
	annotationBundle      = "dev.sigstore.cosign/bundle"
	annotationTimestamp   = "dev.sigstore.cosign/rfc3161timestamp"

	// legacyBundleMediaType is Sigstore bundle v0.1, the version whose transparency-log entries carry a
	// promise (the SET) and no inclusion proof, which is all a legacy layer holds.
	legacyBundleMediaType = "application/vnd.dev.sigstore.bundle+json;version=0.1"
	simpleSigningType     = "cosign container image signature"
)

// LegacyLayer is one layer of a legacy .sig or .att tag manifest: its descriptor, which carries the
// annotations, and the blob the descriptor names.
type LegacyLayer struct {
	Descriptor Descriptor
	Blob       []byte
}

// Candidates are the signatures read for one image: Sigstore bundles from its OCI referrers, and the layers
// of its legacy cosign tags. Each source is bounded by MaxCandidatesPerSigner on its own, and Check
// bounds the two together.
type Candidates struct {
	Bundles [][]byte
	Legacy  []LegacyLayer
}

// LegacyReader reads legacy cosign tags; *RegistryClient is one.
type LegacyReader interface {
	LegacyTag(ctx context.Context, ref Ref, subject, suffix string) (*Manifest, error)
	Blob(ctx context.Context, ref Ref, d Descriptor) ([]byte, error)
}

// ReadLegacy reads the layers of subject's .sig tag, then its .att tag, at most MaxCandidatesPerSigner in all.
// A tag the registry does not have adds nothing. A layer over MaxBlobBytes is kept by its descriptor alone and
// never read: an SBOM attestation is often several MB, and it must not stop the check of the signature beside
// it. A blob that is missing or not its digest is kept the same way, as one bad candidate, and a tag manifest
// that cannot be parsed adds nothing. Only a registry that cannot be read (*Unavailable, a deadline) is
// returned, unchanged, so the caller can tell an unreachable registry from an unsigned image.
func ReadLegacy(ctx context.Context, r LegacyReader, ref Ref, subject string) ([]LegacyLayer, error) {
	var out []LegacyLayer
	for _, suffix := range []string{".sig", ".att"} {
		m, err := r.LegacyTag(ctx, ref, subject, suffix)
		if err != nil && registryDown(err) {
			return nil, err
		}
		if err != nil { // missing, or a manifest that cannot be read: nothing to check from this tag
			continue
		}
		for _, d := range m.Layers {
			if len(out) == MaxCandidatesPerSigner {
				return out, nil
			}
			if d.Size > MaxBlobBytes {
				out = append(out, LegacyLayer{Descriptor: d})
				continue
			}
			b, err := r.Blob(ctx, ref, d)
			if err != nil && registryDown(err) {
				return nil, err
			}
			out = append(out, LegacyLayer{Descriptor: d, Blob: b}) // a blob that failed to read refuses itself
		}
	}
	return out, nil
}

// legacyCandidates converts the first MaxCandidatesPerSigner layers into bundles sigstore-go verifies like any
// other: a .sig layer becomes a message signature over its payload, once the payload is shown to name digest;
// an .att layer becomes a DSSE bundle, whose statement subject Check matches against digest.
func legacyCandidates(layers []LegacyLayer, digest string) []candidate {
	if len(layers) > MaxCandidatesPerSigner {
		layers = layers[:MaxCandidatesPerSigner]
	}
	out := make([]candidate, len(layers))
	for i, l := range layers {
		c, err := legacyCandidate(l, digest)
		if err != nil {
			c = candidate{err: fmt.Errorf("a legacy cosign layer: %w", err)}
		}
		out[i] = c
	}
	return out
}

// legacyCandidate's errors name what is wrong with the layer in fixed words, or with a value Wardyn computed
// (the blob's sha256, the image digest): every field of the layer is registry content, which a registry sent a
// Basic credential could set to that credential, so none is repeated.
func legacyCandidate(l LegacyLayer, digest string) (candidate, error) {
	if l.Descriptor.Size > MaxBundleBytes || len(l.Blob) > MaxBundleBytes {
		return candidate{}, fmt.Errorf("its blob is larger than %d bytes", MaxBundleBytes)
	}
	if sum256(l.Blob) != l.Descriptor.Digest {
		return candidate{}, fmt.Errorf("%w: its blob hashes to %s, not its descriptor's digest", ErrDigestMismatch, sum256(l.Blob))
	}
	ann := l.Descriptor.Annotations
	material, err := legacyMaterial(ann)
	if err != nil {
		return candidate{}, err
	}
	pb := &protobundle.Bundle{MediaType: legacyBundleMediaType, VerificationMaterial: material}
	var c candidate
	switch l.Descriptor.MediaType {
	case LegacySignatureMediaType:
		if err := legacyPayloadNames(l.Blob, digest); err != nil {
			return candidate{}, err
		}
		sig, err := base64.StdEncoding.DecodeString(ann[annotationSignature])
		if err != nil || len(sig) == 0 {
			return candidate{}, errors.New("its signature annotation is missing or not base64")
		}
		sum := sha256.Sum256(l.Blob)
		pb.Content = &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: sum[:]}, Signature: sig}}
		c.payload = l.Blob
	case LegacyAttestationMediaType:
		env, err := legacyEnvelope(l.Blob)
		if err != nil {
			return candidate{}, err
		}
		pb.Content = &protobundle.Bundle_DsseEnvelope{DsseEnvelope: env}
	default:
		return candidate{}, errors.New("its media type is not a cosign signature or attestation")
	}
	if c.b, err = bundle.NewBundle(pb); err != nil {
		return candidate{}, errors.New("it is not a valid bundle")
	}
	return c, nil
}

// legacyPayloadNames checks that a simplesigning payload is a cosign image signature for digest. The message
// signature covers the payload, not the image, so this check is what binds the signature to the image. The
// payload's docker-reference is not checked, as cosign does not: a mirrored image keeps its signature.
func legacyPayloadNames(payload []byte, digest string) error {
	var p struct {
		Critical struct {
			Image struct {
				Digest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return errors.New("its payload is not JSON")
	}
	if p.Critical.Type != simpleSigningType {
		return fmt.Errorf("its payload type is not %q", simpleSigningType)
	}
	if p.Critical.Image.Digest != digest {
		return fmt.Errorf("it signs another image, not %s", digest)
	}
	return nil
}

// legacyEnvelope parses the DSSE envelope an .att layer holds. cosign writes exactly one signature.
func legacyEnvelope(blob []byte) (*protodsse.Envelope, error) {
	var e struct {
		PayloadType string `json:"payloadType"`
		Payload     []byte `json:"payload"`
		Signatures  []struct {
			KeyID string `json:"keyid"`
			Sig   []byte `json:"sig"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(blob, &e); err != nil {
		return nil, errors.New("its envelope is not readable")
	}
	if len(e.Signatures) != 1 {
		return nil, fmt.Errorf("its envelope has %d signatures, not one", len(e.Signatures))
	}
	return &protodsse.Envelope{PayloadType: e.PayloadType, Payload: e.Payload,
		Signatures: []*protodsse.Signature{{Keyid: e.Signatures[0].KeyID, Sig: e.Signatures[0].Sig}}}, nil
}

// legacyMaterial is the verification material of a layer: its leaf certificate, or for a key signature an
// empty key hint, plus the Rekor entry from the bundle annotation and the TSA's response from the
// rfc3161timestamp annotation. The verifier decides what they must prove: a trust root that names a Rekor log
// still requires the entry, and a timestamp counts only from a TSA the root names. The chain annotation is ignored on purpose:
// it comes from the registry, and the signer's trust root supplies the CA certificates.
func legacyMaterial(ann map[string]string) (*protobundle.VerificationMaterial, error) {
	vm := &protobundle.VerificationMaterial{}
	if certPEM := ann[annotationCertificate]; certPEM != "" {
		block, _ := pem.Decode([]byte(certPEM))
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("its certificate annotation is not a PEM certificate")
		}
		vm.Content = &protobundle.VerificationMaterial_X509CertificateChain{X509CertificateChain: &protocommon.X509CertificateChain{
			Certificates: []*protocommon.X509Certificate{{RawBytes: block.Bytes}}}}
	} else {
		vm.Content = &protobundle.VerificationMaterial_PublicKey{PublicKey: &protocommon.PublicKeyIdentifier{}}
	}
	if raw := ann[annotationBundle]; raw != "" {
		entry, err := legacyTlogEntry(raw)
		if err != nil {
			return nil, err
		}
		vm.TlogEntries = []*protorekor.TransparencyLogEntry{entry}
	}
	if raw := ann[annotationTimestamp]; raw != "" {
		var ts struct {
			SignedRFC3161Timestamp []byte `json:"SignedRFC3161Timestamp"`
		}
		if err := json.Unmarshal([]byte(raw), &ts); err != nil || len(ts.SignedRFC3161Timestamp) == 0 {
			return nil, errors.New("its RFC 3161 timestamp annotation is not readable")
		}
		vm.TimestampVerificationData = &protobundle.TimestampVerificationData{
			Rfc3161Timestamps: []*protocommon.RFC3161SignedTimestamp{{SignedTimestamp: ts.SignedRFC3161Timestamp}}}
	}
	return vm, nil
}

// legacyTlogEntry converts cosign's bundle annotation into a transparency-log entry with its promise. The
// entry kind and version are read from the logged body, which sigstore-go then checks against the signature,
// the key or certificate, and the payload.
func legacyTlogEntry(raw string) (*protorekor.TransparencyLogEntry, error) {
	var rb struct {
		SignedEntryTimestamp []byte `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	}
	if err := json.Unmarshal([]byte(raw), &rb); err != nil {
		return nil, errors.New("its Rekor bundle annotation is not readable")
	}
	body, err := base64.StdEncoding.DecodeString(rb.Payload.Body)
	if err != nil {
		return nil, errors.New("its Rekor entry body is not base64")
	}
	logID, err := hex.DecodeString(rb.Payload.LogID)
	if err != nil || len(logID) == 0 {
		return nil, errors.New("its Rekor log ID is missing or not hex")
	}
	var kv struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(body, &kv); err != nil || kv.Kind == "" || kv.APIVersion == "" {
		return nil, errors.New("its Rekor entry body names no kind and version")
	}
	if len(rb.SignedEntryTimestamp) == 0 {
		return nil, errors.New("its Rekor entry has no signed entry timestamp")
	}
	return &protorekor.TransparencyLogEntry{LogIndex: rb.Payload.LogIndex, LogId: &protocommon.LogId{KeyId: logID},
		KindVersion: &protorekor.KindVersion{Kind: kv.Kind, Version: kv.APIVersion}, IntegratedTime: rb.Payload.IntegratedTime,
		InclusionPromise: &protorekor.InclusionPromise{SignedEntryTimestamp: rb.SignedEntryTimestamp}, CanonicalizedBody: body}, nil
}

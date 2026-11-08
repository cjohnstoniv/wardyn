// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// In-toto predicate types that are a signature by default.
const (
	PredicateCosignSign        = "https://sigstore.dev/cosign/sign/v1" // cosign sign --new-bundle-format
	PredicateSLSAProvenanceV02 = "https://slsa.dev/provenance/v0.2"
	PredicateSLSAProvenanceV1  = "https://slsa.dev/provenance/v1" // GitHub build provenance
)

// defaultSignaturePredicates is the closed set of statements that count as a signer's signature on an image:
// a signature, or provenance saying the signer built it. Any other statement by the same identity (an SBOM,
// a vulnerability scan) is about the image, not an approval of it, so it only ever counts as an attestation.
var defaultSignaturePredicates = []string{PredicateCosignSign, PredicateSLSAProvenanceV02, PredicateSLSAProvenanceV1}

// Bounds on what one check reads.
const (
	MaxBundleBytes         = 1 << 20 // a larger bundle is refused before it is parsed
	MaxCandidatesPerSigner = 16      // only the first candidates, bundles then legacy layers, are verified
)

var errNotStatement = errors.New("the bundle signs no in-toto statement, so it names no image")

// Check verifies bundles (Sigstore bundle JSON) for the image digest ("sha256:<hex>") against r and returns the
// verdict. A candidate counts for a signer only when it verifies under the signer's trust root or key, its
// in-toto statement names the digest as a subject, and it was signed by the signer's identity. It reads
// nothing from the network.
func Check(r Requirements, digest string, bundles [][]byte) Verdict {
	return CheckCandidates(r, digest, Candidates{Bundles: bundles})
}

// CheckCandidates is Check over referrer bundles and legacy cosign layers together, so a signature from one
// source and a required attestation from the other admit the image together.
func CheckCandidates(r Requirements, digest string, c Candidates) Verdict {
	bundles := c.Bundles
	d, err := sha256Digest(digest)
	if err != nil {
		return Verdict{Reason: SignatureMissing, Digest: digest, Detail: err.Error()}
	}
	if len(r.AnyOf) == 0 && len(r.AllOf) == 0 {
		return Verdict{Reason: NoPolicy, Digest: digest}
	}
	if len(bundles) > MaxCandidatesPerSigner {
		bundles = bundles[:MaxCandidatesPerSigner]
	}
	cands := append(parse(bundles), legacyCandidates(c.Legacy, digest)...)
	if len(cands) > MaxCandidatesPerSigner { // one bound over both sources, bundles first
		cands = cands[:MaxCandidatesPerSigner]
	}
	var primary *outcome
	if len(r.AnyOf) > 0 {
		outs := make([]outcome, 0, len(r.AnyOf))
		for _, s := range r.AnyOf {
			o := s.evaluate(d, cands)
			outs = append(outs, o)
			if o.passed() {
				primary = &o
				break
			}
		}
		if primary == nil {
			v := refusal(outs)
			v.Digest = digest
			return v
		}
	}
	var signers []string
	if primary != nil {
		signers = append(signers, primary.signer.name)
	}
	for _, s := range r.AllOf {
		o := s.evaluate(d, cands)
		if !o.passed() {
			v := refusal([]outcome{o})
			v.Digest = digest
			return v
		}
		if primary == nil {
			primary = &o
		}
		if !containsName(signers, s.name) {
			signers = append(signers, s.name)
		}
	}
	return Verdict{Digest: digest, Signer: primary.signer.name, Signers: signers, Identity: primary.signed.identity,
		Issuer: primary.signed.issuer, KeyID: primary.signed.keyID, Attestations: append([]string(nil), primary.signer.attestations...)}
}

type candidate struct {
	b   *bundle.Bundle
	err error
	// payload is set for a legacy cosign signature: the payload its message signature covers, which
	// legacyCandidate checked names the image digest.
	payload []byte
}

func parse(bundles [][]byte) []candidate {
	out := make([]candidate, len(bundles))
	for i, raw := range bundles {
		if len(raw) > MaxBundleBytes {
			out[i].err = fmt.Errorf("a bundle is larger than %d bytes", MaxBundleBytes)
			continue
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			out[i].err = errors.New("a bundle is not readable") // the parser's text quotes the bundle's fields
			continue
		}
		out[i].b = &b
	}
	return out
}

// signed names who signed a verified candidate.
type signed struct{ identity, issuer, keyID string }

// outcome is what one signer made of the candidates.
type outcome struct {
	signer   *Signer
	signed   *signed // the first candidate that is the signer's signature
	attested map[string]bool
	mismatch *signed // the first signature by another identity or key
	err      error   // the first verification error
}

func (o *outcome) note(err error) {
	if o.err == nil {
		o.err = err
	}
}

func (o outcome) missingAttestation() string {
	for _, a := range o.signer.attestations {
		if !o.attested[a] {
			return a
		}
	}
	return ""
}

func (o outcome) passed() bool { return o.signed != nil && o.missingAttestation() == "" }

func (s *Signer) evaluate(d []byte, cands []candidate) outcome {
	o := outcome{signer: s, attested: map[string]bool{}}
	for _, c := range cands {
		if c.err != nil {
			o.note(c.err)
			continue
		}
		artifact := verify.WithArtifactDigest("sha256", d)
		if c.payload != nil {
			artifact = verify.WithArtifact(bytes.NewReader(c.payload))
		} else if c.b.Bundle.GetDsseEnvelope() == nil {
			o.note(errNotStatement)
			continue
		}
		// A bundle naming another key cannot verify under this signer's key. The hint is unsigned material a
		// registry can set to anything, so it is never repeated as the key that signed.
		if hint := c.b.Bundle.GetVerificationMaterial().GetPublicKey().GetHint(); s.keyHints != nil && hint != "" && !s.keyHints[hint] {
			if o.mismatch == nil {
				o.mismatch = &signed{}
			}
			continue
		}
		who := verify.WithKey()
		if s.identity != nil {
			who = verify.WithoutIdentitiesUnsafe() // matched below, so a refusal can say who signed instead
		}
		res, err := s.verifier.Verify(c.b, verify.NewPolicy(artifact, who))
		if err != nil {
			o.note(verifyFailure(err))
			continue
		}
		predicate := res.Statement.GetPredicateType()
		// A legacy .sig is what `cosign sign` writes, a signature with no predicate: it is the signer's
		// signature whatever the signer's predicate set, the built-in signer's included.
		isSignature := c.payload != nil || s.sigPredicates[predicate]
		by := signed{keyID: s.want}
		if s.identity != nil {
			cert := res.Signature.Certificate
			if cert == nil {
				o.note(errors.New("a keyless signer's bundle carries no certificate"))
				continue
			}
			by = signed{identity: cert.SubjectAlternativeName, issuer: cert.Extensions.Issuer}
			if err := s.identity.Verify(*cert); err != nil {
				if isSignature && o.mismatch == nil {
					o.mismatch = &by
				}
				continue
			}
		}
		if isSignature && o.signed == nil {
			o.signed = &by
		}
		if c.payload == nil {
			o.attested[predicate] = true
		}
	}
	return o
}

// verifyFailure names the check a bundle failed in fixed words. sigstore-go's text carries what the bundle
// holds (a payload type, a certificate's names, a log entry's fields), which a registry sent a Basic credential
// could set to that credential, so only the verifier's own leading words pick the phrase.
func verifyFailure(err error) error {
	msg := err.Error()
	for _, f := range []struct{ prefix, phrase string }{
		{"failed to verify log inclusion: not enough verified log entries", "not enough verified log entries from a transparency log the trust root names"},
		{"failed to verify log inclusion", "a signature's transparency log entry did not verify"},
		{"failed to verify timestamps: threshold not met for verified log entry", "not enough verified transparency log timestamps"},
		{"failed to verify timestamps", "unable to verify signed timestamps or any other time for the signature"},
		{"failed to verify leaf certificate", "a signature's leaf certificate does not chain to the trust root"},
		{"failed to verify signed certificate timestamp", "a signature's certificate has no valid signed certificate timestamp"},
		{"SCTs required", "a signature has no signed certificate timestamp, which the trust root requires"},
		{"failed to verify signature", "a signature does not verify"},
	} {
		if strings.HasPrefix(msg, f.prefix) {
			return errors.New(f.phrase)
		}
	}
	return errors.New("a signature did not verify under the signer's trust root or key")
}

// refusal is the verdict when no outcome passed: a missing attestation first (the image is signed), then a
// signature by someone else, then no signature at all.
func refusal(outs []outcome) Verdict {
	for _, o := range outs {
		if o.signed != nil {
			if a := o.missingAttestation(); a != "" {
				return Verdict{Reason: AttestationMissing, Signer: o.signer.name, Want: a}
			}
		}
	}
	for _, o := range outs {
		if m := o.mismatch; m != nil {
			return Verdict{Reason: SignatureInvalid, Signer: o.signer.name, Want: o.signer.want, WantIssuer: o.signer.wantIssuer,
				Found: bounded(m.identity + m.keyID), FoundIssuer: m.issuer}
		}
	}
	v := Verdict{Reason: SignatureMissing}
	for _, o := range outs {
		v.Signers = append(v.Signers, o.signer.name)
		if o.err != nil && v.Detail == "" {
			v.Detail = bounded(o.err.Error())
		}
	}
	if len(outs) == 1 {
		v.Signer = outs[0].signer.name
	}
	return v
}

func sha256Digest(digest string) ([]byte, error) {
	h, ok := strings.CutPrefix(digest, "sha256:")
	b, err := hex.DecodeString(h)
	if !ok || err != nil || len(b) != 32 || strings.ToLower(h) != h {
		return nil, fmt.Errorf("%q is not a sha256 image digest", bounded(digest))
	}
	return b, nil
}

// bounded keeps text that came from a bundle or a verifier short enough for a refusal sentence.
func bounded(s string) string {
	const limit = 300
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

func containsName(names []string, n string) bool {
	for _, v := range names {
		if v == n {
			return true
		}
	}
	return false
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

func TestKeylessSignaturePasses(t *testing.T) {
	s := releasesigtest.New(t)
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts()))
	wantReason(t, v, "")
	if v.Signer != "corp-ci" || v.Identity != ciIdentity || v.Issuer != ciIssuer || v.Digest != digest || v.KeyID != "" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestKeylessWrongIdentityIsInvalid(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.Identity = "https://github.com/corp/forks/.github/workflows/build.yml@refs/heads/main"
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), o))
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.Signer != "corp-ci" || v.Found != o.Identity || v.FoundIssuer != ciIssuer || v.Want != ciIdentity || v.WantIssuer != ciIssuer {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestKeylessWrongIssuerIsInvalid(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.Issuer = "https://accounts.google.com"
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), o))
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.FoundIssuer != o.Issuer || v.WantIssuer != ciIssuer {
		t.Fatalf("verdict = %+v", v)
	}
}

// The test root has a CT log, as the public-good root does, so a certificate without an SCT is refused:
// the policy cosign verify applies by default.
func TestSCTRequiredUnderPublicGoodShapedRoot(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.NoSCT = true
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), o))
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "certificate timestamp")
}

func TestTransparencyLogRequired(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.NoTlog = true
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), o))
	wantReason(t, v, imageadmit.SignatureMissing)
}

func TestSubjectDigestMustBeTheImage(t *testing.T) {
	s := releasesigtest.New(t)
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, otherHex), ciOpts()))
	wantReason(t, v, imageadmit.SignatureMissing)
}

func vendorKey(k *releasesigtest.Key) imageadmit.SignerSpec {
	return imageadmit.SignerSpec{Name: "vendor-key", Images: []string{"registry.corp.example/team/*"}, PublicKey: k.PublicKeyPEM()}
}

func TestKeySignaturePasses(t *testing.T) {
	s := releasesigtest.New(t)
	k := s.NewKey()
	v := check(t, compile(t, s, vendorKey(k)), nil, k.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), releasesigtest.Options{}))
	wantReason(t, v, "")
	if v.Signer != "vendor-key" || v.KeyID != k.Hint() || v.Identity != "" {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestKeyMismatchIsInvalid(t *testing.T) {
	s := releasesigtest.New(t)
	k1, k2 := s.NewKey(), s.NewKey()
	v := check(t, compile(t, s, vendorKey(k1)), nil, k2.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), releasesigtest.Options{}))
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.Found != k2.Hint() || v.Want != k1.Hint() {
		t.Fatalf("verdict = %+v", v)
	}
}

// A key signature proves itself without a log only when the signer says so; by default the log entry is
// required.
func TestKeyWithoutTransparencyLogPassesOffline(t *testing.T) {
	s := releasesigtest.New(t)
	k := s.NewKey()
	offline := k.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), releasesigtest.Options{NoTlog: true})
	wantReason(t, check(t, compile(t, s, vendorKey(k)), nil, offline), imageadmit.SignatureMissing)
	spec := vendorKey(k)
	no := false
	spec.TransparencyLog = &no
	wantReason(t, check(t, compile(t, s, spec), nil, offline), "")
}

func withAttestation(spec imageadmit.SignerSpec, predicate string) imageadmit.SignerSpec {
	spec.RequireAttestations = []string{predicate}
	return spec
}

func TestRequiredAttestationMissing(t *testing.T) {
	s := releasesigtest.New(t)
	p := compile(t, s, withAttestation(corpCI(), imageadmit.PredicateSLSAProvenanceV1))
	v := check(t, p, nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts()))
	wantReason(t, v, imageadmit.AttestationMissing)
	if v.Signer != "corp-ci" || v.Want != imageadmit.PredicateSLSAProvenanceV1 {
		t.Fatalf("verdict = %+v", v)
	}
	// Signed provenance is both the signature and the attestation.
	v = check(t, p, nil, s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), ciOpts()))
	wantReason(t, v, "")
	if !contains(v.Attestations, imageadmit.PredicateSLSAProvenanceV1) {
		t.Fatalf("attestations = %v", v.Attestations)
	}
}

func TestAttestationByAnotherIdentityIsMissing(t *testing.T) {
	s := releasesigtest.New(t)
	other := ciOpts()
	other.Identity = "https://github.com/corp/scanner/.github/workflows/scan.yml@refs/heads/main"
	v := check(t, compile(t, s, withAttestation(corpCI(), imageadmit.PredicateSLSAProvenanceV1)), nil,
		s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts()),
		s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), other))
	wantReason(t, v, imageadmit.AttestationMissing)
}

func TestProfileAndSiteSignersBothRequired(t *testing.T) {
	s := releasesigtest.New(t)
	k := s.NewKey()
	p := compile(t, s, corpCI(), vendorKey(k))
	site := s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts())
	v := check(t, p, []string{"vendor-key"}, site)
	wantReason(t, v, imageadmit.SignatureMissing)
	if v.Signer != "vendor-key" {
		t.Fatalf("refusal names %q, want the profile's signer vendor-key (%+v)", v.Signer, v)
	}
	v = check(t, p, []string{"vendor-key"}, site, k.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), releasesigtest.Options{}))
	wantReason(t, v, "")
	if v.Signer != "corp-ci" || !contains(v.Signers, "corp-ci") || !contains(v.Signers, "vendor-key") {
		t.Fatalf("verdict = %+v", v)
	}
}

// An SBOM is a signed statement about the image by the right identity, but a CI identity attests SBOMs of
// images it never approved, so it is not the signer's signature.
func TestSBOMAttestationIsNotASignature(t *testing.T) {
	s := releasesigtest.New(t)
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, predicateCycloneDX, imageHex), ciOpts()))
	wantReason(t, v, imageadmit.SignatureMissing)
}

// A vulnerability scan attests a scan result, even a failed one; it never admits the image.
func TestVulnAttestationIsNotASignature(t *testing.T) {
	s := releasesigtest.New(t)
	v := check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, predicateVuln, imageHex), ciOpts()))
	wantReason(t, v, imageadmit.SignatureMissing)
}

func TestSignaturePredicatesExtendPerSigner(t *testing.T) {
	s := releasesigtest.New(t)
	const approved = "https://corp.example/attestations/approved/v1"
	spec := corpCI()
	spec.SignaturePredicates = []string{approved}
	p := compile(t, s, spec)
	wantReason(t, check(t, p, nil, s.SignDSSE(statement(t, approved, imageHex), ciOpts())), "")
	wantReason(t, check(t, p, nil, s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV02, imageHex), ciOpts())), "")
	wantReason(t, check(t, compile(t, s, corpCI()), nil, s.SignDSSE(statement(t, approved, imageHex), ciOpts())), imageadmit.SignatureMissing)
}

// A bare message signature over bytes whose sha256 is the digest names no predicate, so it is not in the set.
func TestMessageSignatureBundleIsNotAnImageSignature(t *testing.T) {
	s := releasesigtest.New(t)
	manifest := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(manifest)
	req, refused := compile(t, s, corpCI()).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	v := imageadmit.Check(req, "sha256:"+hex.EncodeToString(sum[:]), [][]byte{s.Sign(manifest, ciOpts())})
	wantReason(t, v, imageadmit.SignatureMissing)
}

func TestOversizeBundleRefusedBeforeParsing(t *testing.T) {
	s := releasesigtest.New(t)
	good := s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts())
	padded := append(append([]byte{}, good...), bytes.Repeat([]byte(" "), imageadmit.MaxBundleBytes)...)
	v := check(t, compile(t, s, corpCI()), nil, padded)
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "larger than")
}

func TestCandidateCapPerSigner(t *testing.T) {
	s := releasesigtest.New(t)
	p := compile(t, s, corpCI())
	good := s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts())
	junk := func(n int) [][]byte {
		out := make([][]byte, n)
		for i := range out {
			out[i] = []byte(`{}`)
		}
		return out
	}
	wantReason(t, check(t, p, nil, append(junk(imageadmit.MaxCandidatesPerSigner-1), good)...), "")
	wantReason(t, check(t, p, nil, append(junk(imageadmit.MaxCandidatesPerSigner), good)...), imageadmit.SignatureMissing)
}

func TestCheckRefusesANonSHA256Digest(t *testing.T) {
	s := releasesigtest.New(t)
	req, refused := compile(t, s, corpCI()).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	good := s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts())
	for _, d := range []string{imageHex, "sha512:" + imageHex, "sha256:" + imageHex[:10], "sha256:" + imageHex[:63] + "G"} {
		wantReason(t, imageadmit.Check(req, d, [][]byte{good}), imageadmit.SignatureMissing)
	}
}

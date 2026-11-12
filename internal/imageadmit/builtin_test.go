// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

const wardynImage = "ghcr.io/cjohnstoniv/agent-base:0.10.0"

func releaseOpts(version string) releasesigtest.Options {
	return releasesigtest.Options{Identity: releasesig.Identity(version), Issuer: releasesig.Issuer, Repository: releasesig.Repository}
}

func builtinCheck(t *testing.T, s *releasesigtest.Signer, version string, bundles ...[]byte) imageadmit.Verdict {
	t.Helper()
	p, err := imageadmit.Compile(imageadmit.Config{}, imageadmit.Options{Version: version, PublicGoodRoot: testRoot(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	req, refused := p.Require(wardynImage, true, nil)
	if refused != nil {
		return *refused
	}
	return imageadmit.Check(req, digest, bundles)
}

// The release workflow's build-provenance bundle is the built-in signer's signature.
func TestBuiltinPassesOnReleaseProvenance(t *testing.T) {
	s := releasesigtest.New(t)
	v := builtinCheck(t, s, "0.10.0", s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), releaseOpts("0.10.0")))
	wantReason(t, v, "")
	if v.Signer != imageadmit.BuiltinSigner || v.Identity != releasesig.Identity("0.10.0") || v.Issuer != releasesig.Issuer {
		t.Fatalf("verdict = %+v", v)
	}
}

// Its predicate set is exactly SLSA provenance v1: the release identity's SBOM, a cosign/sign statement or
// v0.2 provenance does not admit.
func TestBuiltinAcceptsOnlyProvenanceV1(t *testing.T) {
	s := releasesigtest.New(t)
	for _, pt := range []string{imageadmit.PredicateCosignSign, imageadmit.PredicateSLSAProvenanceV02, predicateCycloneDX} {
		v := builtinCheck(t, s, "0.10.0", s.SignDSSE(statement(t, pt, imageHex), releaseOpts("0.10.0")))
		wantReason(t, v, imageadmit.SignatureMissing)
	}
}

func TestBuiltinRefusesAnotherVersion(t *testing.T) {
	s := releasesigtest.New(t)
	v := builtinCheck(t, s, "0.10.0", s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), releaseOpts("0.9.8")))
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.Found != releasesig.Identity("0.9.8") || v.Want != releasesig.Identity("0.10.0") {
		t.Fatalf("verdict = %+v", v)
	}
}

// A development build has no release identity, so the built-in signer covers no image.
func TestBuiltinCoversNothingOnADevelopmentBuild(t *testing.T) {
	s := releasesigtest.New(t)
	v := builtinCheck(t, s, "0.10.0+80852d67f-dirty", s.SignDSSE(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), releaseOpts("0.10.0")))
	wantReason(t, v, imageadmit.NoPolicy)
	p, err := imageadmit.Compile(imageadmit.Config{}, imageadmit.Options{Version: "0.10.0+80852d67f", PublicGoodRoot: testRoot(t, s)})
	if err != nil {
		t.Fatal(err)
	}
	_, refused := p.Require(wardynImage, false, []string{imageadmit.BuiltinSigner})
	if refused == nil || refused.Reason != imageadmit.NoPolicy || refused.Signer != imageadmit.BuiltinSigner || refused.Undefined {
		t.Fatalf("profile naming the built-in signer on a development build: %+v", refused)
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
	"github.com/sigstore/sigstore-go/pkg/root"
)

const (
	imageHex = "4b1f0a6e5f3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e"
	otherHex = "0000000000000000000000000000000000000000000000000000000000000001"
	digest   = "sha256:" + imageHex
	ref      = "registry.corp.example/team/app:1.4"

	ciIssuer   = "https://token.actions.githubusercontent.com"
	ciIdentity = "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/main"
	ciRepo     = "https://github.com/corp/images"

	predicateCycloneDX = "https://cyclonedx.org/bom"
	predicateVuln      = "https://cosign.sigstore.dev/attestation/vuln/v1"
)

// statement is an in-toto v1 statement of predicateType about the image whose sha256 is subjectHex.
func statement(t *testing.T, predicateType, subjectHex string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"_type": "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"name": "registry.corp.example/team/app", "digest": map[string]string{"sha256": subjectHex}}},
		"predicateType": predicateType, "predicate": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ciOpts() releasesigtest.Options {
	return releasesigtest.Options{Identity: ciIdentity, Issuer: ciIssuer, Repository: ciRepo}
}

// corpCI is a keyless signer for the corp CI workflow over registry.corp.example/team.
func corpCI() imageadmit.SignerSpec {
	return imageadmit.SignerSpec{Name: "corp-ci", Images: []string{"registry.corp.example/team/*"},
		Keyless: &imageadmit.KeylessSpec{Issuer: ciIssuer, Identity: ciIdentity, SourceRepository: ciRepo}}
}

// testRoot is s's trust root, standing in for the public-good root.
func testRoot(t *testing.T, s *releasesigtest.Signer) *root.TrustedRoot {
	t.Helper()
	tr, ok := s.TrustedMaterial().(*root.TrustedRoot)
	if !ok {
		t.Fatalf("test trust material is %T, want *root.TrustedRoot", s.TrustedMaterial())
	}
	return tr
}

func compile(t *testing.T, s *releasesigtest.Signer, signers ...imageadmit.SignerSpec) *imageadmit.Policy {
	t.Helper()
	p, err := imageadmit.Compile(imageadmit.Config{Signers: signers}, imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s)})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

// check runs the site's check (and the profile's signers) on ref at digest over bundles.
func check(t *testing.T, p *imageadmit.Policy, profile []string, bundles ...[]byte) imageadmit.Verdict {
	t.Helper()
	req, refused := p.Require(ref, true, profile)
	if refused != nil {
		return *refused
	}
	return imageadmit.Check(req, digest, bundles)
}

func wantReason(t *testing.T, v imageadmit.Verdict, want imageadmit.Reason) {
	t.Helper()
	if v.Reason != want {
		t.Fatalf("reason = %q, want %q (verdict %+v)", v.Reason, want, v)
	}
	if want == "" && !v.Admitted() {
		t.Fatalf("Admitted() = false for %+v", v)
	}
	if want != "" && v.Admitted() {
		t.Fatalf("Admitted() = true for refusal %+v", v)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, got, sub string) {
	t.Helper()
	if !strings.Contains(got, sub) {
		t.Fatalf("%q does not contain %q", got, sub)
	}
}

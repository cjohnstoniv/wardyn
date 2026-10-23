// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package releasesig verifies a release's SHA256SUMS against its Sigstore bundle (SHA256SUMS.sigstore.json),
// offline: the bundle carries the transparency-log entry and its inclusion proof, and the trust root is the
// Sigstore public-good trusted_root.json embedded here (refreshed by scripts/refresh-sigstore-root.sh).
// The signer is pinned to this repository's release workflow at the tag of the version being installed.
package releasesig

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

//go:embed trusted_root.json
var trustedRootJSON []byte

const (
	// Issuer is the OIDC issuer of the release workflow's keyless certificate.
	Issuer = "https://token.actions.githubusercontent.com"
	// Repository is the source repository the certificate must name.
	Repository = "https://github.com/cjohnstoniv/wardyn"
	workflowAt = Repository + "/.github/workflows/release.yml@refs/tags/v"
)

// The refusals; every other failure wraps one of them.
var (
	ErrBundle    = errors.New("the signature bundle is not readable")
	ErrIdentity  = errors.New("SHA256SUMS was not signed by the release workflow for this version")
	ErrSignature = errors.New("SHA256SUMS does not verify against its signature bundle")
)

// Identity is the certificate identity a release's SHA256SUMS is signed with: release.yml at tag v<version>.
func Identity(version string) string { return workflowAt + version }

// TrustedRoot is the embedded Sigstore public-good trust root.
func TrustedRoot() (root.TrustedMaterial, error) { return root.NewTrustedRootFromJSON(trustedRootJSON) }

// policy is the verifier policy VerifySums applies, as `cosign verify-blob` does by default: an embedded SCT
// from a trusted CT log, and a transparency-log entry whose time vouches for the short-lived certificate.
var policy = []verify.VerifierOption{
	verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1),
}

// VerifySums checks that bundleJSON signs sums, that the certificate chains to tm's Fulcio and carries an SCT, that a
// transparency-log entry (verified against tm, offline) vouches for the signing time, and that the signer is
// release.yml at v<version> with the GitHub Actions issuer. There is no fallback: any failure refuses.
func VerifySums(tm root.TrustedMaterial, sums, bundleJSON []byte, version string) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("%w: %v", ErrBundle, err)
	}
	v, err := verify.NewVerifier(tm, policy...)
	if err != nil {
		return err
	}
	san, err := verify.NewSANMatcher(Identity(version), "")
	if err != nil {
		return err
	}
	issuer, err := verify.NewIssuerMatcher(Issuer, "")
	if err != nil {
		return err
	}
	id, err := verify.NewCertificateIdentity(san, issuer, certificate.Extensions{SourceRepositoryURI: Repository})
	if err != nil {
		return err
	}
	_, err = v.Verify(&b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(sums)), verify.WithCertificateIdentity(id)))
	var mismatch *verify.ErrNoMatchingCertificateIdentity
	switch {
	case errors.As(err, &mismatch):
		return fmt.Errorf("%w (want %s, issuer %s): %v", ErrIdentity, Identity(version), Issuer, err)
	case err != nil:
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	return nil
}

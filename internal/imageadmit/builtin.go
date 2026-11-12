// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/releasesig"
)

// BuiltinSigner is the signer of Wardyn's own images. It is always defined, never stored, and a site or
// profile may name it.
const BuiltinSigner = "wardyn-release"

// wardynImages are the repositories Wardyn's releases publish.
const wardynImages = "ghcr.io/cjohnstoniv/*"

// builtin is the release workflow at this build's tag, the identity the CLI's release check also pins. Its
// signature is the build-provenance attestation the release workflow pushes beside each image, or the legacy
// cosign .sig the same workflow signs, so its predicate set is exactly SLSA provenance v1: the same identity's
// SBOM attestation never counts. A development build (version "<release>+<commit>") has no release identity,
// so the signer covers no image.
func builtin(version string) SignerSpec {
	spec := SignerSpec{Name: BuiltinSigner, SignaturePredicates: []string{PredicateSLSAProvenanceV1},
		Keyless: &KeylessSpec{Issuer: releasesig.Issuer, Identity: releasesig.Identity(version),
			SourceRepository: releasesig.Repository, TrustedRoot: PublicGood}}
	if !strings.Contains(version, "+") {
		spec.Images = []string{wardynImages}
	}
	return spec
}

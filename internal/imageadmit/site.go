// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"errors"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SiteConfig is the trust part of the site setting images.admission as Compile takes it: every root and signer
// field under the same JSON name, copied. A nil block is the empty Config (the built-in signer only).
func SiteConfig(a *types.SiteImageAdmission) Config {
	if a == nil {
		return Config{}
	}
	var c Config
	for _, r := range a.TrustedRoots {
		c.TrustedRoots = append(c.TrustedRoots, RootSpec{Name: r.Name, Path: r.Path})
	}
	for _, s := range a.Signers {
		spec := SignerSpec{Name: s.Name, Images: slices.Clone(s.Images), PublicKey: s.PublicKey,
			RequireAttestations: slices.Clone(s.RequireAttestations), SignaturePredicates: slices.Clone(s.SignaturePredicates)}
		if s.Keyless != nil {
			spec.Keyless = &KeylessSpec{Issuer: s.Keyless.Issuer, Identity: s.Keyless.Identity, IdentityRegexp: s.Keyless.IdentityRegexp,
				SourceRepository: s.Keyless.SourceRepository, TrustedRoot: s.Keyless.TrustedRoot}
		}
		if s.TransparencyLog != nil {
			tl := *s.TransparencyLog
			spec.TransparencyLog = &tl
		}
		c.Signers = append(c.Signers, spec)
	}
	return c
}

// CheckSite compiles the block as run create will, so a setting the check would refuse at every run is refused
// when it is written: an empty images list, a reserved signer or root name (wardyn-release, public-good), a
// predicate type that is not an absolute URI, an unreadable trust root, and every other rule of Compile. The
// message names the signer or root and the rule, without the compiler's prefix. A nil block is valid.
func CheckSite(a *types.SiteImageAdmission, o Options) error {
	if a == nil {
		return nil
	}
	if _, err := Compile(SiteConfig(a), o); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), ErrPolicy.Error()+": "))
	}
	return nil
}

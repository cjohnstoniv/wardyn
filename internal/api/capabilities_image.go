// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capabilities_image.go — the image kind's deny and restriction readings that
// capabilities.go's resolver calls: spellings on the deny side, and a prefix
// allow that stops short of a restricted repository.

// capDenyOverlaps is capValueOverlaps for a DENY row tested against a request.
// An image deny is checked against every spelling of the requested image
// (imageSpellings: host case, ":443", Docker Hub short names), because a deny
// that one respelling walks around protects nothing. An allow never takes this
// path: it covers only the spelling written. Every other kind is capValueOverlaps.
func capDenyOverlaps(kind, denyValue, want string) bool {
	if kind != capImage {
		return capValueOverlaps(kind, denyValue, want)
	}
	forms := imageDenyForms(denyValue)
	for _, w := range imageSpellings(want) {
		for _, d := range forms {
			if capValueOverlaps(kind, d, w) {
				return true
			}
		}
	}
	return false
}

// onlyPrefixAllows reports whether every image allow covering value is a
// registry-prefix grant (none is the wildcard). Asked after scan loaded the
// grants and a covering allow was found.
func (b *capBatch) onlyPrefixAllows(kind, value string) bool {
	for _, g := range b.byKind[kind] {
		if g.Effect != types.CapabilityAllow || !capValueMatches(kind, g.Value, value) {
			continue
		}
		if _, ok := imagePrefixGrant(g.Value); !ok {
			return false
		}
	}
	return true
}

// imageNormalRepo is imageRepoAsTyped of value read through imageDenyForm, so a
// Docker Hub short name, alias or ":443" host names the same repository as its
// qualified spelling.
func imageNormalRepo(value string) (string, bool) {
	return imageRepoAsTyped(imageDenyForm(value))
}

// imageSiblingRestricted reports whether a restricted image shares value's
// repository (the reference less its tag and digest, normalised by
// imageNormalRepo). Asked after isRestricted loaded the restrictions.
func (b *capBatch) imageSiblingRestricted(value string) bool {
	return imageSiblingIn(b.restricted[capImage], value)
}

// imageSiblingIn is imageSiblingRestricted over a restriction set the caller
// read itself (the availability read shares it with the launch check).
func imageSiblingIn(restricted map[string]bool, value string) bool {
	repo, ok := imageNormalRepo(value)
	if !ok {
		return false
	}
	for v := range restricted {
		if r, ok := imageNormalRepo(v); ok && r == repo {
			return true
		}
	}
	return false
}

// imageRestrictedSpelling reports whether any spelling of value equals any
// form of a restricted image key: the restriction names one image, however
// the request spells it.
func imageRestrictedSpelling(value string, keys map[string]bool) bool {
	spellings := imageSpellings(value)
	for k := range keys {
		for _, f := range imageDenyForms(k) {
			if slices.Contains(spellings, f) {
				return true
			}
		}
	}
	return false
}

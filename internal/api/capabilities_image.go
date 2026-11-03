// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

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
	forms := []string{denyValue}
	if d := imageDenyForm(denyValue); d != denyValue {
		forms = append(forms, d)
	}
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

// imageSiblingRestricted reports whether a restricted image shares value's
// repository (the reference less its tag and digest, as typed). Asked after
// isRestricted loaded the restrictions.
func (b *capBatch) imageSiblingRestricted(value string) bool {
	repo, ok := imageRepoAsTyped(value)
	if !ok {
		return false
	}
	for v := range b.restricted[capImage] {
		if r, ok := imageRepoAsTyped(v); ok && r == repo {
			return true
		}
	}
	return false
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requiredImageSignersRefusal is the write-time check of a profile's, overlay's or restriction's
// required_image_signers: each name must be a signer the site defines (images.admission.signers) or the
// built-in wardyn-release, so a profile can require trust the site grants and never add any. field prefixes
// the message ("limits.required_image_signers"). It returns "" when the list may be written, and refuses a
// name it cannot check because the site config cannot be read. An empty list needs no site read.
//
// A signer removed from the site later is not found here: the members of such a profile fail closed at
// create, naming the missing signer.
func (s *Server) requiredImageSignersRefusal(ctx context.Context, field string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	if err := types.ValidRequiredImageSigners(names); err != nil {
		return field + ": " + err.Error()
	}
	sc, ok := s.siteConfigSnapshot(ctx)
	if !ok {
		return field + ": the site config could not be read to check the signer names — try again"
	}
	var defined []string
	if sc.Images != nil {
		defined = sc.Images.Admission.SignerNames()
	}
	for _, n := range names {
		if n != types.BuiltinImageSigner && !slices.Contains(defined, n) {
			return fmt.Sprintf("%s: %q is not an image signer this organisation defines — name a signer from site setting images.admission.signers, or %s", field, n, types.BuiltinImageSigner)
		}
	}
	return ""
}

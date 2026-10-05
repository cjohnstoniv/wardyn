// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The structural bound on a governance profile's eligible grants: a profile may
// NARROW the deployment's credential eligibility, never MINT new eligibility.
//
// The dominance contract itself is composer.GrantWithin; this file is the api's
// two entry points to it.
package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// governanceGrantsWithinCeiling reports whether every grant in profile is
// within some grant in ceiling (composer.GrantWithin), returning a caller-facing
// error naming the first grant that is not.
//
// Enforced at WRITE by the profile handlers. Config.DefaultPolicy is env-borne,
// so a redeploy that drops a pairing must not leave old profiles serving it
// forever — hence the resolve-time re-intersect, which calls this same
// function.
func governanceGrantsWithinCeiling(profile, ceiling []types.GrantSpec) error {
	for _, g := range profile {
		if err := governanceGrantWithinCeiling(g, ceiling); err != nil {
			return err
		}
	}
	return nil
}

// governanceGrantWithinCeiling is the per-grant half. The scope decode here is
// the stricter one (unknown keys, env var names), so a grant malformed for
// delivery is refused at write with the decode error before the shared contract
// judges it.
func governanceGrantWithinCeiling(g types.GrantSpec, ceiling []types.GrantSpec) error {
	if _, _, _, _, err := storedSecretGrantPairing(g); err != nil {
		return fmt.Errorf("eligible grant %q: invalid scope: %w", g.Kind, err)
	}
	return composer.GrantWithin(g, ceiling)
}

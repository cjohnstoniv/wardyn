// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Self-register the envelope-encrypted Postgres store as the default secret-store
// seam impl, so a blank import (cmd/wardynd) makes "pg" selectable.
func init() {
	secretstore.Register("pg", func(d secretstore.Deps) (secretstore.Store, error) {
		s := &Store{pool: d.Pool}
		if err := s.setLocalKeys(d.AgeIdentity, d.PlatformIdentity); err != nil {
			return nil, err
		}
		// A configured external store is read-only here: pointer rows written
		// in store mode stay readable, and every write seals locally (§2.2).
		s.ext, s.extTimeout = d.External, d.ExternalTimeout
		return s, nil
	})
}

// RegisterExternal registers name as a store-mode secret store: the same pg
// Store, writing every value to the external store the Deps carry (which must
// be the one called name) and keeping only a pointer row. The age identity is
// optional there; without it local rows are refused by name. Each external
// backend calls this from its own init().
func RegisterExternal(name string) {
	secretstore.Register(name, func(d secretstore.Deps) (secretstore.Store, error) {
		if d.External == nil || d.External.Name() != name {
			return nil, fmt.Errorf("secret store %q is selected but not configured (see docs/ENV.md, %q)", name, name)
		}
		s := &Store{pool: d.Pool, ext: d.External, writeExt: true, extTimeout: d.ExternalTimeout}
		if d.AgeIdentity != nil {
			if err := s.setLocalKeys(d.AgeIdentity, d.PlatformIdentity); err != nil {
				return nil, err
			}
		}
		return s, nil
	})
}

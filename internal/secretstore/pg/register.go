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
		// A configured external store is read-only here: rows still seal in
		// Postgres (§2.2), under the key service when it writes, else local KEKs.
		s := &Store{pool: d.Pool, ext: d.External, extTimeout: d.ExternalTimeout}
		if d.AgeIdentity != nil || !d.KEKWrites {
			if err := s.setLocalKeys(d.AgeIdentity, d.PlatformIdentity); err != nil {
				return nil, err
			}
		}
		// A key service alone (no age key) leaves local rows unreadable; wardynd refuses to boot while any exist.
		s.withKEK(d)
		return s, nil
	})
}

// RegisterExternal registers name as a store-mode secret store: the same pg
// Store, writing values to the external store named name and keeping only a
// pointer row. The age identity is optional; without it local rows are
// refused. A key service continues sealing rows, including what
// -migrate-secrets -to=local writes. Each external backend calls this from its
// own init().
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
		s.withKEK(d)
		return s, nil
	})
}

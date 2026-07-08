// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import "github.com/cjohnstoniv/wardyn/internal/secretstore"

// Self-register the age-encrypted Postgres store as the default secret-store
// seam impl, so a blank import (cmd/wardynd) makes "pg" selectable.
func init() {
	secretstore.Register("pg", func(d secretstore.Deps) (secretstore.Store, error) {
		s, err := New(d.Pool, d.AgeIdentity)
		if err != nil {
			return nil, err
		}
		return s, nil
	})
}

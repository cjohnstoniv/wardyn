// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
)

// The in-memory store every provider test runs against is held to the same
// contract as wardynd's Postgres store (cmd/wardynd's pgRevocations).
func TestMemRevocationStore_Conformance(t *testing.T) {
	identitytest.RunRevocationConformance(t, func(*testing.T) identity.RevocationStore {
		return identitytest.NewMemRevocationStore()
	})
}

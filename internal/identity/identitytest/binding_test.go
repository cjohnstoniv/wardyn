// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package identitytest_test

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
)

// The in-memory binding store every provider test runs against is held to the same contract as
// wardynd's Postgres store (internal/store's identity_token_bindings).
func TestMemBindingStore_Conformance(t *testing.T) {
	identitytest.RunBindingConformance(t,
		func(*testing.T) identity.TokenBindingStore { return identitytest.NewMemBindingStore() },
		func(*testing.T) identity.TokenBindingStore {
			s := identitytest.NewMemBindingStore()
			s.SetErr(errors.New("store down"))
			return s
		})
}

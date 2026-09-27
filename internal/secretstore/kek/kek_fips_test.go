// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// An external test package: secretstoretest imports secretstore, which imports
// kek, so an in-package test importing it is an import cycle.
package kek_test

import (
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

// TestNewLocal_RefusesUnderFIPSOnly: GODEBUG=fips140=only forbids X25519, and
// age swallows that error, leaving every identity with the same empty
// recipient. NewLocal must refuse rather than give every age key one kek_id.
func TestNewLocal_RefusesUnderFIPSOnly(t *testing.T) {
	if !secretstoretest.UnderFIPSOnly(t) {
		return
	}
	for range 2 {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		l, err := kek.NewLocal(id)
		if err == nil {
			t.Fatalf("NewLocal derived %s under fips140=only", l.ID())
		}
		if !strings.Contains(err.Error(), "WARDYN_SECRET_STORE=vaultkv") {
			t.Fatalf("the refusal does not name the FIPS-only way out: %v", err)
		}
	}
}

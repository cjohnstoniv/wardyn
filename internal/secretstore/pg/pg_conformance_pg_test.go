// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// The blessed default (pg) must pass the shared secret-store conformance suite —
// the same suite a future OpenBao/Vault/KMS backend will have to pass. Guarded by
// WARDYN_TEST_PG (via newPGStore, which Skips cleanly when unset).

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

func TestPG_Conformance(t *testing.T) {
	secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store {
		s, _, _ := newPGStore(t)
		return s
	})
}

func TestPG_TamperConformance(t *testing.T) {
	var pool *pgxpool.Pool
	secretstoretest.RunTamperConformance(t, func(t *testing.T) secretstore.Store {
		s, p, _ := newPGStore(t)
		pool = p
		return s
	}, func(t *testing.T, owner, name string) { secretstoretest.FlipCiphertext(pool)(t, owner, name) })
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_RunPATs runs the RunPATStore contract against Postgres (migration
// 0102), on a throwaway database so the listings start empty. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
func TestPG_RunPATs(t *testing.T) {
	runPATContract(t, store.NewPG(runsPGPoolIsolated(t)))
}

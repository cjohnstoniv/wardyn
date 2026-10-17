// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"maps"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The closedEnumChecks entry for runner_resident_runs.abandoned_reason is the
// store's own set, so the migration and live-catalog parity guards compare the
// CHECK against the constants the store writes.
func TestResidentAbandonReasonsMatchStore(t *testing.T) {
	want := map[string]bool{store.ResidentAbandonRunnerRevoked: true, store.ResidentAbandonRunnerDeleted: true, store.ResidentAbandonExpired: true}
	if got := db.ClosedEnumValues("runner_resident_runs", "abandoned_reason"); !maps.Equal(got, want) {
		t.Fatalf("closedEnumChecks runner_resident_runs.abandoned_reason = %v, want the store.ResidentAbandon* set %v", got, want)
	}
}

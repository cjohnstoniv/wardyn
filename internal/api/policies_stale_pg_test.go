// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestPG_PolicyPut_StaleCopyIsRefused runs the two-writer case against Postgres, where the
// store's updated_at guard, not the handler, refuses the save from the older copy.
func TestPG_PolicyPut_StaleCopyIsRefused(t *testing.T) {
	srv, _ := pgHarness(t)
	assertStaleSaveRefused(t, srv)
}

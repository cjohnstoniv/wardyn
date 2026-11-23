// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

// SetPortalGrantSweepBatchForTest sets the sweep's batch size and returns the function that restores it.
func SetPortalGrantSweepBatchForTest(n int) (restore func()) {
	old := portalGrantSweepBatch
	portalGrantSweepBatch = n
	return func() { portalGrantSweepBatch = old }
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "time"

// OpenKept is openKept without an engine: the tests use the travel identity, which asks the engine for nothing.
func OpenKept(cfg RunConfig, dir string, now time.Time) (Manifest, Identity, func(), error) {
	return openKept(nil, cfg, dir, now)
}

// SetKeptBootLockWait shortens how long a boot waits for the kept lock; restore undoes it.
func SetKeptBootLockWait(d time.Duration) (restore func()) {
	old := keptBootLockWait
	keptBootLockWait = d
	return func() { keptBootLockWait = old }
}

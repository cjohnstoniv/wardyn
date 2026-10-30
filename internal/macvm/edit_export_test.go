// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "context"

// LockUpdate holds base's update lock, as a prepare or a tools update does for its whole run.
func LockUpdate(baseDir string) (unlock func(), err error) {
	return lockFile(context.Background(), updateLockPath(baseDir), true)
}

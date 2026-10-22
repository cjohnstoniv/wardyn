// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package filelock is a non-blocking exclusive advisory lock on an open file:
// flock(2) on unix, LockFileEx on Windows. The lock dies with the descriptor.
package filelock

import "errors"

// ErrLocked means another descriptor holds the lock.
var ErrLocked = errors.New("file is locked by another holder")

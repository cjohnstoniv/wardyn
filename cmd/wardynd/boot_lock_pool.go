// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// maxLockPoolConns caps WARDYN_LOCK_POOL_MAX_CONNS: three replicas at it already pass Postgres's default 100 connections.
const maxLockPoolConns = 64

// applyLockPoolSize sets db.LockPoolMaxConns before anything dials a lock connection (db.LockerFor sizes on first use).
func applyLockPoolSize(n int) error {
	if n < 1 || n > maxLockPoolConns {
		return fmt.Errorf("refusing to start: WARDYN_LOCK_POOL_MAX_CONNS is %d; want an integer from 1 to %d", n, maxLockPoolConns)
	}
	db.LockPoolMaxConns = n
	return nil
}

// applyLockPoolFlag is applyLockPoolSize for the boot flag. A bootFlags built without registering it, as a test
// does, leaves the pointer nil, which keeps the default.
func applyLockPoolFlag(n *int) error {
	if n == nil {
		return nil
	}
	return applyLockPoolSize(*n)
}

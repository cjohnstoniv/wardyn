// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

func TestLockPoolMaxConnsRefusesOutOfRange(t *testing.T) {
	restore := db.LockPoolMaxConns
	t.Cleanup(func() { db.LockPoolMaxConns = restore })
	for _, n := range []int{0, -1, 65} {
		db.LockPoolMaxConns = 8
		if err := applyLockPoolSize(n); err == nil || !strings.Contains(err.Error(), "WARDYN_LOCK_POOL_MAX_CONNS") {
			t.Errorf("applyLockPoolSize(%d) = %v, want a refusal naming the variable", n, err)
		}
		if db.LockPoolMaxConns != 8 {
			t.Errorf("applyLockPoolSize(%d) changed the size to %d", n, db.LockPoolMaxConns)
		}
	}
	for _, n := range []int{1, 8, 64} {
		if err := applyLockPoolSize(n); err != nil || db.LockPoolMaxConns != n {
			t.Errorf("applyLockPoolSize(%d) = %v, size %d; want it set", n, err, db.LockPoolMaxConns)
		}
	}
}

// db.LockerFor sizes its connection pool on first use, so the size is applied before anything can ask for a lock:
// applyLockPoolSize must come before connectAndMigrate in main.go.
func TestLockPoolSizeIsAppliedBeforeTheConnect(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	apply, connect := strings.Index(string(src), "applyLockPoolSize("), strings.Index(string(src), "connectAndMigrate(")
	if apply < 0 || connect < 0 || apply > connect {
		t.Fatalf("applyLockPoolSize( at %d, connectAndMigrate( at %d; want the first before the second", apply, connect)
	}
}

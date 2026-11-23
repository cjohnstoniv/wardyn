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
// validateBootPosture applies it, and run() calls that before connectAndMigrate.
func TestLockPoolSizeIsAppliedBeforeTheConnect(t *testing.T) {
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	posture, err := os.ReadFile("boot_posture.go")
	if err != nil {
		t.Fatal(err)
	}
	validate, connect := strings.Index(string(main), "validateBootPosture("), strings.Index(string(main), "connectAndMigrate(")
	if validate < 0 || connect < 0 || validate > connect {
		t.Fatalf("validateBootPosture( at %d, connectAndMigrate( at %d in main.go; want the first before the second", validate, connect)
	}
	if !strings.Contains(string(posture), "applyLockPoolFlag(f.lockPoolMaxConns)") {
		t.Fatal("validateBootPosture does not apply the lock pool size")
	}
}

// A bootFlags built without the flag, as the posture tests build theirs, keeps the default and is not refused.
func TestLockPoolFlagUnregisteredKeepsTheDefault(t *testing.T) {
	restore := db.LockPoolMaxConns
	t.Cleanup(func() { db.LockPoolMaxConns = restore })
	db.LockPoolMaxConns = 8
	if err := applyLockPoolFlag(nil); err != nil || db.LockPoolMaxConns != 8 {
		t.Errorf("applyLockPoolFlag(nil) = %v, size %d; want nil and 8", err, db.LockPoolMaxConns)
	}
	n := 12
	if err := applyLockPoolFlag(&n); err != nil || db.LockPoolMaxConns != 12 {
		t.Errorf("applyLockPoolFlag(12) = %v, size %d; want it set", err, db.LockPoolMaxConns)
	}
}

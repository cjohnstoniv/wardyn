// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockExcludesASecondDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.lock")
	a, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := TryLock(a); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if err := TryLock(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := TryLock(b); err != nil {
		t.Fatalf("lock after release: %v", err)
	}
}

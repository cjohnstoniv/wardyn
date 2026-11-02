// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvmtest

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockAux takes an exclusive flock on path, as the framework's VM locks its
// aux storage: it is gone only when f is closed or its process exits.
func lockAux(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrAuxLocked
	}
	return f, nil
}

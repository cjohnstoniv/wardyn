// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockSingleInstance takes an exclusive lock on serve.lock in the runner state directory for the life of
// the process (the kernel drops it when the process dies), so a second serve on one identity stops with a
// clear message. The returned func releases it.
func lockSingleInstance(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another wardyn-runnerd serve is already running for %s: stop it first (a setup-installed and a package-installed agent must not both run)", stateDir)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

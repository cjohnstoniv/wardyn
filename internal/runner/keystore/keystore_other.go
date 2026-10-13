// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package keystore

// No supported keystore off Linux and macOS: runner_resident is refused there.

// Set refuses: this platform has no supported keystore.
func Set(string, string) error { return ErrUnavailable }

// Get refuses: this platform has no supported keystore.
func Get(string) (string, error) { return "", ErrUnavailable }

// Delete refuses: this platform has no supported keystore.
func Delete(string) error { return ErrUnavailable }

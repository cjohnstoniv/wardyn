// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package keystore

// ponytail: no keystore off Linux. The macOS Keychain needs cgo or the
// `security` tool, and the runner ships as a static binary that runs no OS
// tool; a macOS runner refuses runner_resident until one of those changes.

// Set refuses: this platform has no supported keystore.
func Set(string, string) error { return ErrUnavailable }

// Get refuses: this platform has no supported keystore.
func Get(string) (string, error) { return "", ErrUnavailable }

// Delete refuses: this platform has no supported keystore.
func Delete(string) error { return ErrUnavailable }

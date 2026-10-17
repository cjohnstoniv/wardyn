// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package keystore

// No supported keystore off Linux and macOS: runner_resident is refused there.

// Precheck refuses: this platform has no supported keystore.
func (Store) Precheck(string, string) error { return ErrUnavailable }

// Set refuses: this platform has no supported keystore.
func (Store) Set(string, string) error { return ErrUnavailable }

// Get refuses: this platform has no supported keystore.
func (Store) Get(string) (string, error) { return "", ErrUnavailable }

// Delete refuses: this platform has no supported keystore.
func (Store) Delete(string) error { return ErrUnavailable }

// DeleteAll refuses: this platform has no supported keystore.
func (Store) DeleteAll() error { return ErrUnavailable }

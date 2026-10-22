// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// keychainReachable: macOS and Windows always have a keychain; go-keyring's own call reports a failure.
func keychainReachable() error { return nil }

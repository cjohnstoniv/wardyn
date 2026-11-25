// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !(darwin && arm64 && cgo)

package main

import "errors"

// keychainIdentities reads the System keychain; this build cannot.
type keychainIdentities struct{}

func (keychainIdentities) Identities() ([]deviceIdentity, error) {
	return nil, errors.New("device identities are read only on a Mac")
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package keystore

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// The keyring calls, swappable so a test can prove they are never reached.
var (
	keyringSet    = keyring.Set
	keyringGet    = keyring.Get
	keyringDelete = keyring.Delete
)

// Set stores value under account: the Secret Service over D-Bus on Linux; on
// macOS the login Keychain through /usr/bin/security, which go-keyring v0.2.8
// hands the value over stdin (`security -i`), never in argv. That command line
// is capped at 4096 bytes, so a value over about 3 KB is refused, never sent
// another way.
func Set(account, value string) error {
	if err := ready(); err != nil {
		return err
	}
	if err := keyringSet(service, account, value); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Get returns the value stored under account.
func Get(account string) (string, error) {
	if err := ready(); err != nil {
		return "", err
	}
	v, err := keyringGet(service, account)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return v, nil
}

// Delete removes account; one that is already gone is not an error.
func Delete(account string) error {
	if err := ready(); err != nil {
		return err
	}
	if err := keyringDelete(service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

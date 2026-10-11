// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package keystore

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Set stores value under account in the Secret Service (D-Bus; no process is run).
func Set(account, value string) error {
	if err := keyring.Set(service, account, value); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Get returns the value stored under account.
func Get(account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return v, nil
}

// Delete removes account; one that is already gone is not an error.
func Delete(account string) error {
	if err := keyring.Delete(service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

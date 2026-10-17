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
	keyringSet       = keyring.Set
	keyringGet       = keyring.Get
	keyringDelete    = keyring.Delete
	keyringDeleteAll = keyring.DeleteAll
)

// Precheck refuses, before any OS call, a value this keystore cannot take: no
// keystore is reachable, or (macOS) the value is too long for the keychain
// command.
func (s Store) Precheck(account, value string) error {
	if err := ready(); err != nil {
		return err
	}
	return precheck(s.service, account, value)
}

// Set stores value under account: the Secret Service over D-Bus on Linux; on
// macOS the login Keychain through /usr/bin/security, which go-keyring v0.2.8
// hands the value over stdin (`security -i`), never in argv. That command line
// is capped at 4096 bytes, so a value over about 3 KB is refused by Precheck
// before the child process starts, never sent another way.
func (s Store) Set(account, value string) error {
	if err := s.Precheck(account, value); err != nil {
		return err
	}
	if err := keyringSet(s.service, account, value); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Get returns the value stored under account.
func (s Store) Get(account string) (string, error) {
	if err := ready(); err != nil {
		return "", err
	}
	v, err := keyringGet(s.service, account)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return v, nil
}

// Delete removes account; one that is already gone is not an error.
func (s Store) Delete(account string) error {
	if err := ready(); err != nil {
		return err
	}
	if err := keyringDelete(s.service, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// DeleteAll removes every item under this runner's service, and no other.
func (s Store) DeleteAll() error {
	if err := ready(); err != nil {
		return err
	}
	if err := keyringDeleteAll(s.service); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

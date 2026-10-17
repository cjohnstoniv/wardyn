// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

// ready: the login Keychain needs no discovery; `security` is the one tool the
// runner runs (owner ruling), and only through go-keyring.
func ready() error { return nil }

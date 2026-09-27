// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"

// Self-register "vaultkv" as a store-mode secret store: the pg store, writing
// every value to the Vault client wardynd builds from WARDYN_VAULT_*.
func init() { secretstorepg.RegisterExternal(Name) }

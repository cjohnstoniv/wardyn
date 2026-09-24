// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"

// Self-register "azurekv" as a store-mode secret store: the pg store, writing
// every value to the Key Vault client wardynd builds from WARDYN_AZURE_*.
func init() { secretstorepg.RegisterExternal(Name) }

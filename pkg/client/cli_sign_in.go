// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "github.com/cjohnstoniv/wardyn/internal/types"

// CLISignInHealthz is what GET /healthz says about CLI sign-in (wardyn login): whether it is on, whether
// paste-back is, and the origin device proofs are checked against.
type CLISignInHealthz = types.CLISignInHealthz

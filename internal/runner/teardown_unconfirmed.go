// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// TeardownUnconfirmedError is a stop, kill or end the substrate carried out but could not see finish, so
// the sandbox is not known to have stopped and keeps what it holds; the same call made again tries again.
// Err says why; the error reads as Err.
type TeardownUnconfirmedError struct{ Err error }

func (e *TeardownUnconfirmedError) Error() string { return e.Err.Error() }

func (e *TeardownUnconfirmedError) Unwrap() error { return e.Err }

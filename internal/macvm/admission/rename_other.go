// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux && !darwin

package admission

import "errors"

// renameNoReplace has no one-step no-replace rename here, so every move
// fails closed.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return errors.ErrUnsupported
}

// renameSwap has no one-step exchange here, so it fails closed.
func renameSwap(dirfd int, a, b string) error { return errors.ErrUnsupported }

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package admission

import "errors"

// renameNoReplace has no one-step no-replace rename here, so every move
// fails closed.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return errors.ErrUnsupported
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package macvm

import (
	"context"
	"errors"
)

// hostExec refuses: the guest agent is signed only on macOS.
func hostExec(context.Context, []string) ([]byte, error) {
	return nil, errors.New("macvm: the guest agent is signed only on macOS")
}

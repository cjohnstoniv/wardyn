// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package macvm

import (
	"context"
	"os/exec"
)

// hostExec runs argv on this Mac and returns its combined output.
func hostExec(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

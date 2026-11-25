// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package localviewer

import (
	"context"
	"io"
)

// Available is ErrUnsupportedOS: the window is drawn through X11, on Linux only.
func Available() error { return ErrUnsupportedOS }

// Run is ErrUnsupportedOS: the window is drawn through X11, on Linux only. conn is closed.
func Run(_ context.Context, conn io.ReadWriteCloser, _ string) error {
	_ = conn.Close()
	return ErrUnsupportedOS
}

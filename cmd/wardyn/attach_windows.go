// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"context"
	"time"
)

// resizePollInterval is how often Windows is asked for the console size: it has
// no SIGWINCH.
const resizePollInterval = 250 * time.Millisecond

// watchResize sends on out each time the console size changes, by polling. The
// returned func stops the watch.
func watchResize(ctx context.Context, fd int, out chan<- struct{}) func() {
	return pollResize(ctx, resizePollInterval, func() (int, int, error) { return getSizeFn(fd) }, out)
}

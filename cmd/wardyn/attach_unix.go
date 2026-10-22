// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchResize sends on out each time the terminal is resized (SIGWINCH). The
// returned func stops the watch.
func watchResize(ctx context.Context, _ int, out chan<- struct{}) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}

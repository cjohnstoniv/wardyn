// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

// pollResize calls size every interval and sends on out when the result
// differs from the previous one. It is the resize source where the OS has no
// SIGWINCH. The returned func stops the poll.
func pollResize(ctx context.Context, interval time.Duration, size func() (int, int, error), out chan<- struct{}) func() {
	lastCols, lastRows, _ := size()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				cols, rows, err := size()
				if err != nil || (cols == lastCols && rows == lastRows) {
					continue
				}
				lastCols, lastRows = cols, rows
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return func() { close(done) }
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"time"
)

// attachFlow is the client's output flow control (ttyd's model): between a
// `pause` and the next `resume` frame the output goroutine does not read the
// exec, so a client that cannot render as fast as the sandbox writes pushes back
// on the PTY instead of buffering without bound.
//
// A pause that is never resumed is bounded exactly like a blocked write
// (attachWriteTimeout): the stall callback ends the pump. The bound runs on its
// own timer, off the output goroutine, so it holds while that goroutine waits.
type attachFlow struct {
	mu    sync.Mutex
	gate  chan struct{} // non-nil while paused; closed on resume
	stall *time.Timer
}

// pause stops output reads and arms the stall bound; a pause while already
// paused keeps the first timer, so repeated frames never extend it.
func (f *attachFlow) pause(limit time.Duration, onStall func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		return
	}
	f.gate = make(chan struct{})
	f.stall = time.AfterFunc(limit, onStall)
}

// resume lets output flow again and disarms the stall bound.
func (f *attachFlow) resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		return
	}
	close(f.gate)
	f.gate = nil
	f.stall.Stop()
}

// stop disarms the stall bound when the pump ends.
func (f *attachFlow) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stall != nil {
		f.stall.Stop()
	}
}

// wait blocks while paused and reports false when ctx ended first, which is how
// cancellation and revocation (a take-over closes the socket, which ends the
// pump) wake a paused output goroutine.
func (f *attachFlow) wait(ctx context.Context) bool {
	for {
		f.mu.Lock()
		g := f.gate
		f.mu.Unlock()
		if g == nil {
			return ctx.Err() == nil
		}
		select {
		case <-g:
		case <-ctx.Done():
			return false
		}
	}
}

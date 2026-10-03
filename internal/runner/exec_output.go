// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "io"

// OutputDrainer is the optional second half of SandboxSpec.ExecOutput's
// contract: a writer that also implements it is told when a driver starts and
// ends a copy into it, so its owner can tell the end of the output from the end
// of the process. A process's exit is not the end of its output: the driver's
// Wait returns when the process exits, and the copy goroutine reaches EOF on
// its own schedule.
//
// A writer that implements only io.Closer is closed when the copy ends, which
// is the same signal without the start (BeginOutputDrain).
type OutputDrainer interface {
	// BeginDrain is called before the copy starts, before Exec or CreateSandbox
	// returns, so a caller that waits for the process then sees the drain open.
	BeginDrain()
	// EndDrain is called once, when the copy returns: err is nil at EOF and the
	// copy's error otherwise. Neither call may block.
	EndDrain(err error)
}

// BeginOutputDrain starts one drain of w and returns the func the driver calls,
// exactly once, when its copy returns (nil at EOF, the copy's error otherwise).
// A nil w, or one that implements neither OutputDrainer nor io.Closer, gets a
// func that does nothing. Every driver goes through it, so the contract has one
// reading.
func BeginOutputDrain(w io.Writer) (end func(err error)) {
	switch d := w.(type) {
	case OutputDrainer:
		d.BeginDrain()
		return d.EndDrain
	case io.Closer:
		return func(error) { _ = d.Close() }
	}
	return func(error) {}
}

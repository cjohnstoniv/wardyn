// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"io"
)

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

// ErrOutputUnrecoverable is RecoverOutput's answer when the substrate keeps no
// copy of the agent's output to read back (a Docker exec agent: its output is a
// hijacked stream dockerd does not log). The caller records a capture gap; the
// agent is never re-run to get it.
var ErrOutputUnrecoverable = errors.New("runner: this run's output cannot be recovered from the substrate")

// OutputRecoverer is an OPTIONAL Runner capability: a process that holds no tail
// for a run (it restarted, or another replica adopted the run) reads the agent's
// output back from the substrate into w. It reads the whole log from its start,
// so w must be a fresh writer; a log still being written is followed until the
// agent exits, a finished one is read to its end. ctx bounds the read: the
// caller gives it a deadline, and an implementation MUST return when it ends.
//
// It returns ErrOutputUnrecoverable when the substrate keeps no log for this
// agent, ErrSandboxGone when the sandbox no longer exists, and otherwise nil at
// the end of the log or the error that cut the read short (a partial read is
// not an error to discard: the bytes already written to w stand).
type OutputRecoverer interface {
	RecoverOutput(ctx context.Context, ref string, w io.Writer) error
}

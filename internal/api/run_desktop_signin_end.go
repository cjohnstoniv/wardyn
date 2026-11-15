// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signinSettleGaps space the reads of pid 1's state after its SIGCONT. dockerd
// applies a kill whose client has gone (ContainerKill takes no context), in the
// order the handlers take the container's lock, so a SIGSTOP that answered an
// error can land after the SIGCONT; one held back for longer than this is missed.
var signinSettleGaps = [...]time.Duration{100 * time.Millisecond, 400 * time.Millisecond}

// signinMainState is pid 1's stat line, read in the pid namespace its signals land in.
var signinMainState = []string{"/bin/cat", "/proc/1/stat"}

var errSigninStateUnread = errors.New("the agent's state could not be read")

// errSigninResumeStage marks an end that failed at its "resume" stage, after
// the display was proven closed: handleEndDesktopSignin answers it with its own
// message.
var errSigninResumeStage = errors.New("the agent could not be resumed")

// continueMain sends pid 1 a SIGCONT, then reads its state through
// signinSettleGaps: nil only when the last read finds it running, and a read
// that finds it stopped sends one more SIGCONT. errSigninStateUnread: the
// SIGCONT was accepted and a read could not be made. The read runs as the
// person's uid, whose exec the agent can neither signal, trace nor write to.
func (s *Server) continueMain(ctx context.Context, se runner.SigninExecer, run types.AgentRun) error {
	ref := run.SandboxRef
	if err := se.SignalMain(ctx, ref, "SIGCONT"); err != nil {
		return err
	}
	for i := 0; ; i++ {
		out, err := s.sandboxExec(ctx, run, signinAsPerson, nil, 4<<10, signinMainState)
		line := string(out)
		j := strings.LastIndexByte(line, ')')
		if err != nil || j < 0 || j+2 >= len(line) {
			return errors.Join(errSigninStateUnread, err)
		}
		stopped := line[j+2] == 'T' // a job-control stop, which SIGCONT ends
		switch {
		case i == len(signinSettleGaps) && stopped:
			return errors.New("the agent is still stopped")
		case i == len(signinSettleGaps):
			return nil
		case stopped:
			if err := se.SignalMain(ctx, ref, "SIGCONT"); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return errors.Join(errSigninStateUnread, ctx.Err())
		case <-time.After(signinSettleGaps[i]):
		}
	}
}

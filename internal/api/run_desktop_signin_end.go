// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// errSigninEndUnrecorded marks an end whose display closed and whose agent was
// continued, but whose row could not be closed.
var errSigninEndUnrecorded = errors.New("the sign-in's end could not be recorded")

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

// signinEndHold bounds an end that claimed its row (claimSigninEnd). An end runs
// at most five execs, each within signinExecTimeout, so one still running when
// its hold passes is waiting on a signal that hung: its context is done there,
// and a later end may claim the row from it.
const signinEndHold = 6 * time.Minute

// signinBeginHold bounds a begin's steps up to its pause record
// (handleBeginDesktopSignin): its SIGSTOP and the record's write run within it,
// and pause-agent between them within signinExecTimeout. An end leaves a row
// with no pause record to its begin for twice as long (handleEndDesktopSignin),
// a margin for the clocks of two replicas.
var signinBeginHold = 3 * time.Minute

// signinStageEnding is the end_stage of a row an end has claimed:
// "ending:<the unix second its hold passes>".
const signinStageEnding = "ending"

// errSigninEndClaimed: another end holds the sign-in's row, or the row closed,
// so this end sent nothing.
var errSigninEndClaimed = errors.New("another end holds the sign-in")

// signinEndClaimLive reports whether endStage is an end's claim whose hold has
// not passed at now.
func signinEndClaimLive(endStage string, now time.Time) bool {
	rest, ok := strings.CutPrefix(endStage, signinStageEnding+":")
	if !ok {
		return false
	}
	until, err := strconv.ParseInt(rest, 10, 64)
	return err == nil && now.Before(time.Unix(until, 0))
}

// claimSigninEnd claims si's row for one end, before any of its signals. Ends
// that no failure has marked (the person's, a begin's rollback, abandon or
// unstarted close, the sweep's once the window passed) can run at once, and a
// signal from one whose row another end closed could undo a newer sign-in's
// stop. While the claim holds, every other such end loses its own and sends
// nothing; and the store allows one open row per run, so no newer sign-in can
// begin before the claimer closes the row. It returns ctx bounded by
// signinEndHold, and release, which ends that bound and clears the claim when
// the end left the row open and unmarked, so the next end need not wait out
// the hold. errSigninEndClaimed: another end holds the row, or it closed. A
// store that cannot take the claim cannot close the row either, so the end
// then runs unclaimed with a release that does nothing: a store fault never
// leaves the agent stopped.
func (s *Server) claimSigninEnd(ctx context.Context, st store.DesktopSigninStore, si types.DesktopSignin) (context.Context, func(), error) {
	now := s.cfg.Now()
	if signinEndClaimLive(si.EndStage, now) {
		return ctx, nil, errSigninEndClaimed
	}
	claim := signinStageEnding + ":" + strconv.FormatInt(now.Add(signinEndHold).Unix(), 10)
	claimed, err := st.ClaimDesktopSigninEnd(ctx, si.ID, si.EndStage, claim)
	if err != nil {
		return ctx, func() {}, nil
	}
	if !claimed {
		return ctx, nil, errSigninEndClaimed
	}
	held, cancel := context.WithTimeout(ctx, signinEndHold)
	return held, func() {
		cancel()
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), signinExecTimeout)
		defer rcancel()
		_, _ = st.ClaimDesktopSigninEnd(rctx, si.ID, claim, "")
	}, nil
}

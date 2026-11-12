// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/google/uuid"
)

// The window and the run's settings. A macOS VM guest applies the run's non-secret settings to the window's
// desktop session after Init answers, once the agent account is at the console, and keeps where that
// stands in a status file (guestagent.SessionEnvStatus). The control plane reads that file over the same
// guest exec channel the prep uses, and holds the window until the settings are set or failed:
//
//   - the at-start window waits, in its own background goroutine, up to macGUIEnvWait; the run's startup
//     is never held;
//   - an open asked for by hand while the settings are pending is refused with a sentence, and the same
//     sentence is the run's window status;
//   - failed does not hold the window: it opens, and the status says what failed and how to retry;
//   - an at-start wait that ends with no answer after the guest said pending (the wait ran out, or the status
//     could not be read again) counts as failed, so the window opens and says the same.
//
// A guest that never says (an older one, a paused or unreachable VM) holds nothing and adds no sentence.
var (
	// macGUIEnvPollEvery is how often the at-start wait asks the guest.
	macGUIEnvPollEvery = 2 * time.Second
	// macGUIEnvWait bounds that wait: the guest's own console wait (ten minutes) and a margin, so the guest
	// has said failed by then.
	macGUIEnvWait = 10*time.Minute + 30*time.Second
	// macGUIEnvCacheTTL is how long a pending or failed answer stands for reads of the run's window state.
	// A set answer never changes back, so it stands for good.
	macGUIEnvCacheTTL = 3 * time.Second
)

const (
	macWindowSettingsPendingMsg = "This run's desktop settings are still being applied; the window opens when they are."
	macGUIEnvReadWait           = 10 * time.Second
)

// macGUIEnvArgv reads the guest's status file.
var macGUIEnvArgv = []string{"/bin/cat", guestagent.MacSessionEnvStatusPath}

type macGUIEnvEntry struct {
	st      guestagent.SessionEnvStatus
	at      time.Time
	unknown bool // the guest could not say
}

// macGUIEnvSeen is the last answer per run, by run id.
var macGUIEnvSeen sync.Map

// macGUIEnvGaveUp marks a run, by run id, whose at-start wait ended with no answer after the guest said
// pending: it counts as failed from then on, unless the guest later says set.
var macGUIEnvGaveUp sync.Map

// macGUIEnvForget drops the run's entries; called when the run ends (revokeRunSteps, killTeardownTail).
func macGUIEnvForget(runID uuid.UUID) {
	macGUIEnvSeen.Delete(runID)
	macGUIEnvGaveUp.Delete(runID)
}

// macGUIEnvWaited marks a context whose at-start wait is over, so the open it carries is not refused for
// settings the wait gave up on.
type macGUIEnvWaited struct{}

// macGUIEnvStatus is the run's settings status as of the last 3 s (a set one for good), read from the
// guest when there is none; false when the guest cannot say.
func (s *Server) macGUIEnvStatus(ctx context.Context, run types.AgentRun) (guestagent.SessionEnvStatus, bool) {
	if v, ok := macGUIEnvSeen.Load(run.ID); ok {
		if e := v.(macGUIEnvEntry); e.st.State == guestagent.SessionEnvSet || time.Since(e.at) < macGUIEnvCacheTTL {
			return e.st, !e.unknown
		}
	}
	return s.macGUIEnvFresh(ctx, run)
}

// macGUIEnvFresh asks the guest and remembers the answer.
func (s *Server) macGUIEnvFresh(ctx context.Context, run types.AgentRun) (guestagent.SessionEnvStatus, bool) {
	st, ok := s.macGUIEnvExec(ctx, run)
	macGUIEnvSeen.Store(run.ID, macGUIEnvEntry{st: st, at: time.Now(), unknown: !ok})
	return st, ok
}

// macGUIEnvExec cats the guest's status file over ExecStream (never Exec, which registers an agent process).
func (s *Server) macGUIEnvExec(ctx context.Context, run types.AgentRun) (guestagent.SessionEnvStatus, bool) {
	if s.cfg.Runner == nil || run.SandboxRef == "" {
		return guestagent.SessionEnvStatus{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, macGUIEnvReadWait)
	defer cancel()
	sess, err := s.cfg.Runner.ExecStream(cctx, run.SandboxRef, runner.ExecSpec{Argv: macGUIEnvArgv})
	if err != nil || sess == nil {
		return guestagent.SessionEnvStatus{}, false
	}
	defer func() { _ = sess.Close() }()
	stop := context.AfterFunc(cctx, func() { _ = sess.Close() })
	defer stop()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	var out, errOut tailBuffer
	var wg sync.WaitGroup
	for _, d := range []struct {
		r   io.Reader
		dst *tailBuffer
	}{{sess.Stdout, &out}, {sess.Stderr, &errOut}} {
		if d.r == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(d.dst, d.r) // drained to the end: an undrained stream blocks the exec's Wait
		}()
	}
	wg.Wait()
	if exit, err := sess.Wait(); err != nil || exit != 0 {
		return guestagent.SessionEnvStatus{}, false
	}
	st, err := guestagent.ParseSessionEnvStatus(out.b)
	return st, err == nil
}

// macGUIEnvRefusal is the window row for settings still being applied: nil unless the guest says pending
// and the at-start wait (macGUIEnvAwait) is not the caller. A window already open is never closed by it
// (reopen: the re-check of an open window).
func (s *Server) macGUIEnvRefusal(ctx context.Context, run types.AgentRun, reopen bool) *runRefusal {
	if reopen || ctx.Value(macGUIEnvWaited{}) != nil {
		return nil
	}
	if _, gaveUp := macGUIEnvGaveUp.Load(run.ID); gaveUp {
		return nil // counts as failed, which never holds the window
	}
	if st, ok := s.macGUIEnvStatus(ctx, run); ok && st.State == guestagent.SessionEnvPending {
		return runError(409, reasonMacWindowPreparing, macWindowSettingsPendingMsg)
	}
	return nil
}

// macGUIEnvAwait holds the at-start window until the guest says set or failed, for at most macGUIEnvWait,
// and returns the context the open goes on with. It runs in the launch's own goroutine.
func (s *Server) macGUIEnvAwait(ctx context.Context, run types.AgentRun) context.Context {
	deadline := time.Now().Add(macGUIEnvWait)
	for saidPending := false; ; saidPending = true {
		st, ok := s.macGUIEnvFresh(ctx, run)
		if ok && st.State != guestagent.SessionEnvPending {
			break // set or failed: the window state says which
		}
		if !ok && !saidPending {
			break // a guest that cannot say (an older one) holds nothing
		}
		if !ok || !time.Now().Before(deadline) || ctx.Err() != nil {
			macGUIEnvGaveUp.Store(run.ID, true) // pending, then no answer: the window opens and says so
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(macGUIEnvPollEvery):
		}
	}
	return context.WithValue(ctx, macGUIEnvWaited{}, true)
}

// macGUIEnvNote words a failed set on the window state of a run whose window may open: the reason and the
// plain retry. An at-start wait that gave up (macGUIEnvGaveUp) is worded the same, unless the guest has since
// said set.
func (s *Server) macGUIEnvNote(ctx context.Context, run types.AgentRun, v *macWindowView) {
	if v.State != macWindowStateOpen || v.Message != "" {
		return
	}
	st, ok := s.macGUIEnvStatus(ctx, run)
	reason := ""
	if ok && st.State == guestagent.SessionEnvFailed {
		reason = macGUIEnvReason(st.Reason)
	} else if _, gaveUp := macGUIEnvGaveUp.Load(run.ID); gaveUp && (!ok || st.State != guestagent.SessionEnvSet) {
		reason = "they were not confirmed in time"
	}
	if reason != "" {
		v.Message = "This run's settings could not be applied to the window's desktop (" + reason +
			"), so apps opened there may not use the run's proxy. To retry, run agent-run --session-env-retry in the window's Terminal."
	}
}

func macGUIEnvReason(code string) string {
	switch code {
	case guestagent.SessionEnvNotSignedIn:
		return "the agent account was not signed in"
	case guestagent.SessionEnvSetenvFailed:
		return "the desktop session did not take them"
	}
	return "unknown reason"
}

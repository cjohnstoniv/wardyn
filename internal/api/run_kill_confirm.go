// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's kill on the in-process Kubernetes runner can find the sandbox's pods already terminating
// when its wait runs out (runner.ErrTeardownConfirming). That is not a failure: the run is KILLED, the
// revocations ran, and the kill answers 202 with `teardown: confirming`. startKillConfirm then re-runs the
// idempotent sandbox kill in the background until the pods are gone or killConfirmWindow from the kill's
// start has passed, and writes one run.kill.confirm row either way. The terminal sweep stays the backstop
// for a ref the confirm leaves held.

// killConfirmWindow is how long after the kill's start the background confirm keeps trying; killConfirmRetry
// is its wait between tries. Variables so a test can shorten them.
var (
	killConfirmWindow = 60 * time.Second
	killConfirmRetry  = 2 * time.Second
)

// killConfirms holds the run ids whose confirm is running in this process: a second kill of such a run
// answers the same 202 and starts nothing.
var killConfirms sync.Map

// errKillConfirmBusy is a confirm try that did not get the run's lock: someone else is working the run.
var errKillConfirmBusy = errors.New("the run's lock is held")

// killConfirm rides the kill cascade's context from handleKillRun to killTeardownTail and back: the
// tail marks it when the sandbox stop is still being confirmed, and the handler reads it for its answer.
// A context value rather than a return value, because killTeardownTail and killRunCascade have many
// other callers and a key in the tail's error map would read as a failure to each of them.
type killConfirm struct {
	start      time.Time
	confirming atomic.Bool
}

type killConfirmKey struct{}

// withKillConfirm marks the kill as starting now, for handleKillRun.
func withKillConfirm(ctx context.Context) context.Context {
	return context.WithValue(ctx, killConfirmKey{}, &killConfirm{start: time.Now()})
}

// markKillConfirming is killTeardownTail's note that the sandbox stop is not yet confirmed; a no-op on a
// cascade whose caller did not ask (withKillConfirm).
func markKillConfirming(ctx context.Context) {
	if kc, ok := ctx.Value(killConfirmKey{}).(*killConfirm); ok {
		kc.confirming.Store(true)
	}
}

// writeKillAccepted is handleKillRun's 202: a kill whose sandbox stop is still being confirmed says so and
// starts the background confirm.
func (s *Server) writeKillAccepted(ctx context.Context, w http.ResponseWriter, run types.AgentRun) {
	body := map[string]any{"id": run.ID, "state": types.RunKilled}
	if kc, ok := ctx.Value(killConfirmKey{}).(*killConfirm); ok && kc.confirming.Load() {
		body["teardown"] = "confirming"
		s.startKillConfirm(run, kc.start)
	}
	writeJSON(w, http.StatusAccepted, body)
}

// startKillConfirm runs the run's confirm on a tracked goroutine, once per run per process.
func (s *Server) startKillConfirm(run types.AgentRun, start time.Time) {
	if _, running := killConfirms.LoadOrStore(run.ID, struct{}{}); running {
		return
	}
	window, retry := killConfirmWindow, killConfirmRetry
	s.goBackground(func() {
		defer killConfirms.Delete(run.ID)
		s.confirmKillStop(run, start, window, retry)
	})
}

// confirmKillStop retries the run's sandbox kill until it answers nil (success), a failure, or the window
// from start ends (failure). A daemon shutdown ends it with no row: the run still holds its ref and the
// terminal sweep finds it.
func (s *Server) confirmKillStop(run types.AgentRun, start time.Time, window, retry time.Duration) {
	ctx, cancel := context.WithDeadline(s.cfg.BaseCtx, start.Add(window))
	defer cancel()
	for {
		err := s.tryKillConfirm(ctx, run)
		if s.cfg.BaseCtx.Err() != nil {
			return
		}
		pending := errors.Is(err, runner.ErrTeardownConfirming) || errors.Is(err, errKillConfirmBusy)
		if err == nil || !pending || ctx.Err() != nil {
			s.recordKillConfirm(run, start, err)
			return
		}
		select {
		case <-ctx.Done():
			s.recordKillConfirm(run, start, err)
			return
		case <-time.After(retry):
		}
	}
}

// tryKillConfirm is one try, under the run's lock. nil means the sandbox is gone (or another path already
// cleared the ref); the ref is cleared here, as the kill tail clears it, only after a confirmed stop.
func (s *Server) tryKillConfirm(ctx context.Context, run types.AgentRun) error {
	lctx, unlock, ok := s.tryLockRunOp(ctx, run.ID)
	if !ok {
		return errKillConfirmBusy
	}
	defer unlock()
	ref := run.SandboxRef
	if cur, err := s.cfg.Store.GetRun(lctx, run.ID); err == nil {
		if cur.SandboxRef == "" {
			return nil // a second kill or the sweep confirmed it first
		}
		ref = cur.SandboxRef
	}
	if ref == "" {
		return nil
	}
	kerr := s.cfg.Runner.KillSandbox(lctx, ref)
	if kerr == nil {
		if c, ok := s.cfg.Store.(sandboxRefRetainingClearer); ok {
			_ = c.ClearSandboxRefRetaining(lctx, run.ID)
		}
	}
	return kerr
}

// recordKillConfirm writes the confirm's one row, on a context that outlives the window.
func (s *Server) recordKillConfirm(run types.AgentRun, start time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), killRevokeTimeout)
	defer cancel()
	data := map[string]any{"waited_ms": time.Since(start).Milliseconds()}
	outcome := "success"
	if err != nil {
		outcome = "failure"
		data["step"] = "kill_sandbox"
		data["reason"] = auditStep(ctx, auditStepKillSandbox, err, slog.String("run_id", run.ID.String()))
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.kill.confirm",
		run.ID.String(), outcome, mustJSON(data)))
}

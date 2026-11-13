// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The start fence. A command start (an exec, an attach, a relay, a desktop dial) and a write that ends the run it
// starts in, or marks it kept, take one per-run lock (db.RunStartLockClass). A start holds it from its last read of
// the run until the runner has started the command, at most execStartBudget; a writer holds it around its state
// write alone, never its tail. So a run ended before a start's last read is refused, and an end that comes after it
// lands once the command has started, which then loses its sandbox with the run. A start that cannot take the lock
// is refused. A writer is never refused by a start: one refused the lock (no lock connection, or none within
// runStartFenceWait) writes anyway, unfenced and logged, and one whose own context ends while it waits writes nothing
// that pass, so its end lands on the next.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runStartFenceWait is the longest a start or a writer waits for the fence: past a start's own budget, so a writer
// gives up only on a hold that outlived it.
var runStartFenceWait = 2 * execStartBudget

func runStartKey(id uuid.UUID) db.LockKey { return db.NewLockKey(db.RunStartLockClass, id.String()) }

// FenceRunStarts takes run id's start lock for one write that ends the run or marks it kept, and returns its
// release, called right after the write. A writer holding the run's operation lock on ctx nests on that hold's
// connection. A lock that cannot be taken is logged and the write goes on without it; the release is then a no-op.
func (s *Server) FenceRunStarts(ctx context.Context, id uuid.UUID) func() {
	_, release, err := s.locker().Lock(ctx, runStartKey(id), runStartFenceWait)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not fence the run's command starts; writing its end without the fence",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return func() {}
	}
	return release
}

// fenced runs write, one write that ends run id or marks it kept, under FenceRunStarts.
func (s *Server) fenced(ctx context.Context, id uuid.UUID, write func() (bool, error)) (bool, error) {
	defer s.FenceRunStarts(ctx, id)()
	return write()
}

// fenceRunsStarts is FenceRunStarts for one write that ends every run in runs (a runner's revoke): the keys are taken
// in ascending order on one hold, as db.LockOrder allows for this class, and one that cannot be taken is logged and
// left out.
func (s *Server) fenceRunsStarts(ctx context.Context, runs []types.AgentRun) func() {
	keys := make([]db.LockKey, 0, len(runs))
	for _, run := range runs {
		keys = append(keys, runStartKey(run.ID))
	}
	slices.SortFunc(keys, func(a, b db.LockKey) int { return cmp.Compare(a.Obj, b.Obj) })
	var releases []func()
	for _, k := range slices.Compact(keys) {
		hctx, release, err := s.locker().Lock(ctx, k, runStartFenceWait)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: could not fence a run's command starts; writing its end without the fence",
				slog.String("lock", k.String()), slog.Any("err", err))
			continue
		}
		ctx, releases = hctx, append(releases, release)
	}
	return func() {
		for _, release := range slices.Backward(releases) {
			release()
		}
	}
}

// The start fence's own refusals, each one the person may retry. A start that cannot take the fence is
// startUnfenced (lockUnavailableMsg's words).
var (
	errStartTooSlow  = errors.New("the runner took too long to start the command; try again")
	errStartLockLost = errors.New("the server lost its hold on the run while starting the command; try again")
)

// startUnfenced is a start refused because its fence could not be taken. It reads lockUnavailableMsg and is the
// lock's error underneath, so writeLockRefused answers it as any lock refusal.
type startUnfenced struct{ err error }

func (e startUnfenced) Error() string { return lockUnavailableMsg }
func (e startUnfenced) Unwrap() error { return e.err }

// thawRefusal is a command door's answer to its thaw (thawForExec): "" for none, the lock's words when the run's
// operation lock could not be taken (another operation holds the run), else the paused run's.
func thawRefusal(err error) string {
	switch {
	case err == nil:
		return ""
	case db.LockRefused(err):
		return lockUnavailableMsg
	}
	return "run is paused and could not be resumed; try again"
}

// startHold is one start's hold of its run's start lock: ctx carries it and ends with db.ErrLockLost as its cause
// if the lock is lost. release may be called more than once; a second call waits for the first.
type startHold struct {
	ctx     context.Context
	release func()
}

// startGates lets one start per run ask for the run's start lock at a time in this process; the rest wait here,
// holding no lock connection. A start takes a connection of the lock pool before it waits (db.LockPoolMaxConns), so
// without the gate one run's page load, eight relay dials at once, would hold the whole pool while its starts queued
// behind one another in the database, and every other run's lock would be refused meanwhile. It also keeps a write
// that ends the run behind at most one waiting start of this process.
type startGates struct {
	mu sync.Mutex
	m  map[uuid.UUID]*startGate
}

// startGate is one run's gate: turn holds the start whose turn it is, and users counts the starts at the gate, so
// the entry is dropped when the last one leaves.
type startGate struct {
	turn  chan struct{}
	users int
}

// enter waits for run id's turn until ctx ends. leave gives the turn up; it is called once.
func (g *startGates) enter(ctx context.Context, id uuid.UUID) (leave func(), err error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[uuid.UUID]*startGate{}
	}
	gate := g.m[id]
	if gate == nil {
		gate = &startGate{turn: make(chan struct{}, 1)}
		g.m[id] = gate
	}
	gate.users++
	g.mu.Unlock()
	drop := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if gate.users--; gate.users == 0 {
			delete(g.m, id)
		}
	}
	select {
	case gate.turn <- struct{}{}:
		return func() { <-gate.turn; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// lockStart takes run id's start lock for one start, in its turn among this process's starts on the run
// (startGates). The turn and the lock share one wait (runStartFenceWait). A lock that cannot be taken refuses the
// start (startUnfenced): a start never goes on unfenced.
func (s *Server) lockStart(ctx context.Context, id uuid.UUID) (startHold, error) {
	refuse := func(err error) (startHold, error) {
		slog.WarnContext(ctx, "wardynd: could not take a run's start lock; refusing the start",
			slog.String("run_id", id.String()), slog.Any("err", err))
		return startHold{}, startUnfenced{err}
	}
	until := time.Now().Add(runStartFenceWait)
	gctx, stop := context.WithDeadline(ctx, until)
	leave, err := s.locks.starts.enter(gctx, id)
	stop()
	if err != nil {
		return refuse(fmt.Errorf("%w: waiting for the run's earlier command starts: %w", db.ErrLockBusy, err))
	}
	hctx, release, err := s.locker().Lock(ctx, runStartKey(id), max(time.Until(until), time.Second))
	if err != nil {
		leave()
		return refuse(err)
	}
	return startHold{ctx: hctx, release: sync.OnceFunc(func() { release(); leave() })}, nil
}

// startAdmitted runs start, one command start under h, and lets the fence go beside the started command, never
// while it runs. start runs on a context of ctx's that is cut at execStartBudget, which also lets the fence go, or
// when h's lock is lost. A value start returns after either, or once a check on the lock's own connection
// (lockHeld) finds the lock gone, is ended (end) and refused (errStartTooSlow, errStartLockLost): a command the
// fence did not cover is never handed on. That context is never ended once start has returned in time, so the
// command outlives the fence.
func startAdmitted[T any](s *Server, ctx context.Context, h startHold, start func(context.Context) (T, error), end func(T)) (T, error) {
	var zero T
	if errors.Is(context.Cause(h.ctx), db.ErrLockLost) {
		h.release()
		return zero, errStartLockLost
	}
	sctx, cut := context.WithCancelCause(ctx)
	slow := time.AfterFunc(execStartBudget, func() { cut(errStartTooSlow); h.release() })
	unwatch := context.AfterFunc(h.ctx, func() {
		if errors.Is(context.Cause(h.ctx), db.ErrLockLost) {
			cut(errStartLockLost)
		}
	})
	v, err := start(sctx)
	late := !slow.Stop()
	unwatch()
	var refused error
	switch {
	case late:
		refused = errStartTooSlow
	case errors.Is(context.Cause(h.ctx), db.ErrLockLost), err == nil && s.lockHeld(h.ctx) != nil:
		refused = errStartLockLost
	}
	go h.release()
	switch {
	case refused != nil && err == nil:
		end(v)
		return zero, refused
	case refused != nil:
		return zero, refused
	}
	return v, err
}

// execAdmitted is startAdmitted for an ExecStream into ref.
func (s *Server) execAdmitted(ctx context.Context, h startHold, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	return startAdmitted(s, ctx, h, func(ctx context.Context) (*runner.ExecSession, error) { return s.execStream(ctx, ref, spec) }, closeExecSession)
}

// execStream is the runner's ExecStream, with no session read as a failure.
func (s *Server) execStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	sess, err := s.cfg.Runner.ExecStream(ctx, ref, spec)
	if err == nil && sess == nil {
		err = errors.New("the runner returned no exec session")
	}
	return sess, err
}

func closeExecSession(sess *runner.ExecSession) {
	if sess.Close != nil {
		_ = sess.Close()
	}
}

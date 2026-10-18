// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run on a runner is created and executed only under a lease the organisation sent it (design H3 slice 2
// §9). Dispatch persists the run's next execution generation (BeginRunnerExecution), sends the runner a
// LEASE that carries it, and only then asks for the sandbox; the runner refuses a create with no live lease.
// The Exec or Start that follows carries that same generation from its own context, and its correlated
// result is what binds the execution handle. A token renewal sends a fresh lease, so the runner's lapse
// containment follows the organisation's own stamp.

type runnerExecutionKey struct{}

// runnerExecution is the generation one dispatch or revive issued for a run on a runner. Its Exec or Start
// carries exactly this generation (bind), never one read back from the latest row.
type runnerExecution struct {
	runnerID, runID, generation uuid.UUID
	operation                   string
}

func (e runnerExecution) bind(ctx context.Context) context.Context {
	if e.generation == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, runnerExecutionKey{}, e)
}

// selftest is e's BYOI selftest: it rides the task's issued Exec generation without spending it, and
// carries an operation of its own, so it authorizes the selftest call only and never the task's Exec.
func (e runnerExecution) selftest() runnerExecution {
	if e.operation != runnerwire.MethodExec {
		return runnerExecution{}
	}
	e.operation = runnerwire.MethodSelftest
	return e
}

var (
	errRunnerExecutionUnbound = runnerwire.Refuse("this call carries no execution generation the organisation issued")
	// errRunnerContained: the runner contained the run while it was offline and refuses it a lease until a
	// revive's recovery (reviveRecoverContainment) has been applied.
	errRunnerContained   = errors.New("the run was contained on its runner while it was offline and cannot be leased again")
	errRunnerLeaseLapsed = errors.New("the run's token lapsed before its lease reached its runner")
	errRunnerLeaseEnded  = errors.New("the run's end passed before its lease reached its runner")
	errRunnerNotReady    = errors.New("the run's runner is not connected")
	// errRunnerLeaseBusy: the run's lock, or its owner's one LEASE-send slot, is held by a sender of a
	// lease of its own. A renewal that meets it answers 503 and the run's proxy retries it a minute later
	// (renewRetry), well inside the 65 minutes the runner's current lease still gives the run.
	errRunnerLeaseBusy = errors.New("the run's lease is being sent by another holder")
)

// leaseSendTimeout bounds one LEASE send under the run's lock: a runner that stops acknowledging cannot
// pin the lock, or the process's lock connections, past it. leaseWait is how long a sender that must not
// lose to a stale renewal (an end change, a READY re-lease, a revive) waits for the owner's slot and the
// run's lock: one send's worth.
const (
	leaseSendTimeout = 5 * time.Second
	leaseWait        = leaseSendTimeout + time.Second
)

// reviveRunnerTimeout bounds everything a revive of a run on a runner does under the run's lock, so a
// runner that stops answering cannot hold the lock or a lock connection past it.
var reviveRunnerTimeout = 3 * time.Minute

// withRunnerDeadline bounds ctx by deadline for a run on a runner; any other run is left as it is.
func withRunnerDeadline(ctx context.Context, run types.AgentRun, deadline time.Time) (context.Context, context.CancelFunc) {
	if run.RunnerID == nil {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

// lockRunOpWithin takes the run's lock, waiting at most wait (0: only if it is free now).
func (s *Server) lockRunOpWithin(ctx context.Context, id uuid.UUID, wait time.Duration) (context.Context, func(), bool) {
	if wait <= 0 {
		return s.tryLockRunOp(ctx, id)
	}
	lctx, unlock, err := s.locker().Lock(ctx, db.NewLockKey(db.RunOpLockClass, id.String()), wait)
	if err != nil {
		return ctx, nil, false
	}
	return lctx, unlock, true
}

// leaseSkew is how far inside the runner's own refusal the organisation stops sending a lease: the two
// clocks agree only to the session's handshake.
const leaseSkew = 5 * time.Second

// runnerExecutionStore is the Postgres store: runs on runners exist only there.
func (s *Server) runnerExecutionStore() (store.PG, error) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return store.PG{}, store.ErrRunnerExecutionRefused
	}
	return st, nil
}

// leaseRunnerExecution issues the run's next execution generation for operation (Exec at dispatch, Start
// at revive), after previous, and sends the runner the lease that carries it. operation "" issues none:
// a run that execs nothing is leased with no generation. Both happen before the call the lease authorizes,
// under the run's lock, so a renewal never sends an older generation after this one.
func (s *Server) leaseRunnerExecution(ctx context.Context, run types.AgentRun, previous uuid.UUID, operation string) (runnerExecution, error) {
	st, err := s.runnerExecutionStore()
	if err != nil || run.RunnerID == nil {
		return runnerExecution{}, cmp.Or(err, store.ErrRunnerExecutionRefused)
	}
	release, ok := s.runnerOwnerSlots(run.CreatedBy).acquireLease(ctx, leaseWait)
	if !ok {
		return runnerExecution{}, errRunnerLeaseBusy
	}
	defer release()
	ctx, unlock, err := s.lockRunOp(ctx, run.ID)
	if err != nil {
		return runnerExecution{}, err
	}
	defer unlock()
	renewed, err := s.leaseSendable(ctx, st, run)
	if err != nil {
		return runnerExecution{}, err
	}
	e := runnerExecution{runnerID: *run.RunnerID, runID: run.ID, operation: operation}
	if operation != "" {
		e.generation = uuid.New()
		if _, err := st.BeginRunnerExecution(ctx, e.runnerID, run.ID, previous, e.generation, operation); err != nil {
			return runnerExecution{}, err
		}
	}
	return e, s.sendRunnerLease(ctx, run, e.generation, renewed)
}

// leaseSendable is the run's token stamp when the runner would accept its lease now, else why not. The
// runner ends its whole session on a LEASE it refuses, dropping every other run on it, so the organisation
// never sends one: not for a run the runner holds contained, nor one whose token stamp or end is past, nor
// to a session that is not READY.
func (s *Server) leaseSendable(ctx context.Context, st store.PG, run types.AgentRun) (time.Time, error) {
	switch held, err := st.RunHeldContainedOnRunner(ctx, run.ID); {
	case err != nil:
		return time.Time{}, err
	case held:
		return time.Time{}, errRunnerContained
	}
	renewed, err := st.RunTokenRenewedAt(ctx, run.ID)
	if err != nil {
		return time.Time{}, err
	}
	now := s.cfg.Now()
	switch {
	case now.Sub(renewed) >= identity.RunTokenLapseAfter-leaseSkew:
		return time.Time{}, errRunnerLeaseLapsed
	case run.EndsAt != nil && !run.EndsAt.After(now.Add(leaseSkew)):
		return time.Time{}, errRunnerLeaseEnded
	case !s.runnerEndpoint(*run.RunnerID).link.Online():
		return time.Time{}, errRunnerNotReady
	}
	return renewed, nil
}

// sendRunnerLease sends the run's lease: its token stamp, the one the organisation's lapsed-token sweep
// reads, and its stored end (zero for none). A LEASE is processed before any later call on the session.
func (s *Server) sendRunnerLease(ctx context.Context, run types.AgentRun, generation uuid.UUID, renewed time.Time) error {
	l := runnerwire.Lease{RunID: run.ID, ExecutionGeneration: generation, TokenRenewedAt: renewed}
	if run.EndsAt != nil {
		l.EndsAt = *run.EndsAt
	}
	payload, err := runnerwire.Marshal(l)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, leaseSendTimeout)
	defer cancel()
	return s.runnerEndpoint(*run.RunnerID).link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: payload})
}

// renewRunnerLease re-sends a run's lease with the generation already issued: after its token was renewed
// or its end changed, on a new runner session, and at revive. It takes the runner owner's LEASE-send slot, then
// the run's lock, waiting at most wait for each (a token renewal waits for neither: errRunnerLeaseBusy), and
// re-reads the run and the generation under the lock, so it never lands after a revive's newer lease nor
// puts back an end that has changed.
func (s *Server) renewRunnerLease(ctx context.Context, run types.AgentRun, wait time.Duration) error {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return nil
	}
	st, err := s.runnerExecutionStore()
	if err != nil {
		return err
	}
	release, ok := s.runnerOwnerSlots(run.CreatedBy).acquireLease(ctx, wait)
	if !ok {
		return errRunnerLeaseBusy
	}
	defer release()
	return s.renewRunnerLeaseHeld(ctx, st, run, wait)
}

// renewalLeaseSlot takes, for a token renewal of a run on a runner, its owner's LEASE-send slot before
// anything is minted or stamped, so a renewal that meets another sender changes nothing. busy says the slot
// is taken; a run on no runner has none to take.
func (s *Server) renewalLeaseSlot(ctx context.Context, run types.AgentRun) (release func(), busy bool) {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return func() {}, false
	}
	release, ok := s.runnerOwnerSlots(run.CreatedBy).acquireLease(ctx, 0)
	return release, !ok
}

// sendRenewedLease is a token renewal's lease, sent with the owner's LEASE-send slot already taken
// (renewalLeaseSlot); it never waits for the run's lock.
func (s *Server) sendRenewedLease(ctx context.Context, run types.AgentRun) error {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return nil
	}
	st, err := s.runnerExecutionStore()
	if err != nil {
		return err
	}
	return s.renewRunnerLeaseHeld(ctx, st, run, 0)
}

// renewRunnerLeaseHeld is renewRunnerLease for a caller that already holds the owner's LEASE-send slot.
func (s *Server) renewRunnerLeaseHeld(ctx context.Context, st store.PG, run types.AgentRun, wait time.Duration) error {
	ctx, unlock, ok := s.lockRunOpWithin(ctx, run.ID, wait)
	if !ok {
		return errRunnerLeaseBusy
	}
	defer unlock()
	run, err := st.GetRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return nil
	}
	renewed, err := s.leaseSendable(ctx, st, run)
	if err != nil {
		return err
	}
	var generation uuid.UUID
	switch cur, err := st.RunnerExecutionOf(ctx, run.ID); {
	case err == nil:
		generation = cur.Generation
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	return s.sendRunnerLease(ctx, run, generation, renewed)
}

// releaseRunnerLeases re-leases every live run of a runner whose session just became READY. A runner keeps
// its leases' deadlines in memory only, and a run's renewals come from its own proxy, which a lapse stops:
// without this, a runner whose session was replaced while a renewal could not reach it contains the run.
func (s *Server) releaseRunnerLeases(ctx context.Context, runnerID uuid.UUID) {
	st, err := s.runnerExecutionStore()
	if err != nil {
		return
	}
	runs, err := st.ListLiveRunnerRuns(ctx, runnerID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing a runner's live runs to lease them failed", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
		return
	}
	for _, run := range runs {
		if err := s.renewRunnerLease(ctx, run, leaseWait); err != nil {
			slog.WarnContext(ctx, "wardynd: re-leasing a run on its runner's new session failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
}

// renewLeaseAfterEndChange re-sends a run's lease once its end changed, with the end it now has: the
// runner contains a run at the end its lease carries. It runs to completion whatever becomes of the request
// that changed the end, whose write is already committed. A failure is logged, not answered: the end is
// committed and enforced by the organisation itself, a runner that is asleep cannot take a lease, and the
// next renewal (at most 30 minutes away) or the runner's next READY carries the new end.
func (s *Server) renewLeaseAfterEndChange(ctx context.Context, runID uuid.UUID) {
	ctx = context.WithoutCancel(ctx)
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err == nil {
		err = s.renewRunnerLease(ctx, run, leaseWait)
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: re-leasing a run whose end changed failed", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// bindRunnerExecution binds the handle of the Exec or Start that e authorized, from that call's own
// result; an exec-less runtime's empty handle is the main process.
func (s *Server) bindRunnerExecution(ctx context.Context, e runnerExecution, execID string) error {
	if e.generation == uuid.Nil {
		return nil
	}
	st, err := s.runnerExecutionStore()
	if err != nil {
		return err
	}
	return st.BindRunnerExecution(ctx, e.runnerID, e.runID, e.generation, cmp.Or(execID, runner.MainProcessExecID))
}

// dispatchedGeneration is runner runnerID's remote.Options.ExecutionGeneration: the generation the calling
// dispatch or revive bound to its context, still the run's persisted, unattempted intent for this method
// on this runner and this sandbox. Anything else refuses before the call reaches the runner. A BYOI
// selftest checks the task's unattempted Exec intent and binds nothing: it is not that execution.
func (s *Server) dispatchedGeneration(ctx context.Context, runnerID uuid.UUID, method, ref string) (uuid.UUID, error) {
	e, ok := ctx.Value(runnerExecutionKey{}).(runnerExecution)
	if !ok || e.runnerID != runnerID || e.operation != method {
		return uuid.Nil, errRunnerExecutionUnbound
	}
	if method == runnerwire.MethodSelftest {
		method = runnerwire.MethodExec
	}
	st, err := s.runnerExecutionStore()
	if err != nil {
		return uuid.Nil, err
	}
	cur, err := st.RunnerExecutionOf(ctx, e.runID)
	if err != nil {
		return uuid.Nil, err
	}
	run, err := st.GetRun(ctx, e.runID)
	if err != nil {
		return uuid.Nil, err
	}
	if cur.RunnerID != runnerID || cur.Generation != e.generation || cur.Operation != method || cur.ExecID != "" || run.SandboxRef != ref {
		return uuid.Nil, errRunnerExecutionUnbound
	}
	return e.generation, nil
}

// leaseRunOnRunner is dispatch's lease for a run bound to a runner, before its sandbox is asked for: a
// task run is issued its first generation, for the Exec that starts its agent. A run on the organisation's
// own executors is not leased.
func (s *Server) leaseRunOnRunner(ctx context.Context, run types.AgentRun) (runnerExecution, error) {
	if run.RunnerID == nil {
		return runnerExecution{}, nil
	}
	operation := ""
	if run.Task != "" {
		operation = runnerwire.MethodExec
	}
	e, err := s.leaseRunnerExecution(ctx, run, uuid.Nil, operation)
	if err != nil {
		return runnerExecution{}, fmt.Errorf("lease the run to its runner: %w", err)
	}
	return e, nil
}

// startRunOnRunner is revive's Start of a stopped agent on a runner: a fresh generation after the run's
// current one, sent in a lease first, carried by the Start, and bound from its result.
func (s *Server) startRunOnRunner(ctx context.Context, starter runner.SandboxStarter, run types.AgentRun) error {
	st, err := s.runnerExecutionStore()
	if err != nil {
		return err
	}
	var previous uuid.UUID
	switch cur, err := st.RunnerExecutionOf(ctx, run.ID); {
	case err == nil:
		previous = cur.Generation
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	e, err := s.leaseRunnerExecution(ctx, run, previous, runnerwire.MethodStart)
	if err != nil {
		return err
	}
	if err := starter.StartSandbox(e.bind(ctx), run.SandboxRef); err != nil {
		return err
	}
	return s.bindRunnerExecution(ctx, e, "")
}

// replaceRevivedProxy is revive's proxy replacement. A run on a runner is re-leased first, on the claim's
// fresh token stamp: its runner refuses to replace the proxy of a run whose lease has lapsed.
func (s *Server) replaceRevivedProxy(ctx context.Context, rv runner.ProxyReviver, run types.AgentRun, cfgJSON []byte) error {
	if run.RunnerID == nil {
		return rv.ReplaceProxy(ctx, run.SandboxRef, cfgJSON)
	}
	if err := s.renewRunnerLease(ctx, run, leaseWait); err != nil {
		return err
	}
	return rv.ReplaceProxy(ctx, run.SandboxRef, cfgJSON)
}

// startRevivedAgent starts a revived run's stopped agent: on a runner under a fresh generation.
func (s *Server) startRevivedAgent(ctx context.Context, starter runner.SandboxStarter, run types.AgentRun) error {
	if run.RunnerID != nil {
		return s.startRunOnRunner(ctx, starter, run)
	}
	return starter.StartSandbox(ctx, run.SandboxRef)
}

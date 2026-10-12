// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

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

var (
	errRunnerExecutionUnbound = runnerwire.Refuse("this call carries no execution generation the organisation issued")
	// errRunnerContained: the runner contained the run while it was offline and refuses it a lease until a
	// recovery the server does not send yet.
	errRunnerContained = errors.New("the run was contained on its runner while it was offline and cannot be leased again")
)

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
// a run that execs nothing is leased with no generation. Both happen before the call the lease authorizes.
func (s *Server) leaseRunnerExecution(ctx context.Context, run types.AgentRun, previous uuid.UUID, operation string) (runnerExecution, error) {
	st, err := s.runnerExecutionStore()
	if err != nil || run.RunnerID == nil {
		return runnerExecution{}, cmp.Or(err, store.ErrRunnerExecutionRefused)
	}
	e := runnerExecution{runnerID: *run.RunnerID, runID: run.ID, operation: operation}
	if operation != "" {
		e.generation = uuid.New()
		if _, err := st.BeginRunnerExecution(ctx, e.runnerID, run.ID, previous, e.generation, operation); err != nil {
			return runnerExecution{}, err
		}
	}
	return e, s.sendRunnerLease(ctx, st, run, e.generation)
}

// sendRunnerLease sends the run's lease: its token stamp, the one the organisation's lapsed-token sweep
// reads, and its stored end (zero for none). A LEASE is processed before any later call on the session.
func (s *Server) sendRunnerLease(ctx context.Context, st store.PG, run types.AgentRun, generation uuid.UUID) error {
	switch held, err := st.RunHeldContainedOnRunner(ctx, run.ID); {
	case err != nil:
		return err
	case held:
		return errRunnerContained
	}
	renewed, err := st.RunTokenRenewedAt(ctx, run.ID)
	if err != nil {
		return err
	}
	l := runnerwire.Lease{RunID: run.ID, ExecutionGeneration: generation, TokenRenewedAt: renewed}
	if run.EndsAt != nil {
		l.EndsAt = *run.EndsAt
	}
	payload, err := runnerwire.Marshal(l)
	if err != nil {
		return err
	}
	return s.runnerEndpoint(*run.RunnerID).link.Send(ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: payload})
}

// renewRunnerLease re-sends a run's lease after its token was renewed, with the generation already issued,
// so the runner keeps the run's egress past the lapse window it was first leased for.
func (s *Server) renewRunnerLease(ctx context.Context, run types.AgentRun) error {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return nil
	}
	st, err := s.runnerExecutionStore()
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
	return s.sendRunnerLease(ctx, st, run, generation)
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
// on this runner and this sandbox. Anything else refuses before the call reaches the runner.
func (s *Server) dispatchedGeneration(ctx context.Context, runnerID uuid.UUID, method, ref string) (uuid.UUID, error) {
	e, ok := ctx.Value(runnerExecutionKey{}).(runnerExecution)
	if !ok || e.runnerID != runnerID || e.operation != method {
		return uuid.Nil, errRunnerExecutionUnbound
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
	if err := s.renewRunnerLease(ctx, run); err != nil {
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

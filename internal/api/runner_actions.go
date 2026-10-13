// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// maxActionResultBytes bounds one action_result event body.
const maxActionResultBytes = 1024

var errRunnerEventUnsupported = errors.New("runner event kind has no org ingestion")

// substrateRegistry is the orchestrator's runtime registration seam.
type substrateRegistry interface {
	Add(substrate.Substrate)
	Remove(substrate.Substrate)
}

type runnerEndpoint struct {
	sub  *remote.Substrate
	link *remote.Link
}

// ownerSlots are one runner owner's LEASE-send slot and revive slot (capacity 1 each), both taken before a
// run's lock. They are the person's, not a runner's: runners that stop answering hold at most one lock
// connection for their owner's lease senders and one for their owner's revives, whatever number of runners
// that person claims and of their runs are re-leased or revived.
type ownerSlots struct {
	leasing, reviving chan struct{}
}

// runnerOwnerSlots is owner's slots. The owner of a run on a runner is the run's creator: placement
// (poolOwner, ownRunnerTargets) binds a run only to its creator's own claimed runners, which is what holds a
// task-less dispatch, a renewal and a proxy-only revive to the owner; BeginRunnerExecution re-checks it
// only for a run that execs or starts.
func (s *Server) runnerOwnerSlots(owner string) ownerSlots {
	s.runnerStreams.subsMu.Lock()
	defer s.runnerStreams.subsMu.Unlock()
	if o, ok := s.runnerStreams.owners[owner]; ok {
		return o
	}
	if s.runnerStreams.owners == nil {
		s.runnerStreams.owners = map[string]ownerSlots{}
	}
	o := ownerSlots{leasing: make(chan struct{}, 1), reviving: make(chan struct{}, 1)}
	s.runnerStreams.owners[owner] = o
	return o
}

// acquireLease takes the owner's LEASE-send slot, waiting at most wait (0: only if it is free now).
func (o ownerSlots) acquireLease(ctx context.Context, wait time.Duration) (release func(), ok bool) {
	return acquireSlot(ctx, o.leasing, wait)
}

// acquireRevive takes the owner's revive slot, waiting at most wait.
func (o ownerSlots) acquireRevive(ctx context.Context, wait time.Duration) (release func(), ok bool) {
	return acquireSlot(ctx, o.reviving, wait)
}

// acquireSlot takes a capacity-1 slot, waiting at most wait (0: only if it is free now).
func acquireSlot(ctx context.Context, slot chan struct{}, wait time.Duration) (release func(), ok bool) {
	release = func() { <-slot }
	select {
	case slot <- struct{}{}:
		return release, true
	default:
	}
	if wait <= 0 {
		return nil, false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case slot <- struct{}{}:
		return release, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

// runnerEndpoint is the one remote substrate (and Link) of a runner for this
// process, registered with the orchestrator so its refs route to it. Offline,
// its teardown verbs queue through the exact-binding pending-action store.
func (s *Server) runnerEndpoint(id uuid.UUID) runnerEndpoint {
	s.runnerStreams.subsMu.Lock()
	defer s.runnerStreams.subsMu.Unlock()
	if ep, ok := s.runnerStreams.subs[id]; ok {
		return ep
	}
	ep := runnerEndpoint{link: &remote.Link{}}
	ep.sub = remote.New(id.String(), ep.link, remote.Options{Queue: func(ctx context.Context, kind types.RunnerActionKind, ref string) error {
		return s.queueRunnerTeardown(ctx, id, kind, ref)
	}, Dispatch: s.runnersEnabled, Relay: s.acceptRelay,
		ExecutionGeneration: func(ctx context.Context, method, ref string) (uuid.UUID, error) {
			return s.dispatchedGeneration(ctx, id, method, ref)
		}})
	if s.runnerStreams.subs == nil {
		s.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
	}
	s.runnerStreams.subs[id] = ep
	if reg, ok := s.cfg.Runner.(substrateRegistry); ok {
		reg.Add(ep.sub)
	}
	return ep
}

func (s *Server) queueRunnerTeardown(ctx context.Context, runnerID uuid.UUID, kind types.RunnerActionKind, ref string) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return store.ErrRunnerActionRefused
	}
	_, _, err := st.QueueRunnerTeardown(ctx, runnerID, kind, ref)
	return err
}

// RegisterRunnerSubstrates registers every claimed runner at boot, before the
// boot reconcile, so a local run's ref routes to its runner (and an offline
// teardown queues) before that runner reconnects.
func (s *Server) RegisterRunnerSubstrates(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return nil
	}
	runners, err := st.ListRunners(ctx)
	if err != nil {
		return err
	}
	for _, rn := range runners {
		if rn.State == types.RunnerClaimed {
			s.runnerEndpoint(rn.ID)
		}
	}
	return nil
}

// admitRunner is the production HubConfig.Admit: the durable inventory, queue,
// result and activity callbacks bound to the authenticated stored runner.
func (s *Server) admitRunner(_ context.Context, rn types.Runner) (remote.Admission, error) {
	if _, ok := s.cfg.Store.(store.PG); !ok || s.cfg.RunnerAuditPrepare == nil {
		return remote.Admission{}, errors.New("runner admission requires the Postgres store and the runner audit policy")
	}
	id := rn.ID
	ep := s.runnerEndpoint(id)
	return remote.Admission{Substrate: ep.sub, Link: ep.link,
		Pending:        func(ctx context.Context) ([]runnerwire.PendingAction, error) { return s.PendingFromRunner(ctx, id) },
		BeginInventory: func(ctx context.Context, session string) error { return s.BeginInventoryFromRunner(ctx, id, session) },
		StatePage: func(ctx context.Context, session string, page runnerwire.StatePage) error {
			return s.StatePageFromRunner(ctx, id, session, page)
		},
		Event:    func(ctx context.Context, ev runnerwire.Event) error { return s.EventFromRunner(ctx, id, ev) },
		Activity: func(ctx context.Context, runID uuid.UUID) error { return s.ActivityFromRunner(ctx, id, runID) },
		Ready:    func() { go s.releaseRunnerLeases(s.cfg.BaseCtx, id) },
	}, nil
}

// BeginInventoryFromRunner is Admission.BeginInventory, off the read loop:
// when the previous session completed its inventory, an asserted subject's
// deferred action continues in one successor, then the new inventory begins.
// The successor is in this session's PENDING.
func (s *Server) BeginInventoryFromRunner(ctx context.Context, runnerID uuid.UUID, sessionID string) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return store.ErrRunnerExecutionRefused
	}
	if _, err := st.ReissueDeferredAssertedRunnerActions(ctx, runnerID, sessionID); err != nil {
		return err
	}
	return st.BeginRunnerInventory(ctx, runnerID, sessionID)
}

// EventFromRunner is Admission.Event: the checked dispatcher of one runner's
// events. caps and waiting are the substrate's own; a kind with no org
// ingestion refuses rather than being acknowledged unrecorded.
func (s *Server) EventFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) error {
	switch ev.Kind {
	case runnerwire.EventActionResult:
		return s.ActionResultFromRunner(ctx, runnerID, ev)
	case runnerwire.EventAgentExit:
		return s.ExitFromRunner(ctx, runnerID, ev)
	case runnerwire.EventSpool:
		return s.DecisionIngestFromRunner(ctx, runnerID, ev)
	case runnerwire.EventPosture:
		return s.PostureFromRunner(ctx, runnerID, ev)
	case runnerwire.EventResidentErase:
		return s.ResidentEraseFromRunner(ctx, runnerID, ev)
	case runnerwire.EventCaps:
		s.capsFromRunner(ctx, runnerID, ev)
		return nil
	case runnerwire.EventWaiting:
		return nil
	default:
		return fmt.Errorf("%w: %s", errRunnerEventUnsupported, ev.Kind)
	}
}

// PendingFromRunner is the PENDING source of a fresh session: one bounded page
// of the runner's oldest open actions. StatePageFromRunner refuses READY while
// an open action remains, so the next fresh session carries the next page.
func (s *Server) PendingFromRunner(ctx context.Context, runnerID uuid.UUID) ([]runnerwire.PendingAction, error) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return nil, store.ErrRunnerActionRefused
	}
	page, _, err := st.ListPendingRunnerActions(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	out := make([]runnerwire.PendingAction, 0, len(page))
	for _, a := range page {
		out = append(out, runnerwire.PendingAction{Retirement: a.Retirement, Quarantine: a.Quarantine, Recovery: a.Recovery,
			ID: a.ID, Kind: a.Kind, RunID: a.RunID, Ref: a.Ref})
	}
	return out, nil
}

// ActionResultFromRunner is the checked action_result adapter. runnerID is
// the stream's authenticated stored identity; the store binds the event's run
// and the action to it in the committing transaction.
func (s *Server) ActionResultFromRunner(ctx context.Context, runnerID uuid.UUID, event runnerwire.Event) error {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok || event.Kind != runnerwire.EventActionResult || event.RunID == nil || len(event.Data) > maxActionResultBytes || s.cfg.RunnerAuditPrepare == nil {
		return store.ErrRunnerActionRefused
	}
	var result runnerwire.ActionResult
	dec := json.NewDecoder(bytes.NewReader(event.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return store.ErrRunnerActionRefused
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return store.ErrRunnerActionRefused
	}
	_, _, err := st.RecordRunnerActionResult(ctx, runnerID, *event.RunID, result.ID, result.Outcome, result.ObservedAt, s.cfg.RunnerAuditPrepare)
	return err
}

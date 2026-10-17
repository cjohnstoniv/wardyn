// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func (m *Manager) stateRows(ctx context.Context, ids []uuid.UUID) ([]runnerwire.RunState, error) {
	out := make([]runnerwire.RunState, 0, len(ids))
	for _, id := range ids {
		row, err := m.observeRun(ctx, id)
		if err != nil {
			return nil, err
		}
		if row != nil {
			out = append(out, *row)
		}
	}
	return out, nil
}

// observeExits is the periodic lifecycle pass. It observes only runs whose
// current execution can still yield an unrecorded exit, so its cost follows
// live executions, not every retained run. One failing run does not stop the
// pass for the others.
func (m *Manager) observeExits(ctx context.Context) error {
	m.mu.Lock()
	if err := m.ensureDurable(); err != nil {
		m.mu.Unlock()
		return err
	}
	var ids []uuid.UUID
	for _, id := range m.inventoryIDs() {
		if r := m.state.Runs[id]; r.Retirement == nil && r.Ref != "" && r.ExecID != "" && r.Generation != uuid.Nil && r.Exit == nil {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if _, err := m.observeRun(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// The observer owns its drain slot through exit persistence, not just substrate
// IO. Retirement cannot seal history while a captured exit is still unpublished.
func (m *Manager) observeRun(ctx context.Context, id uuid.UUID) (*runnerwire.RunState, error) {
	m.mu.Lock()
	if err := m.ensureDurable(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	r := m.state.Runs[id]
	if r.Retired || r.Quarantine != nil {
		m.mu.Unlock()
		return nil, nil
	}
	key := mutationKey{"local_observer", r.Ref, r.Generation}
	m.beginMutation(key.method, key.ref, key.generation)
	row := &runnerwire.RunState{RunID: id, Generation: r.Generation, Retirement: r.Retirement, Containment: r.Containment, ContainmentOnly: r.ContainmentOnly, MutationFailure: r.MutationFailure, ProxyStoppedAt: r.ProxyStoppedAt, Execution: durableExecutionResult(r), ContainmentPending: r.ContainmentPending}
	if r.Ref != "" {
		row.Ref = placement.RunnerRefPrefix(m.runnerID.String()) + r.Ref
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.finishMutation(key); m.mu.Unlock() }()
	// Retirement intent is written only after settled Kill and a complete drain.
	// No new substrate observer may create evidence after that exact settlement.
	if r.Retirement == nil && r.Ref != "" {
		st, err := m.sub.Status(ctx, r.Ref)
		if err != nil {
			return nil, err
		}
		row.SandboxPresent = st.State != types.RunStopped || st.ExitCode != nil
		if err := m.observeAgent(ctx, id, r, row); err != nil {
			return nil, err
		}
	}
	// A quarantine that arrived meanwhile owns the evidence: do not reopen (and
	// so recreate) a spool erasure may be about to delete.
	m.mu.Lock()
	current := m.state.Runs[id]
	m.mu.Unlock()
	if current.Quarantine != nil || current.EvidenceErased {
		return nil, nil
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		return nil, err
	}
	depth, _, drops, err := spool.Stats(ctx)
	receipt, receiptErr := spool.Receipt(ctx)
	if err := errors.Join(err, receiptErr, spool.Close()); err != nil {
		return nil, err
	}
	row.SpoolReceipt = &receipt
	row.SpoolDepth = depth
	row.SpoolDrops = int(min(drops, uint64(^uint(0)>>1)))
	return row, nil
}

func (m *Manager) observeAgent(ctx context.Context, id uuid.UUID, r localRun, row *runnerwire.RunState) error {
	if r.ExecID == "" || r.Generation == uuid.Nil {
		return nil
	}
	agent, err := m.sub.AgentStatus(ctx, r.Ref, r.ExecID)
	if err != nil {
		return err
	}
	row.AgentState, row.ExitCode = string(agent.State), agent.ExitCode
	if agent.ExitCode == nil || r.Exit != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.state.Runs[id]
	if current.Generation != r.Generation || current.ExecID != r.ExecID || current.Ref != r.Ref || current.Exit != nil || current.Quarantine != nil || current.EvidenceErased {
		return nil
	}
	current.Exit = &runnerwire.AgentExit{Generation: r.Generation, ExecID: r.ExecID, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + r.Ref, Code: *agent.ExitCode, ObservedAt: m.now().UTC()}
	m.state.Runs[id] = current
	return m.save()
}

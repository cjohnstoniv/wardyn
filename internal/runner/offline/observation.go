// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func (m *Manager) stateRows(ctx context.Context, ids []uuid.UUID) ([]runnerwire.RunState, error) {
	out := make([]runnerwire.RunState, 0, len(ids))
	for _, id := range ids {
		m.mu.Lock()
		r := m.state.Runs[id]
		m.mu.Unlock()
		if r.MovedAway != "" && r.MovedAway == r.Ref {
			// The run was moved to another placement: the organisation holds it there, and this sandbox is
			// only the move's cleanup to do.
			continue
		}
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
	row := &runnerwire.RunState{RunID: id, Generation: r.Generation, Retirement: r.Retirement, Containment: r.Containment, ContainmentOnly: r.ContainmentOnly, MutationFailure: r.MutationFailure, ProxyStoppedAt: r.ProxyStoppedAt, Execution: durableExecutionResult(r), ContainmentPending: r.ContainmentPending, TeardownHold: r.teardownReport()}
	if r.Ref != "" {
		row.Ref = placement.RunnerRefPrefix(m.runnerID.String()) + r.Ref
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.finishMutation(key); m.mu.Unlock() }()
	// Retirement intent is written only after settled Kill and a complete drain.
	// No new substrate observer may create evidence after that exact settlement.
	if r.Retirement == nil && r.Ref != "" {
		if !m.coversKind(r.MachineKind) {
			// Unknown, never absent: the substrate cannot enumerate this kind now (macOS VMs off), so
			// the run is reported present, unprobed, and the org neither stamps nor ends it (#3667).
			row.SandboxPresent = true
		} else {
			st, err := m.sub.Status(ctx, r.Ref)
			if err != nil {
				return nil, err
			}
			// A recorded exit the org has not acknowledged is the run's end, still to be sent (Replay
			// follows STATE): the run reads present until then, so its real code lands, not a gone end.
			row.SandboxPresent = st.State != types.RunStopped || st.ExitCode != nil || (r.Exit != nil && !r.ExitAcked)
			if err := m.observeAgent(ctx, id, r, row); err != nil {
				return nil, err
			}
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

// coversKind reports whether the substrate can enumerate kind's sandboxes now (runner.KindCoverer).
// One that does not say covers containers and Linux VMs only.
func (m *Manager) coversKind(kind types.MachineKind) bool {
	if c, ok := m.sub.(runner.KindCoverer); ok {
		return c.CoversKind(kind)
	}
	switch kind.Normalize() {
	case "", types.MachineLinuxVM:
		return true
	}
	return false
}

// MoveUnbound records the organisation's notice that runID was moved away from localRef: that sandbox is
// left out of the run state this runner reports. A notice for a run this runner does not hold, or for
// another sandbox of it, changes nothing. The organisation never relies on the notice: it ignores a moved
// run's rows from the runners its move names.
func (m *Manager) MoveUnbound(_ context.Context, runID uuid.UUID, localRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureDurable(); err != nil {
		return err
	}
	r, ok := m.state.Runs[runID]
	if !ok || r.Ref == "" || r.Ref != localRef || r.MovedAway == localRef {
		return nil
	}
	r.MovedAway = localRef
	m.state.Runs[runID] = r
	return m.save()
}

// SpoolDrained reports whether localRef's run has no spooled decision the organisation has not
// acknowledged, the condition a move needs before it closes the run's spool cursor.
func (m *Manager) SpoolDrained(ctx context.Context, localRef string) (bool, error) {
	m.mu.Lock()
	id, _, ok := m.findRef(localRef)
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("runner: no run holds sandbox %q", localRef)
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		return false, err
	}
	depth, _, _, err := spool.Stats(ctx)
	if err := errors.Join(err, spool.Close()); err != nil {
		return false, err
	}
	return depth == 0, nil
}

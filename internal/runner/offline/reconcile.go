// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// Pending applies the org's durable queue in supplied order, before STATE and
// replay. A failed ACK retains the local result, so retry cannot repeat a
// destructive action against a later incarnation of the sandbox.
func (m *Manager) Pending(ctx context.Context, peer *runnerwire.Peer, actions []runnerwire.PendingAction) error {
	for _, a := range actions {
		result, err := m.apply(ctx, a)
		if err != nil {
			return err
		}
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventActionResult, RunID: &a.RunID, Data: b}); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) apply(ctx context.Context, a runnerwire.PendingAction) (runnerwire.ActionResult, error) {
	m.actionMu.Lock()
	defer m.actionMu.Unlock()
	proof := m.lostProof(ctx, a)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Revoked {
		return runnerwire.ActionResult{}, runnerwire.ErrRevoked
	}
	if err := m.ensureDurable(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	if old, ok := m.state.Actions[a.ID]; ok {
		if !samePendingAction(old.Action, a) {
			return runnerwire.ActionResult{}, errors.New("pending action identity changed")
		}
		if old.Result.Outcome != "" {
			return old.Result, m.save()
		}
	}
	r, err := m.pendingRun(a, proof)
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	local := r.Ref
	if a.Kind == types.RunnerActionRetire {
		return m.retireRun(ctx, a, r)
	}
	if a.Retirement != nil || r.Retired {
		return runnerwire.ActionResult{}, errors.New("unexpected retirement proof or retired identity")
	}
	if a.Kind == types.RunnerActionQuarantine {
		return m.quarantineRun(ctx, a, r)
	}
	if a.Quarantine != nil {
		return runnerwire.ActionResult{}, errors.New("unexpected quarantine proof")
	}
	if r.ContainmentOnly && !missingIdentityAction(a) {
		return runnerwire.ActionResult{}, errors.New("containment-only identity refuses this action")
	}
	if a.Kind == types.RunnerActionRecoverContainment {
		return m.recoverContainment(a, r)
	}
	if a.Kind == types.RunnerActionRecoverMutation {
		return m.recoverMutation(a, r)
	}
	if a.Recovery != nil {
		return runnerwire.ActionResult{}, errors.New("unexpected mutation recovery proof")
	}
	if err := m.beginContainment(a, r); err != nil {
		return runnerwire.ActionResult{}, err
	}
	m.mu.Unlock()
	err = nil
	switch a.Kind {
	case types.RunnerActionKill:
		err = m.sub.KillSandbox(ctx, local)
	case types.RunnerActionStopProxy:
		err = m.stopper.StopProxy(ctx, local)
	case types.RunnerActionEnd:
		end, ok := m.sub.(runner.SandboxEnder)
		if !ok {
			m.mu.Lock()
			return runnerwire.ActionResult{}, runner.ErrEndUnsupported
		}
		err = end.EndSandbox(ctx, local)
	default:
		m.mu.Lock()
		return runnerwire.ActionResult{}, errors.New("unknown pending action")
	}
	m.mu.Lock()
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	r = m.state.Runs[a.RunID]
	at := m.now().UTC()
	r.ProxyStoppedAt = &at
	outcome := "deferred"
	if m.mutationsDrained(a.RunID) && (r.Containment.Contained || r.Containment.Operation == string(a.Kind)) {
		outcome = "success"
		marker := *r.Containment
		marker.Contained = true
		r.Containment = &marker
	}
	m.state.Runs[a.RunID] = r
	result := runnerwire.ActionResult{ID: a.ID, Outcome: outcome, ObservedAt: at}
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	return result, nil
}

// AgentExited records exit before reporting it. Only org reconciliation may
// decide whether a kept headless run becomes terminal.
func (m *Manager) AgentExited(ctx context.Context, id uuid.UUID, localRef string, code int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r, ok := m.state.Runs[id]
	if !ok || r.Ref != localRef || r.Generation == uuid.Nil || r.ExecID == "" {
		return errors.New("agent exit has no matching local run")
	}
	if r.Retirement != nil {
		return errors.New("agent exit observer started after retirement settlement")
	}
	if r.Exit != nil {
		if r.Exit.Code != code {
			return errors.New("agent exit code changed")
		}
		return m.ensureDurable()
	}
	r.Exit = &runnerwire.AgentExit{Generation: r.Generation, ExecID: r.ExecID, Ref: placement.RunnerRefPrefix(m.runnerID.String()) + localRef, Code: code, ObservedAt: m.now().UTC()}
	m.state.Runs[id] = r
	return m.save()
}

func (m *Manager) runIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m.state.Runs))
	for id := range m.state.Runs {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// Replay uses only the authenticated runner peer; expired run tokens are never
// involved. Spool ACKs and durable exit results survive reconnect and restart.
func (m *Manager) Replay(ctx context.Context, peer *runnerwire.Peer) error {
	m.replayMu.Lock()
	defer m.replayMu.Unlock()
	m.mu.Lock()
	ids := m.runIDs()
	revoked := m.state.Revoked
	m.mu.Unlock()
	if revoked {
		return runnerwire.ErrRevoked
	}
	for _, id := range ids {
		m.mu.Lock()
		quarantined := m.state.Runs[id].Quarantine != nil || m.state.Runs[id].Retired
		m.mu.Unlock()
		if quarantined {
			continue
		}
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			return err
		}
		err = spool.Replay(ctx, func(ctx context.Context, rows []runnerspool.Record) error {
			evs := make([]runnerwire.Event, len(rows))
			for i, row := range rows {
				b, err := row.Marshal()
				if err != nil {
					return err
				}
				evs[i] = runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &id, Data: b}
			}
			return peer.EventsAck(ctx, evs)
		})
		closeErr := spool.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
		m.mu.Lock()
		if err := m.ensureDurable(); err != nil {
			m.mu.Unlock()
			return err
		}
		r := m.state.Runs[id]
		m.mu.Unlock()
		if r.Exit == nil || r.ExitAcked {
			continue
		}
		b, err := json.Marshal(r.Exit)
		if err != nil {
			return err
		}
		if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &id, Data: b}); err != nil {
			return err
		}
		m.mu.Lock()
		current := m.state.Runs[id]
		if current.Exit != nil && *current.Exit == *r.Exit {
			current.ExitAcked = true
			m.state.Runs[id] = current
			err = m.save()
		}
		m.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

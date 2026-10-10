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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Revoked {
		return runnerwire.ActionResult{}, runnerwire.ErrRevoked
	}
	if old, ok := m.state.Actions[a.ID]; ok {
		if old.Action != a {
			return runnerwire.ActionResult{}, errors.New("pending action identity changed")
		}
		return old.Result, m.save()
	}
	if a.ID == uuid.Nil {
		return runnerwire.ActionResult{}, errors.New("pending action has no identity")
	}
	r, ok := m.state.Runs[a.RunID]
	local, belongs := strings.CutPrefix(a.Ref, placement.RunnerRefPrefix(m.runnerID.String()))
	if !ok || !belongs || local == "" || r.Ref != local {
		return runnerwire.ActionResult{}, errors.New("pending action does not match a local run")
	}
	m.mu.Unlock()
	var err error
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
	m.state.Runs[a.RunID] = r
	result := runnerwire.ActionResult{ID: a.ID, Outcome: "success", ObservedAt: at}
	m.state.Actions[a.ID] = appliedAction{Action: a, Result: result}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	return result, nil
}

// RecordExec keeps the substrate's restart-safe agent liveness handle.
func (m *Manager) RecordExec(ctx context.Context, localRef, execID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if execID == "" {
		return errors.New("empty runner agent exec id")
	}
	for id, r := range m.state.Runs {
		if r.Ref == localRef {
			r.ExecID = execID
			r.Exit = nil
			r.ExitAcked = false
			m.state.Runs[id] = r
			return m.save()
		}
	}
	return errors.New("agent exec has no durable run binding")
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
	if !ok || r.Ref != localRef {
		return errors.New("agent exit has no matching local run")
	}
	if r.Exit != nil {
		if r.Exit.Code != code {
			return errors.New("agent exit code changed")
		}
		return nil
	}
	r.Exit = &runnerwire.AgentExit{Ref: placement.RunnerRefPrefix(m.runnerID.String()) + localRef, Code: code, ObservedAt: m.now().UTC()}
	m.state.Runs[id] = r
	return m.save()
}

// State queries the real substrate: a persisted Created ref may have been
// rolled back by a subsequent output-binding failure.
func (m *Manager) State(ctx context.Context) ([]runnerwire.RunState, error) {
	m.mu.Lock()
	ids := m.runIDs()
	m.mu.Unlock()
	out := make([]runnerwire.RunState, 0, len(ids))
	for _, id := range ids {
		m.mu.Lock()
		r := m.state.Runs[id]
		m.mu.Unlock()
		row := runnerwire.RunState{RunID: id, ProxyStoppedAt: r.ProxyStoppedAt}
		if r.Ref != "" {
			st, err := m.sub.Status(ctx, r.Ref)
			if err != nil {
				return nil, err
			}
			row.SandboxPresent = st.State != types.RunStopped || st.ExitCode != nil
			if r.ExecID != "" {
				agent, err := m.sub.AgentStatus(ctx, r.Ref, r.ExecID)
				if err != nil {
					return nil, err
				}
				row.AgentState = string(agent.State)
				row.ExitCode = agent.ExitCode
				if agent.ExitCode != nil && r.Exit == nil {
					m.mu.Lock()
					current := m.state.Runs[id]
					if current.ExecID == r.ExecID && current.Ref == r.Ref && current.Exit == nil {
						current.Exit = &runnerwire.AgentExit{Ref: placement.RunnerRefPrefix(m.runnerID.String()) + r.Ref, Code: *agent.ExitCode, ObservedAt: m.now().UTC()}
						m.state.Runs[id] = current
						err = m.save()
					}
					m.mu.Unlock()
					if err != nil {
						return nil, err
					}
				}
			}
		}
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			return nil, err
		}
		depth, _, drops, err := spool.Stats(ctx)
		closeErr := spool.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return nil, err
		}
		row.SpoolDepth = depth
		row.SpoolDrops = int(min(drops, uint64(^uint(0)>>1)))
		out = append(out, row)
	}
	return out, nil
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
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			return err
		}
		err = spool.Replay(ctx, func(ctx context.Context, row runnerspool.Record) error {
			b, err := row.Marshal()
			if err != nil {
				return err
			}
			return peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &id, Data: b})
		})
		closeErr := spool.Close()
		if err := errors.Join(err, closeErr); err != nil {
			return err
		}
		m.mu.Lock()
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

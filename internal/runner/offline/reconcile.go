// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
		if a.Kind == types.RunnerActionRetire && result.Outcome == "success" {
			// The org holds the receipt now. A failed compaction keeps the full record, as before.
			if err := m.compactRetired(a.RunID); err != nil {
				slog.WarnContext(ctx, "runner kept a retired run's full record", "run_id", a.RunID, "error", err)
			}
		}
	}
	return nil
}

// replayedAction answers an action already applied: done with its saved result, or an error when the
// org replays a different action under the same id. A retirement whose receipt was saved but whose
// output release never ran (a crash between the two) releases it on the org's replay. Release is
// idempotent and refuses unsettled output. m.mu is held.
func (m *Manager) replayedAction(ctx context.Context, a runnerwire.PendingAction) (runnerwire.ActionResult, bool, error) {
	old, ok := m.state.Actions[a.ID]
	if !ok {
		return runnerwire.ActionResult{}, false, nil
	}
	if !samePendingAction(old.Action, a) {
		return runnerwire.ActionResult{}, false, errors.New("pending action identity changed")
	}
	if old.Result.Outcome == "" {
		return runnerwire.ActionResult{}, false, nil
	}
	if r := m.state.Runs[a.RunID]; a.Kind == types.RunnerActionRetire && old.Result.Outcome == "success" && r.Retired && m.releaseOutput != nil {
		if err := m.releaseOutput(a.RunID, r.Ref); err != nil {
			slog.WarnContext(ctx, "runner kept a retired run's output", "run_id", a.RunID, "error", err)
		}
	}
	return old.Result, true, m.save()
}

// acceptNodeRecord hands a security operator's node record to the substrate, an infrastructure fence
// it checks against its own record before it can complete containment (design K1 §3.1 (b)). It runs
// with m.mu unlocked; done means apply returns res and err now, and m.mu is then held again.
func (m *Manager) acceptNodeRecord(ctx context.Context, a runnerwire.PendingAction, local string) (res runnerwire.ActionResult, done bool, err error) {
	acc, ok := m.sub.(runner.NodeRecordAcceptor)
	if !ok {
		m.mu.Lock()
		res, err = m.refuseAction(ctx, a, errors.New("this runner's substrate takes no node record"))
		return res, true, err
	}
	if err = acc.AcceptNodeRecord(ctx, local, *a.NodeRecord); err != nil {
		m.mu.Lock()
		if errors.Is(err, runner.ErrNodeRecordRefused) {
			res, err = m.refuseAction(ctx, a, err)
			return res, true, err
		}
		return runnerwire.ActionResult{}, true, err
	}
	return runnerwire.ActionResult{}, false, nil
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
	if res, done, err := m.replayedAction(ctx, a); done || err != nil {
		return res, err
	}
	r, err := m.pendingRun(a, proof)
	if refused := (actionRefusal{}); errors.As(err, &refused) {
		return m.refuseAction(ctx, a, err)
	}
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	local := r.Ref
	if a.Kind == types.RunnerActionRetire {
		res, err := m.retireRun(ctx, a, r)
		if refused := (actionRefusal{}); errors.As(err, &refused) {
			return m.refuseAction(ctx, a, err)
		}
		return res, err
	}
	if a.Retirement != nil || r.Retired {
		// Refused, not failed: a plain error would fail Pending on every
		// fresh session and keep the runner from READY.
		return m.refuseAction(ctx, a, refusal(errors.New("unexpected retirement proof or retired identity")))
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
		if a.NodeRecord != nil {
			if res, done, err := m.acceptNodeRecord(ctx, a, local); done {
				return res, err
			}
		}
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

// errReplayHeld ends one run's spool replay when the run was quarantined or retired while it ran.
var errReplayHeld = errors.New("offline: the run was quarantined or retired during its spool replay")

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
	// A spool that will not open is that run's own fault: the other runs' evidence is still sent, and
	// the error is returned last, so this runner does not come ready with a spool it never replayed.
	var unopened []error
	for _, id := range ids {
		m.mu.Lock()
		quarantined := m.state.Runs[id].Quarantine != nil || m.state.Runs[id].Retired
		m.mu.Unlock()
		if quarantined {
			continue
		}
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			unopened = append(unopened, fmt.Errorf("open the spool of run %s: %w", id, err))
			continue
		}
		err = spool.Replay(ctx, func(ctx context.Context, rows []runnerspool.Record) error {
			// A quarantine or retirement that lands mid-replay stops the rest of this run's backlog, as
			// one that landed before the replay began does.
			m.mu.Lock()
			held := m.state.Runs[id].Quarantine != nil || m.state.Runs[id].Retired
			m.mu.Unlock()
			if held {
				return errReplayHeld
			}
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
		if errors.Is(err, errReplayHeld) {
			// Held mid-replay: like a run held before it began, its exit is not sent either.
			if closeErr != nil {
				return closeErr
			}
			continue
		}
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
	return errors.Join(unopened...)
}

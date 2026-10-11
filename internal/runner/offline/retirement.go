// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func exitDigest(exit *runnerwire.AgentExit) string {
	if exit == nil {
		return ""
	}
	b, _ := json.Marshal(exit)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// retireRun runs under actionMu and mu. Kill is already irreversible and
// confirmed; the authenticated org separately attests terminal-tail settlement.
func (m *Manager) retireRun(ctx context.Context, a runnerwire.PendingAction, r localRun) (runnerwire.ActionResult, error) {
	if err := retirementProof(a, r); err != nil {
		return runnerwire.ActionResult{}, err
	}
	p := a.Retirement
	// A fresh action may carry a newer evidence watermark after deferred replay.
	// Its execution/exit identity remains fixed by the comparisons above.
	if !m.mutationsDrained(a.RunID) || (r.Exit != nil && !r.ExitAcked) || (r.MutationFailure != nil && !r.MutationFailure.Contained) {
		return m.actionReceipt(a, "deferred")
	}
	end, settled, err := m.outputSettled(ctx, a.RunID, r.Ref)
	if errors.Is(err, runner.ErrOutputUnrecoverable) {
		return m.actionReceipt(a, "deferred")
	}
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	if !settled || end != p.OutputEnd {
		return m.actionReceipt(a, "deferred")
	}
	proof := *p
	a.Retirement = &proof
	r.Retirement = &proof
	m.state.Runs[a.RunID] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	if err := m.save(); err != nil {
		return runnerwire.ActionResult{}, err
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", a.RunID.String()), a.RunID)
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	sealed, err := spool.Seal(ctx, p.Spool)
	if err := errors.Join(err, spool.Close()); err != nil {
		return runnerwire.ActionResult{}, err
	}
	if !sealed {
		return m.actionReceipt(a, "deferred")
	}
	r.Retired = true
	m.state.Runs[a.RunID] = r
	// Retained run, execution and action tombstones prevent ID reuse, and
	// neither an exit ACK nor this operation advances evidence ACKs. Retirement
	// deletes one thing: the run's settled output, released only after the
	// retirement is durable. Erasing evidence a run asserted is a separate path.
	result, err := m.actionReceipt(a, "success")
	if err != nil || m.releaseOutput == nil {
		return result, err
	}
	// The run is retired either way; output a release refuses or fails to
	// delete stays on disk, as it did before release existed.
	if err := m.releaseOutput(a.RunID, r.Ref); err != nil {
		slog.WarnContext(ctx, "runner kept a retired run's output", "run_id", a.RunID, "error", err)
	}
	return result, nil
}

// ReleaseSettledOutput sets the hook that deletes a retired run's output. The
// hook must refuse output that is not settled (complete and fully
// acknowledged). Without it, retirement keeps all output.
func (m *Manager) ReleaseSettledOutput(release func(runID uuid.UUID, ref string) error) {
	m.mu.Lock()
	m.releaseOutput = release
	m.mu.Unlock()
}

func retirementProof(a runnerwire.PendingAction, r localRun) error {
	p := a.Retirement
	if p == nil || p.ID == uuid.Nil || a.Recovery != nil || a.Quarantine != nil || r.Quarantine != nil || r.Retired {
		return errors.New("retirement requires exact nonquarantined settlement")
	}
	if p.Generation != r.Generation || p.ExecID != r.ExecID || p.ExitHash != exitDigest(r.Exit) || (r.Generation == uuid.Nil && len(r.Executions) != 0) || p.OutputEnd < 0 || p.Spool.SpoolID == uuid.Nil {
		return errors.New("retirement evidence identity changed")
	}
	if r.Containment == nil || r.Containment.ID == uuid.Nil || r.Containment.Generation != r.Generation || r.Containment.Operation != string(types.RunnerActionKill) || !r.Containment.Contained || r.ProxyStoppedAt == nil {
		return errors.New("retirement requires settled terminal containment")
	}
	return nil
}

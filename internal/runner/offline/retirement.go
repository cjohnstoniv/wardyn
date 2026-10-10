// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

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
	p := a.Retirement
	if p == nil || p.ID == uuid.Nil || a.Recovery != nil || a.Quarantine != nil || r.Quarantine != nil || r.Retired {
		return runnerwire.ActionResult{}, errors.New("retirement requires exact nonquarantined settlement")
	}
	if p.Generation != r.Generation || p.ExecID != r.ExecID || p.ExitHash != exitDigest(r.Exit) || (r.Generation == uuid.Nil && len(r.Executions) != 0) || p.OutputEnd < 0 || p.Spool.SpoolID == uuid.Nil {
		return runnerwire.ActionResult{}, errors.New("retirement evidence identity changed")
	}
	if r.Containment == nil || r.Containment.Operation != string(types.RunnerActionKill) || !r.Containment.Contained || r.ProxyStoppedAt == nil {
		return runnerwire.ActionResult{}, errors.New("retirement requires settled terminal containment")
	}
	// A fresh action may carry a newer evidence watermark after deferred replay.
	// Its execution/exit identity remains fixed by the comparisons above.
	if !m.mutationsDrained(a.RunID) || (r.Exit != nil && !r.ExitAcked) || (r.MutationFailure != nil && !r.MutationFailure.Contained) {
		return m.quarantineReceipt(a, "deferred")
	}
	end, settled, err := m.outputSettled(ctx, a.RunID, r.Ref)
	if err != nil {
		return runnerwire.ActionResult{}, err
	}
	if !settled || end != p.OutputEnd {
		return m.quarantineReceipt(a, "deferred")
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
		return m.quarantineReceipt(a, "deferred")
	}
	r.Retired = true
	m.state.Runs[a.RunID] = r
	// Retained run, execution and action tombstones prevent ID reuse. Nothing is
	// deleted and neither an exit ACK nor this operation advances evidence ACKs.
	return m.quarantineReceipt(a, "success")
}

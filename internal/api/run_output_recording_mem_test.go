// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

type memRecordingOutput struct {
	requested, completed int64
	token                uuid.UUID
	claimedAt            time.Time
	erased               bool
}

func (m *memRunOutputs) recordingEligible(ctx context.Context, id uuid.UUID, retention time.Duration) bool {
	run, err := m.Store.GetRun(ctx, id)
	if err != nil || run.Interactive || retention > 0 && run.EndedAt != nil && time.Since(*run.EndedAt) > retention {
		return false
	}
	r, found := m.rows[id]
	return !found || r.CapturedAt == nil || r.Source == recordingOutputSource || r.Source == "stdout" && r.CaptureGap
}

func (m *memRunOutputs) recordingFence(id uuid.UUID) error {
	if m.erased[id] {
		return store.ErrRunOutputErased
	}
	if m.recording[id].erased {
		return store.ErrRecordingOutputErased
	}
	return nil
}

func (m *memRunOutputs) QueueRecordingRunOutput(ctx context.Context, id uuid.UUID, retention, retryAfter time.Duration, newRecording bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recordingFence(id); err != nil {
		return err
	}
	if !m.recordingEligible(ctx, id, retention) {
		return nil
	}
	if r, found := m.rows[id]; !newRecording && found && r.Source == recordingOutputSource && r.CapturedAt != nil && !r.CaptureGap {
		return nil
	}
	p, found := m.recording[id]
	if !found || newRecording || p.requested == p.completed && time.Since(p.claimedAt) > retryAfter {
		p.requested++
		m.recording[id] = p
	}
	return nil
}

func (m *memRunOutputs) ClaimRecordingRunOutput(ctx context.Context, id uuid.UUID, retention, staleAfter time.Duration) (store.RecordingOutputClaim, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claim := store.RecordingOutputClaim{RunID: id, Token: uuid.New()}
	if err := m.recordingFence(id); err != nil {
		return claim, false, err
	}
	p := m.recording[id]
	if !m.recordingEligible(ctx, id, retention) {
		p.completed, p.token = p.requested, uuid.Nil
		m.recording[id] = p
		return claim, false, nil
	}
	run, err := m.Store.GetRun(ctx, id)
	if err != nil || !run.State.IsTerminal() || p.requested == p.completed || p.token != uuid.Nil && time.Since(p.claimedAt) < staleAfter {
		return claim, false, err
	}
	p.token, p.claimedAt, claim.Generation = claim.Token, time.Now(), p.requested
	m.recording[id] = p
	return claim, true, nil
}

func (m *memRunOutputs) SaveRecordingRunOutput(ctx context.Context, claim store.RecordingOutputClaim, o store.RunOutput, retention time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recordingFence(claim.RunID); err != nil {
		return false, err
	}
	if m.failSave > 0 {
		m.failSave--
		return false, errors.New("postgres is down")
	}
	p := m.recording[claim.RunID]
	if p.token != claim.Token {
		return false, nil
	}
	eligible := m.recordingEligible(ctx, claim.RunID, retention)
	p.token = uuid.Nil
	if eligible && p.requested != claim.Generation {
		m.recording[claim.RunID] = p
		return false, nil
	}
	p.completed = p.requested
	m.recording[claim.RunID] = p
	if !eligible {
		return false, nil
	}
	if prior, found := m.rows[claim.RunID]; o.CaptureGap && found && prior.CapturedAt != nil {
		return false, nil
	}
	now := time.Now()
	o.Output = append([]byte{}, o.Output...)
	o.CapturedAt, o.ClaimedAt, o.Incomplete = &now, now, true
	m.rows[claim.RunID] = o
	m.saves[claim.RunID]++
	return true, nil
}

func (m *memRunOutputs) ListPendingRecordingRunOutputs(ctx context.Context, staleAfter time.Duration, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for id, p := range m.recording {
		if p.erased || p.requested == p.completed || p.token != uuid.Nil && time.Since(p.claimedAt) < staleAfter || len(ids) >= limit {
			continue
		}
		if run, err := m.Store.GetRun(ctx, id); err == nil && run.State.IsTerminal() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memRunOutputs) EraseRecordingRunOutputs(_ context.Context, ids []uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range ids {
		p := m.recording[id]
		p.erased, p.completed, p.token = true, p.requested, uuid.Nil
		m.recording[id] = p
		if r, found := m.rows[id]; found && r.Source == recordingOutputSource {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

var _ store.RunOutputStore = (*memRunOutputs)(nil)

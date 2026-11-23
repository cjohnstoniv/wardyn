// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"errors"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// A run's substrate evidence record on disk. Split out of manager.go for scripts/check-file-size.sh.

// LoadEvidence returns a run's substrate evidence record (nil when none).
func (m *Manager) LoadEvidence(runID uuid.UUID) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.state.Runs[runID].Substrate), nil
}

// SaveEvidence writes a run's substrate evidence record with the state's atomic write and fsync. A
// run the runner holds no record of (a lost identity met by the sweep) gets a containment-only
// record, which CheckLapses contains.
func (m *Manager) SaveEvidence(runID uuid.UUID, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if runID == uuid.Nil {
		return errors.New("evidence for no run")
	}
	r, ok := m.state.Runs[runID]
	if !ok {
		ref := m.expectedRef(runID)
		if ref == "" {
			return errors.New("evidence for a run with no recoverable reference")
		}
		r = localRun{RunID: runID, Ref: ref, ContainmentOnly: true, Lease: runnerwire.Lease{RunID: runID}}
	}
	before := m.state.Runs[runID]
	r.Substrate = slices.Clone(b)
	m.state.Runs[runID] = r
	if err := m.save(); err != nil {
		if ok {
			m.state.Runs[runID] = before
		} else {
			delete(m.state.Runs, runID)
		}
		return err
	}
	return nil
}

// SinkRunIDs lists the runs whose decision sink is registered again at startup: those holding a
// substrate evidence record that are neither retired nor erased, so a spool is never reopened.
func (m *Manager) SinkRunIDs() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for _, id := range m.runIDs() {
		if r := m.state.Runs[id]; len(r.Substrate) > 0 && !r.Retired && !r.EvidenceErased {
			ids = append(ids, id)
		}
	}
	return ids
}

// EvidenceRunIDs lists the unretired runs holding a substrate evidence record, for the relay's
// fence replay: a fence is dropped with the run's offline state at retirement.
func (m *Manager) EvidenceRunIDs() []uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []uuid.UUID
	for _, id := range m.runIDs() {
		if r := m.state.Runs[id]; len(r.Substrate) > 0 && !r.Retired {
			ids = append(ids, id)
		}
	}
	return ids
}

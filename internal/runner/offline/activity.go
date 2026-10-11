// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"path/filepath"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/google/uuid"
)

func (m *Manager) activityDir(id uuid.UUID) string {
	return filepath.Join(m.dir, "spools", id.String())
}

// Called under mu. Activity is best effort; a missing epoch conservatively
// loses a keepalive, never reuses authority from the prior connection.
func (m *Manager) publishActivity() {
	for id := range m.state.Runs {
		_ = runneractivity.Publish(m.activityDir(id), m.activityEpoch)
	}
}

func (m *Manager) activityLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		ids := m.runIDs()
		m.mu.Unlock()
		for _, id := range ids {
			marker, err := runneractivity.Read(m.activityDir(id))
			if err != nil {
				continue
			}
			m.mu.Lock()
			peer, epoch := m.peer, m.activityEpoch
			eligible := m.state.Runs[id].Containment == nil && m.state.Runs[id].Quarantine == nil && !m.dirty && m.state.Runs[id].MutationFailure == nil && m.online && m.replayReady && peer != nil && epoch != uuid.Nil && marker.Epoch == epoch && m.activitySeen[id] != marker.ID && !m.expired(id) && !m.state.Runs[id].FailedCreation
			if eligible {
				m.activitySeen[id] = marker.ID
			}
			m.mu.Unlock()
			if eligible {
				signal, cancel := context.WithTimeout(ctx, time.Second)
				_ = peer.CurrentActivity(signal, id)
				cancel()
			}
		}
	}
}

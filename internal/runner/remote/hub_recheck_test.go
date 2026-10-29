// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A limits recheck decided on the pair an old session carried must not end the session that replaced it
// after a reconnect, whose AUTH_OK named the stored pair; the same recheck on the still-current session
// does end it.
func TestHubLimitsRecheckEndsOnlyTheSessionItServed(t *testing.T) {
	for _, reconnected := range []bool{true, false} {
		name := "same session"
		if reconnected {
			name = "replaced by a reconnect"
		}
		t.Run(name, func(t *testing.T) {
			row := types.Runner{ID: uuid.New(), Owner: "owner", State: types.RunnerClaimed,
				Limits: &types.RunnerLimits{RunsAtOnce: 1, PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}}}
			hub := NewHub(t.Context(), HubConfig{Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }})
			t.Cleanup(hub.Close)
			link := &Link{}
			orgOld, localOld := runnerwire.Loopback()
			t.Cleanup(func() { _ = localOld.Close() })
			served := link.AttachSession(orgOld, runnerwire.PeerConfig{})
			current := served
			if reconnected {
				orgNew, localNew := runnerwire.Loopback()
				t.Cleanup(func() { _ = localNew.Close() })
				current = link.AttachSession(orgNew, runnerwire.PeerConfig{})
			}
			if err := link.Ready(current); err != nil {
				t.Fatal(err)
			}
			s := &hubSession{id: "session", peer: current, link: link}
			id := s.id
			s.readyID.Store(&id)
			s.perRun.Store(&perRunLimits{cpuMillis: 2000, memoryMiB: 4096})
			hub.sessions[row.ID] = s

			hub.recheck(t.Context(), row.ID, s, served)

			if got := !current.Online(); got != !reconnected {
				t.Errorf("current session ended = %v, want %v", got, !reconnected)
			}
			if got := link.Online(); got != reconnected {
				t.Errorf("link online = %v, want %v", got, reconnected)
			}
			if got := s.readyID.Load() != nil; got != reconnected {
				t.Errorf("session still ready = %v, want %v", got, reconnected)
			}
		})
	}
}

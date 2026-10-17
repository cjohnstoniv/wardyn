// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxAttachSessionsPerRun bounds the live attach sessions one run may have on this replica:
// writers and observers, web and SSH, together. Each one is an exec in the sandbox and a cast
// buffered in memory up to maxSessionCastBytes until it is persisted, observers included. Eight is
// the UI gateway's per-run connection cap (maxUIConnsPerRun), the sibling bound. It is per
// replica: it bounds this process's memory, not a deployment's.
const maxAttachSessionsPerRun = 8

// attachSessionCapMsg is what the refused client is told.
var attachSessionCapMsg = fmt.Sprintf("too many open terminal sessions for this run (max %d) — close one and retry", maxAttachSessionsPerRun)

// reserveAttachSlot counts one more live attach session for runID, false when the run is at
// maxAttachSessionsPerRun. It is taken before the exec is opened or the recorder built, and the
// returned release, idempotent, is called once the session's recording is finalized, not when
// it gives up the keyboard: a cast still being persisted is still in memory.
func (s *Server) reserveAttachSlot(runID uuid.UUID) (release func(), ok bool) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.slots == nil {
		reg.slots = map[uuid.UUID]int{}
	}
	if reg.slots[runID] >= maxAttachSessionsPerRun {
		return nil, false
	}
	reg.slots[runID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			reg.mu.Lock()
			defer reg.mu.Unlock()
			if reg.slots[runID]--; reg.slots[runID] <= 0 {
				delete(reg.slots, runID)
			}
		})
	}, true
}

// webAttachSlot is reserveAttachSlot for the web lane, before the upgrade: a refusal is answered
// here, with nil returned.
func (s *Server) webAttachSlot(ctx context.Context, w http.ResponseWriter, run types.AgentRun, actorType types.ActorType, principal string) (release func()) {
	release, ok := s.reserveAttachSlot(run.ID)
	if !ok {
		s.auditAttachSessionCap(ctx, run, actorType, principal, map[string]any{})
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAttachSessionCapReached, attachSessionCapMsg)
	}
	return release
}

// auditAttachSessionCap records an attach refused at maxAttachSessionsPerRun.
func (s *Server) auditAttachSessionCap(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal string, data map[string]any) {
	data["state"] = "refused"
	data["reason"] = reasonAttachSessionCapReached
	data["max"] = maxAttachSessionsPerRun
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, actorType, principal, "session.attach",
		run.ID.String(), "failure", mustJSON(data)))
}

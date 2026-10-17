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

// What the refused client is told, naming the cap that refused it: the run's, or on a run more
// than one principal can attach to, the person's own.
var (
	attachSessionCapMsg          = fmt.Sprintf("too many open terminal sessions for this run (max %d) — close one and retry", maxAttachSessionsPerRun)
	attachPrincipalSessionCapMsg = fmt.Sprintf("you have too many open terminal sessions on this run (max %d per person) — close one and retry", maxAttachSessionsPerPrincipal)
)

// maxAttachSessionsPerPrincipal bounds one principal's live attach sessions on a run that can admit
// more than one principal, so no single principal can fill the run and lock the others out. A personal
// run admits only its owner's own principal, so it keeps the run cap alone.
const maxAttachSessionsPerPrincipal = maxAttachSessionsPerRun - 1

// attachPrincipalKey is one principal's count of live attach sessions on one run.
type attachPrincipalKey struct {
	run       uuid.UUID
	principal string
}

// multiPrincipalAttach reports whether more than one principal can attach to run: an operator-owned
// run (any super admin enters it) or a service identity's run (any of its sponsors).
func multiPrincipalAttach(run types.AgentRun) bool {
	_, service := serviceOfRun(run)
	return run.OperatorOwned || service
}

// reserveAttachSlot counts one more live attach session for run, false when the run is at
// maxAttachSessionsPerRun or, on a run more than one principal can attach to, principal is at
// maxAttachSessionsPerPrincipal. It is taken before the exec is opened or the recorder built, and the
// returned release, idempotent, is called once the session's recording is finalized, not when
// it gives up the keyboard: a cast still being persisted is still in memory.
func (s *Server) reserveAttachSlot(run types.AgentRun, principal string) (release func(), ok bool) {
	release, refused := s.reserveAttachSlotOrSay(run, principal)
	return release, refused == ""
}

// reserveAttachSlotOrSay is reserveAttachSlot answering a refusal with the sentence of the cap
// that refused it (atCap), "" when the slot was taken.
func (s *Server) reserveAttachSlotOrSay(run types.AgentRun, principal string) (release func(), refused string) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if reg.slots == nil {
		reg.slots = map[uuid.UUID]int{}
		reg.principalSlots = map[attachPrincipalKey]int{}
	}
	key := attachPrincipalKey{run.ID, principal}
	perPrincipal := multiPrincipalAttach(run)
	if refused := reg.atCap(run, principal); refused != "" {
		return nil, refused
	}
	reg.slots[run.ID]++
	if perPrincipal {
		reg.principalSlots[key]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			reg.mu.Lock()
			defer reg.mu.Unlock()
			if reg.slots[run.ID]--; reg.slots[run.ID] <= 0 {
				delete(reg.slots, run.ID)
			}
			if perPrincipal {
				if reg.principalSlots[key]--; reg.principalSlots[key] <= 0 {
					delete(reg.principalSlots, key)
				}
			}
		})
	}, ""
}

// atCap is reserveAttachSlot's refusal, as the sentence that names the cap: run is at
// maxAttachSessionsPerRun or, on a run more than one principal can attach to, principal is at
// maxAttachSessionsPerPrincipal. "" when neither holds. The caller holds reg.mu.
func (reg *attachHolderRegistry) atCap(run types.AgentRun, principal string) string {
	switch {
	case reg.slots[run.ID] >= maxAttachSessionsPerRun:
		return attachSessionCapMsg
	case multiPrincipalAttach(run) && reg.principalSlots[attachPrincipalKey{run.ID, principal}] >= maxAttachSessionsPerPrincipal:
		return attachPrincipalSessionCapMsg
	}
	return ""
}

// attachCapMax is the cap a refusal sentence names, for the audit row's max.
func attachCapMax(msg string) int {
	if msg == attachPrincipalSessionCapMsg {
		return maxAttachSessionsPerPrincipal
	}
	return maxAttachSessionsPerRun
}

// attachSlotsFull is the sentence of the cap principal's next attach to run would be refused at on
// this replica, "" when none. It reserves nothing: the ticket mint asks it so the person is told the
// cap in a response they can read (a failed WebSocket handshake shows a browser no status or body),
// and the upgrade still takes the slot itself.
func (s *Server) attachSlotsFull(run types.AgentRun, principal string) string {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.atCap(run, principal)
}

// webAttachSlot is reserveAttachSlot for the web lane, before the upgrade: a refusal is answered
// here, with nil returned.
func (s *Server) webAttachSlot(ctx context.Context, w http.ResponseWriter, run types.AgentRun, actorType types.ActorType, principal string) (release func()) {
	release, refused := s.reserveAttachSlotOrSay(run, principal)
	if refused != "" {
		s.auditAttachSessionCap(ctx, run, actorType, principal, refused, map[string]any{})
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAttachSessionCapReached, refused)
	}
	return release
}

// auditAttachSessionCap records an attach refused at the run's or the principal's cap; max is the
// cap that refused it (refused is that cap's sentence).
func (s *Server) auditAttachSessionCap(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal, refused string, data map[string]any) {
	data["state"] = "refused"
	data["reason"] = reasonAttachSessionCapReached
	data["max"] = attachCapMax(refused)
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, actorType, principal, "session.attach",
		run.ID.String(), "failure", mustJSON(data)))
}

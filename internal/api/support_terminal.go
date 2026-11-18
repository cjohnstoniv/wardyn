// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The terminal and POST /exec doors of a support grant (support_entry.go holds the arm). The doors themselves
// call supportMayEnter, so the census (support_census_test.go) can name them; this file holds what they share:
// the door a ticket's purpose maps to, the fields a helper's rows carry, and the owner's claim on the writer seat.

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// supportOwnerJoinedReason is the close reason a displaced helper's client reads when the run's owner
	// attached while the helper held the terminal.
	supportOwnerJoinedReason = "The run's owner joined"
	// supportTakeoverOwnerJoined is the session.takeover reason of that displacement.
	supportTakeoverOwnerJoined = "owner_joined"
	// supportEntryHelper is the entry field of a row a helper's door wrote.
	supportEntryHelper = "support"
	// supportPathModeMsg answers a web apps or desktop entry on a deployment serving apps from one shared host.
	supportPathModeMsg = "This deployment serves apps from one shared host, so web apps and the desktop cannot be shared with a helper."
)

// supportTicketDoor is the door an attach ticket's purpose opens: "" is the terminal and "ui" the UI gateway's
// two doors. known is false for any other purpose, which no support ticket may carry (a purpose names a lane the
// grant's doors do not reach), so the caller refuses it after asking whether a grant is live at all.
func supportTicketDoor(purpose string) (door supportDoor, known bool) {
	switch purpose {
	case "":
		return supportDoorTerminal, true
	case "ui":
		return supportDoorUI, true
	}
	return supportDoorTerminal, false
}

// refuseSupportTicket is a support ticket's last checks before it is minted, true when it has answered: a UI
// ticket on a deployment serving apps from one shared host is refused (the doors are not offered there), and a
// background run takes no helper.
func (s *Server) refuseSupportTicket(w http.ResponseWriter, r *http.Request, run types.AgentRun, purpose string) bool {
	if purpose == "ui" && s.uiRunOrigin(run.ID) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonInboundNeedsHostMode, supportPathModeMsg)
		return true
	}
	return s.refuseBackgroundOnly(w, r, run, "terminal")
}

// supportTicketGrant is the grant id and the expiry a helper's ticket carries.
func supportTicketGrant(g store.SupportGrant) (uuid.UUID, time.Time) {
	var expires time.Time
	if g.ExpiresAt != nil {
		expires = *g.ExpiresAt
	}
	return g.ID, expires
}

// supportEntryFields are the fields a row a helper's door wrote carries beside the window stamp.
func supportEntryFields(grant uuid.UUID) map[string]any {
	return map[string]any{"support_grant": grant, "entry": supportEntryHelper}
}

type supportExecCtxKey struct{}

// withSupportExec marks ctx as a helper's POST /exec, so auditRunExec adds the entry fields to every row of it.
func withSupportExec(ctx context.Context, grant uuid.UUID) context.Context {
	return context.WithValue(ctx, supportExecCtxKey{}, grant)
}

// supportExecFields adds a helper's entry fields to data when ctx carries a grant.
func supportExecFields(ctx context.Context, data map[string]any) map[string]any {
	if grant, ok := ctx.Value(supportExecCtxKey{}).(uuid.UUID); ok {
		for k, v := range supportEntryFields(grant) {
			data[k] = v
		}
	}
	return data
}

// hasOwnObserver reports whether a client that is not a helper waits behind the writer.
func hasOwnObserver(ra *runAttach) bool {
	return slices.ContainsFunc(ra.observers, func(o *attachHolder) bool { return o.support == uuid.Nil })
}

// nextObserver is the index of the observer a free seat goes to: the oldest that is not a helper, else the
// oldest. -1 when none waits.
func nextObserver(ra *runAttach) int {
	if i := slices.IndexFunc(ra.observers, func(o *attachHolder) bool { return o.support == uuid.Nil }); i >= 0 {
		return i
	}
	if len(ra.observers) > 0 {
		return 0
	}
	return -1
}

// evictSupportWriter is the owner's claim on the writer seat: owner, which came back read-only, was queued behind
// helper's session on this replica. It takes the seat as evictStaleWriter does: the eviction that promotes
// owner's own socket under the registry lock, then the session.takeover row (reason owner_joined), then the
// displacement of helper, whose client reads supportOwnerJoinedReason, then the promotion's notice. The row is
// written before displace so it is never lost to the teardown it describes. Nothing happens when helper already
// left. The helper may attach again, and then observes: the registry seats a helper only behind no owner client.
func (s *Server) evictSupportWriter(runID uuid.UUID, owner, helper *attachHolder) {
	prev, promote := s.evictAttachWriter(runID, owner.principal, helper)
	if prev == nil {
		return // it left, or was replaced, while the owner was registering
	}
	ctx := s.cfg.BaseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if owner.via != nil {
		ctx = audit.WithDelegation(ctx, *owner.via)
	}
	s.recordTakeover(ctx, runID, owner.sandboxRef, owner.actorType, owner.principal, prev, supportTakeoverOwnerJoined)
	prev.displace(supportOwnerJoinedReason)
	if promote != nil {
		go promote()
	}
}

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentPinnedMsg is what a run is told when a component it carries is now pinned to a unit the run
// is not in (componentPinnedOut).
var componentPinnedMsg = "wardyn: this run is no longer given credentials: a component it carries is now pinned to a unit this run is not in; start a new run in that unit"

// componentCut is the reason and text every live door refuses a run with when a component it carries is
// pinned to a unit the run is not in (pinned) or deleted or no longer its owner's to use (withdrawn, a
// componentWithdrawn reason), or "" when neither. withdrawn wins: a new run in the pinned unit would not
// get a withdrawn component either.
func componentCut(pinned bool, withdrawn string) (reason, msg string) {
	switch {
	case withdrawn != "":
		return withdrawn, componentWithdrawnMsgs[withdrawn]
	case pinned:
		return reasonComponentUnitPinned, componentPinnedMsg
	}
	return "", ""
}

// gatewayComponentCut is the gateway identity mint's ask, for an organisation's MCP server, of what the
// credential mint and redeem ask of the run's components (liveWithholding): 403 with componentCut's reason
// and text, or the door's own 503 when they cannot be read, never a token on an unread check. Every MCP
// server's token on the run is refused alike, as every credential on it is.
func (s *Server) gatewayComponentCut(ctx context.Context, run types.AgentRun) *ownerRefusal {
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	var pinned, withdrawn string
	if err == nil {
		pinned, err = s.componentPinnedOut(ctx, run, comps)
	}
	if err == nil {
		withdrawn, err = s.componentWithdrawn(ctx, run, comps)
	}
	if err != nil {
		slog.ErrorContext(ctx, "gateway identity: read the run's components", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return gatewayUnavailable()
	}
	if reason, msg := componentCut(pinned != "", withdrawn); reason != "" {
		return &ownerRefusal{status: http.StatusForbidden, reason: reason, msg: msg}
	}
	return nil
}

// gatewayComponentCutReason says whether a refusal is gatewayComponentCut's.
func gatewayComponentCutReason(reason string) bool {
	return reason == reasonComponentUnitPinned || reason == reasonOwnerCapabilityComponent || reason == reasonOwnerComponentGone
}

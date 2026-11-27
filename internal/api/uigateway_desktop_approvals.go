// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// uiDesktopApprovals is the desktop viewer's read of the sites its run is waiting on (#2964). It lists and never
// decides: the viewer shares its origin with the run's own relayed apps, so a page the agent serves there could send
// anything the viewer can. A person answers on the run's page in the console, under the console's own session.
const uiDesktopApprovals = "/__approvals"

// desktopApproval is one waiting site as the viewer shows it. Nothing else of the approval leaves the server here.
type desktopApproval struct {
	ID          uuid.UUID  `json:"id"`
	Host        string     `json:"host"`
	Held        bool       `json:"held,omitempty"`
	HeldUntil   *time.Time `json:"held_until,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
}

// serveDesktopApprovals answers GET <desktop>/__approvals for an open desktop session of runID: the run's PENDING
// egress_domain approvals, in the store's order. A failed read answers 503 and lists nothing, so the viewer never
// says that nothing is waiting when it could not tell.
func (s *Server) serveDesktopApprovals(w http.ResponseWriter, r *http.Request, runID uuid.UUID) {
	byRun, err := s.pendingApprovalsForRuns(r, []uuid.UUID{runID})
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonStoreError, "could not read the sites this run is waiting on; try again")
		return
	}
	out := []desktopApproval{}
	for _, ap := range byRun[runID] {
		if ap.Kind != types.ApprovalEgressDomain {
			continue
		}
		out = append(out, desktopApproval{ID: ap.ID, Host: approvalHost(ap), Held: ap.Held, HeldUntil: ap.HeldUntil, RequestedAt: ap.RequestedAt})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out})
}

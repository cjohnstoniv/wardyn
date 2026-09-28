/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approvals_reauth.go — the ONE helper decide() needs for the credential_reauth
// kind, in its own file because approvals.go sits at the 1000-line cap and this
// helper is the seam the kind rule introduced.

// canSeeApproval reports whether this caller may be told that ap EXISTS — the
// security tier, which decides any kind on any run, or a caller who owns ap's
// run (or is an admin), which is the same ownership rule authorizeUserDecision
// (approvals.go) applies. Everyone else is told nothing, so no refusal above can become
// the existence oracle the two 404s in that gate exist to deny.
func (s *Server) canSeeApproval(r *http.Request, ap types.ApprovalRequest) bool {
	if s.isSecurityOperator(r.Context()) {
		return true
	}
	run, err := s.cfg.Store.GetRun(r.Context(), ap.RunID)
	return err == nil && s.ownsRunOrAdmin(r, run)
}

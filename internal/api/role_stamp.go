// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleStampStale reports whether t's role and group stamp is older than WARDYN_ROLE_STAMP_TTL at now.
// Off (TTL zero) is never stale.
//
// Wardyn keeps no identity-provider token, so it cannot re-derive a role on its
// own: a demotion made only at the identity provider reaches a token when its
// owner next signs in, and OnLogin re-stamps it. This is the bound on how long
// that can take. A stamp that was never set reads as stale (sshRoleFresh), the
// fail-closed direction.
func (s *Server) roleStampStale(t types.APIToken, now time.Time) bool {
	return s.cfg.RoleStampTTL > 0 && !sshRoleFresh(t.IdentityStampedAt, now, s.cfg.RoleStampTTL)
}

// refuseStaleRoleStamp answers a token whose stamp is stale (roleStampStale) with
// 401 role_stamp_stale and its authz.denied row, and reports whether it did.
func (s *Server) refuseStaleRoleStamp(w http.ResponseWriter, r *http.Request, t types.APIToken) bool {
	if !s.roleStampStale(t, s.cfg.Now().UTC()) {
		return false
	}
	s.answerStaleRoleStamp(w, r, t)
	return true
}

// answerStaleRoleStamp writes the 401 role_stamp_stale and its authz.denied row
// for a token already found stale.
//
// The row is attributed to the token's owner, who is known by now, though the
// request is not yet authenticated as them: only the subject is put on the
// context, never the stale role.
func (s *Server) answerStaleRoleStamp(w http.ResponseWriter, r *http.Request, t types.APIToken) {
	r = r.WithContext(withOIDCHuman(r.Context(), t.Principal))
	s.refuse(w, r, authz.Deny(authz.ReasonRoleStampStale, "api_token", ""))
}

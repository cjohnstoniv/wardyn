// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// handleMe reports the authenticated principal, how they authenticated, their
// B1-derived role, their session email, and whether they hold the admin role,
// so the UI can show the real signed-in user instead of a placeholder AND hide
// the admin-only actions instead of letting a member discover them as raw
// 403s. It sits behind humanOrAdminAuth, so reaching it already proves
// authentication.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	method := "token"
	switch {
	case s.cfg.LocalMode:
		method = "local"
	case oidc.PrincipalFromContext(r.Context()) != "":
		method = "sso"
	}
	principal := principalFromRequest(r)
	role := oidc.RoleAdmin
	if !s.isOperator(r.Context()) {
		role = oidc.RoleMember
	}
	body := map[string]any{
		"principal": principal,
		"method":    method,
		"role":      role,
		"email":     oidcEmailFromContext(r.Context()),
		// The SAME predicate operatorOnly gates every admin route with
		// (isOperator, http.go) — never a second, driftable copy of the rule.
		// Kept for the console, which already consumes it: operator == role==admin.
		"operator": s.isOperator(r.Context()),
	}
	// M3: the AddWorkspaceDialog root-constraint hint (member-role-desktop.md
	// §DECISIONS O1, ui-batch2-mock.md's "New wire this mock assumes"). null for
	// an operator (the dialog never renders the hint for one) and for a member
	// with no configured root either way (RootsFor's own empty-means-unavailable
	// contract). Presentational only — ValidateMemberMountSource, not this
	// value, is what actually enforces the boundary at bind time.
	body["member_local_dir_root"] = nil
	if role == oidc.RoleMember {
		body["member_local_dir_root"] = memberLocalDirRootLabel(s.cfg.MemberMounts.RootsFor(principal))
	}
	// W31-S1-7: an SSO session dies outright at this instant (no refresh) — the
	// console polls this and warns ahead of it, rather than the human learning
	// about it from a sudden 401 that wipes mid-work state back to the gate.
	// Omitted (zero) for local/token auth, which has no session to expire.
	if exp := oidcExpiryFromContext(r.Context()); !exp.IsZero() {
		body["session_expires_at"] = exp.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, body)
}

// memberLocalDirRootLabel renders the operator's configured member root(s) as
// the short human-readable prose the dialog shows before a path is typed —
// never a dump of every prefix (ui-batch2-mock.md's M3 wire note). No "under "
// prefix: the UI's ROOT_HINT template (permissions-copy.ts) already supplies
// that word, so this returns the bare root(s) to avoid doubling it in the
// composed sentence. nil when roots is empty, the same "unavailable" signal
// RootsFor already uses.
func memberLocalDirRootLabel(roots []string) *string {
	if len(roots) == 0 {
		return nil
	}
	label := strings.Join(roots, " or ")
	return &label
}

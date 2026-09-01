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
	// The REAL session role, not a two-valued re-derivation of it: since 0.7
	// the tier set is three-valued (admin / security_admin / member) and
	// collapsing it through isOperator here would report a security admin as a
	// plain member — the console's own account chip, and every consumer of this
	// field, would then contradict what the server actually enforces.
	//
	// oidc.RoleAdmin is the fallback for a caller with NO verified OIDC human
	// (admin token, local mode, OIDC unconfigured), which is byte-identical to
	// what this endpoint returned before: those callers have no session role,
	// and isOperator's own no-human arm already calls them admins.
	role := oidcRoleFromContext(r.Context())
	if oidcHumanFromContext(r.Context()) == "" {
		role = oidc.RoleAdmin
	}
	body := map[string]any{
		"principal": principal,
		"method":    method,
		"role":      role,
		"email":     oidcEmailFromContext(r.Context()),
		// The SAME predicate operatorOnly gates every admin route with
		// (isOperator, http.go) — never a second, driftable copy of the rule.
		// Kept for the console, which already consumes it: operator == role==admin.
		//
		// DELIBERATELY NOT widened to include security_admin. Its ~20 UI
		// consumers gate SUPER-admin writes (secrets, LLM credential, setup,
		// workspace writes); widening it here would hand a security admin
		// controls the server then refuses, which is the raw-403 discovery this
		// field exists to prevent. security_operator below is the additive
		// answer for the surfaces that DO move.
		"operator": s.isOperator(r.Context()),
		// The SAME predicate the securityOps group gates the security
		// governance routes with (isSecurityOperator, http.go) — the same
		// never-a-second-copy rule as "operator" directly above. True for an
		// admin too: the tiers overlap on this surface.
		"security_operator": s.isSecurityOperator(r.Context()),
	}
	// M3: the AddWorkspaceDialog root-constraint hint (member-role-desktop.md
	// §DECISIONS O1, ui-batch2-mock.md's "New wire this mock assumes"). null for
	// an operator (the dialog never renders the hint for one) and for a member
	// with no configured root either way (RootsFor's own empty-means-unavailable
	// contract). Presentational only — ValidateMemberMountSource, not this
	// value, is what actually enforces the boundary at bind time.
	//
	// Keyed on !isOperator, NOT on role == RoleMember: the constraint follows
	// the workspace-OWNERSHIP namespace (secretOwnerFromRequest, deliberately
	// still isOperator — see runs_policy.go), so a SECURITY ADMIN's workspaces
	// are owner-stamped like a member's and are clamped by the same roots. This
	// is byte-identical for the two pre-0.7 tiers; without it the third tier
	// would be silently clamped at bind time with no hint ever shown.
	body["member_local_dir_root"] = nil
	if !s.isOperator(r.Context()) {
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

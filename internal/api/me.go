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
	// The caller's USER DRIVE (0.7, migration 0054), nil-means-none — the same
	// convention member_local_dir_root above uses, so the console's "you have
	// none" state needs no sentinel value to special-case.
	//
	// DELIBERATELY NOT keyed on !isOperator, unlike the root hint directly
	// above, and the difference is the whole shape of the feature: a workspace
	// root bounds a MEMBER's authoring, while a drive is allocated to a
	// PRINCIPAL. A security admin's own drive resolves exactly like a member's,
	// and so does an SSO admin's. An operator with no per-human identity (admin
	// token, local mode) still gets nil, because the resolver itself refuses to
	// name a home for a caller who has no subject — not because of their tier.
	//
	// A nil map value marshals as JSON null, but a nil *meUserDrive stored in an
	// `any` does not — it becomes a typed nil that encoding/json still renders
	// as null. Assigned through the typed local so that stays true by
	// construction rather than by luck.
	body["user_drive"] = nil
	if ud := s.resolveMeUserDrive(r); ud != nil {
		body["user_drive"] = ud
	}
	// The DOOR, as a SIBLING of the allocation rather than a field inside it
	// (owner ruling at the mock gate): the profile name when this caller's
	// governance profile denies mounting a drive, "" when it does not.
	//
	// Two keys because there are FOUR states and one key can only carry three.
	// A member with no allocation and a shut door is a real state, and it is the
	// one where the obvious advice — "ask an admin for an allocation" — is
	// wrong. Always PRESENT (never omitted), so an older daemon's missing key is
	// distinguishable from an open door.
	body["user_drive_denied_by_profile"] = s.userDriveDeniedByProfile(r)
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

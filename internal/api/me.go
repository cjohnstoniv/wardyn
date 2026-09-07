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
	ud, unavailable := s.resolveMeUserDrive(r)
	if ud != nil {
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
	deniedBy, doorReason := s.userDriveDeniedByProfile(r)
	body["user_drive_denied_by_profile"] = deniedBy
	// A ceiling that could not be resolved makes the DOOR unknown, not open, and
	// an unknown door must not ship beside an allocation the card would then
	// offer: the checkbox and its writable/read-only sentence would promise a
	// mount the create path refuses at that same ceiling. So the allocation is
	// suppressed with it and the reason says which half is missing — the caller
	// is told "I cannot answer", never "yes" to a question nobody answered.
	if doorReason != "" {
		body["user_drive"] = nil
		// THE WIDENING YIELDS TO A DIFFERENT REMEDY, and only to that.
		//
		// PF-26's own motivating case is a TRUNCATED group snapshot, which fails
		// BOTH resolves: the drive resolver names it groups_snapshot_stale — the
		// same token the launch path's 403 carries — and the ceiling resolve
		// then fails for the identical reason. Overwriting unconditionally
		// replaced the specific token with the generic one, so /me told the
		// member "governance is unavailable" (wait, or ask an operator) while
		// POST /runs told them "sign in again (or re-mint your API token)". One
		// of those is theirs to act on, and it was the one being discarded.
		//
		// SCOPED TO THAT TOKEN rather than to "any narrower reason", because the
		// key exists for the CLIENT to pick a remedy: `unavailable` and
		// `governance_unavailable` both mean "the server could not answer — wait
		// or ask an operator", so preferring one over the other tells the member
		// nothing new and would only churn a pinned answer.
		// groups_snapshot_stale is the one token whose remedy is the member's
		// own, which is exactly why it must survive.
		//
		// The SUPPRESSION above stays unconditional: an unknown door must never
		// ship beside an allocation, whichever reason names it.
		//
		// FROM EITHER HALF (R1 F273's residue). The earlier form asked only
		// whether the DRIVE resolver had said groups_snapshot_stale, which is
		// true when the drive is allocated by group — and silently false on a
		// deployment that assigns governance by group while allocating drives
		// per user, where the drive resolves fine and the CEILING is the half
		// that could not answer. userDriveDeniedByProfile now returns that
		// reason instead of a bool, so the token survives from whichever
		// resolver actually met it.
		if unavailable == driveUnavailableGroups || doorReason == driveUnavailableGroups {
			unavailable = driveUnavailableGroups
		} else {
			unavailable = driveUnavailableGovernance
		}
	}
	// THE THIRD KEY, and the reason there are three rather than two. The door
	// needed its own key because four states do not fit in one; this is the same
	// argument one layer up. `user_drive: null` means "you have no allocation",
	// which is ADVICE ("ask an admin for one") — and it was also what a member
	// got when their group snapshot was stale, when their allocation could not
	// name a directory, and when the store was down. Three states whose remedies
	// differ, wearing the answer whose remedy is wrong for all of them.
	//
	// ALWAYS PRESENT, for the reason the door key is: an older daemon's missing
	// key has to be distinguishable from a daemon that answered "nothing is
	// wrong". "" is that answer; every other value is a token from the closed
	// set beside writeDriveError, which composes the sentence a member meets if
	// they launch anyway.
	body["user_drive_unavailable"] = unavailable
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

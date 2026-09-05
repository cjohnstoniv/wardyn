// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// derive.go holds the IDENTITY-DERIVATION seam: everything that turns an ID
// token's claims into what a Wardyn session carries about WHO this human is —
// their role (deriveRole, Config.RoleMap, the legacy operator allowlist) and
// their group snapshot (sessionGroups, the subject a capability grant matches).
//
// Split out of oidc.go when the group snapshot pushed that file past the
// 1000-line gate. The seam is real, not arithmetic: nothing here touches HTTP,
// cookies, or the OAuth2 exchange. It is pure claims-in, identity-out, which is
// also why it is the part of this package that is unit-testable without a
// signed token or a fake IdP.
package oidc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// ─── role derivation ─────────────────────────────────────────────────────────

// Wardyn roles a session can carry. See Session.Role / Config.RoleMap.
//
// RoleSecurityAdmin is the THIRD tier (0.7): a principal who governs the
// deployment's security posture — approvals, audit, permissions/capability
// grants, governance profiles — WITHOUT the super admin's reach into other
// people's runs, credentials or host configuration. It is NOT a rung on a
// monotone ladder below RoleAdmin: internal/api keeps two named predicates
// (isOperator = super only, isSecurityOperator = super OR security admin)
// precisely because security_admin ⊄ admin at the role-SNAPSHOT stamp sites
// (ssh keys, attach tickets), where RoleAdmin means the narrower, concrete
// thing "this credential reaches runs its holder does not own". A ladder would
// stamp admin on a security admin's SSH key — an interactive shell in every
// developer's sandbox. See internal/api/http.go's isSecurityOperator.
//
// MAPPED TIER ONLY. security_admin is reachable through the merged role map
// (chart WARDYN_OIDC_ROLE_MAP rows plus console-managed rows) and nothing
// else: there is deliberately no WARDYN_OIDC_SECURITY_EMAILS twin of the
// operator allowlist, and cmd/wardynd REFUSES it as WARDYN_OIDC_DEFAULT_ROLE
// (a fallthrough tier is exactly the accident that should never grant it).
const (
	RoleAdmin         = "admin"
	RoleSecurityAdmin = "security_admin"
	RoleMember        = "member"
)

// ValidRole reports whether s is a recognized role value. Used to validate
// WARDYN_OIDC_ROLE_MAP entries (ParseRoleMap), console role-mapping rows
// (mergeRoleMaps, internal/api's /access write boundary) and
// WARDYN_OIDC_DEFAULT_ROLE (cmd/wardynd, at boot) — all fail closed on a typo
// rather than letting a garbage role value silently reach a session cookie.
//
// Roles is the closed set, in rank order, and it is the ONE place the set is
// written down. ValidRole is implemented over it and the DDL parity guard
// (internal/db's TestClosedEnumChecksMatchConstants, role_mappings.role) reads
// it, so a fourth role cannot land on one side alone in EITHER direction. It
// used to be typed out a third time in that guard, which left it blind in the
// Go-widens-first direction — and that is the direction of the incident 0053
// documents: ValidRole accepted security_admin while 0051's CHECK still refused
// it, so POST /access/mappings passed validation and then 500'd at the database.
// The sibling user_drives enums already derive from types.DriveBackends and
// friends for exactly this reason; this is the surface that did not.
//
// A slice rather than a map so the order is stable for callers that render it;
// membership goes through ValidRole.
var Roles = []string{RoleAdmin, RoleSecurityAdmin, RoleMember}

// WARDYN_OIDC_DEFAULT_ROLE validates through validDefaultRole (cmd/wardynd),
// which is STRICTER than this: it additionally refuses RoleSecurityAdmin.
func ValidRole(s string) bool {
	for _, r := range Roles {
		if s == r {
			return true
		}
	}
	return false
}

// roleRank orders the role values for deriveRole's highest-wins fold:
//
//	"" (no match) 0  <  member 1  <  security_admin 2  <  admin 3
//
// It is the GENERALIZATION of the pre-0.7 rule "any admin match wins over a
// member match, no matter which claim produced it" — the same escalating-union
// semantics, now over three tiers instead of two booleans. An unrecognized
// value ranks 0, which is what keeps a garbage map entry from ever deciding an
// outcome (it also never becomes a Match: deriveRole skips it before the fold).
//
// Deliberately unexported and used ONLY here. It is an ordering over the
// DERIVATION inputs, not an authorization ladder: nothing in internal/api may
// ask "is my rank ≥ admin's" — see the two-predicate doctrine on
// RoleSecurityAdmin above, which exists precisely because that comparison is
// false at the role-snapshot stamp sites.
func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleSecurityAdmin:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

// ParseRoleMap parses WARDYN_OIDC_ROLE_MAP: a comma-separated list of
// "value=role" pairs, e.g.
// "Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin". value is matched
// case-insensitively against an ID token's roles/groups claims or its email
// (see deriveRole); role must satisfy ValidRole — RoleAdmin, RoleSecurityAdmin
// or RoleMember. This map is the ONLY way a session reaches RoleSecurityAdmin
// (see that constant's doc): the chart carries it the moment ValidRole accepts
// it, with no other boot knob to turn. Empty/blank input
// returns a nil map (role derivation disabled — Config.RoleMap's empty
// behavior) and no error; non-empty input that yields no usable entry (e.g.
// "," or a single malformed pair) is an error, never a silent nil — nil means
// "everyone is admin" (deriveRole), which must never be an accident.
func ParseRoleMap(csv string) (map[string]string, error) {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(csv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, cut := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !cut || k == "" {
			return nil, fmt.Errorf("malformed entry %q: want value=role", pair)
		}
		if !ValidRole(v) {
			return nil, fmt.Errorf("entry %q: invalid role %q (want %q, %q or %q)", pair, v, RoleAdmin, RoleSecurityAdmin, RoleMember)
		}
		// A non-ASCII key can NEVER match: deriveRole skips non-ASCII claim
		// values before lookup (ASCIIOnly, the fold-escalation guard), so
		// this would silently be a dead entry — worse, one that INVERTS
		// intent under WARDYN_OIDC_DEFAULT_ROLE=admin, where the operator
		// meant to name this value out for a lesser role but it can never
		// match and every such login instead gets the default.
		if !ASCIIOnly(k) {
			return nil, fmt.Errorf("entry %q: non-ASCII value can never match (matching is ASCII-only)", pair)
		}
		key := strings.ToLower(k)
		// A duplicate key silently let the LAST entry win — in the
		// escalating direction when an earlier entry mapped to member and a
		// later, easy-to-miss duplicate maps the same value to admin. An
		// operator reading the file top-to-bottom would expect the first
		// entry to hold; reject instead of guessing which one they meant.
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("entry %q: duplicate value %q (already mapped by an earlier entry)", pair, k)
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid entries in %q", csv)
	}
	return out, nil
}

// MatchSource names which config source produced a Match: a chart
// WARDYN_OIDC_ROLE_MAP entry or a merged-in console row (MatchSourceMapRow —
// mergeRoleMaps folds both into one map before deriveRole ever runs, so the
// two are indistinguishable by the time a Match is built), the
// WARDYN_OIDC_OPERATOR_EMAILS allowlist (MatchSourceOperatorAllowlist), or
// DefaultRole's no-match fallthrough (MatchSourceDefaultRole).
type MatchSource string

const (
	MatchSourceMapRow            MatchSource = "map_row"
	MatchSourceOperatorAllowlist MatchSource = "operator_allowlist"
	MatchSourceDefaultRole       MatchSource = "default_role"
)

// Match is one claim/email value that contributed to a derived role, with
// where it came from. deriveRole returns every match it found — not only the
// one that decided the outcome — so a caller (CallbackHandler's audit log,
// PreviewRole's console preview) can show WHY a role came out the way it did,
// not just which role won.
//
// The json tags are POST /access/preview's wire shape: internal/api returns
// this type directly rather than copying it into a twin that could drift.
type Match struct {
	// Value is the claim/email value that matched, exactly as the ID token
	// or email carried it — matching itself is case-insensitive (deriveRole
	// lowers before lookup) but Value is not lowered, so a preview can show
	// the human the literal claim that hit. Empty for a MatchSourceDefaultRole
	// match, which was not driven by any claim value at all.
	Value  string      `json:"value"`
	Role   string      `json:"role"`
	Source MatchSource `json:"source"`
}

// RoleMapping is one console-managed (Getting Started → People) value=>role
// row. Value is EXPECTED already canonical — trimmed, ASCII, lowercase — the
// API layer that owns writes to this store is responsible for canonicalizing
// a row before it ever reaches here. mergeRoleMaps no longer trusts that
// contract blindly, though: a non-canonical row (empty, whitespace,
// non-ASCII, mixed case) or an invalid Role is DROPPED AND LOGGED (via
// mergeRoleMaps' shadowed return), never stored verbatim — an empty/
// whitespace Value would otherwise match ANY empty/whitespace claim
// (ASCIIOnly("") is true) and an invalid Role would be a dead key that still
// changes deriveRole's arm. A canonical, valid row is still stored under
// Value exactly as given, the same way ParseRoleMap's chart keys are already
// lowercase by the time deriveRole looks one up.
type RoleMapping struct {
	Value string
	Role  string
}

// RoleMappingSource is the console's store-backed role-mapping source,
// read once per login by CallbackHandler and merged with the chart's
// WARDYN_OIDC_ROLE_MAP (Config.RoleMap) — see mergeRoleMaps. Config.RoleMappings
// wires it in; nil (the default) disables the console source entirely, the
// same additive-optional-Config-field rule SessionRevocations follows.
type RoleMappingSource interface {
	// ListRoleMappings returns every console-managed row. A non-nil error
	// DENIES the login in progress (CallbackHandler fails closed rather than
	// falling back to env-only, which could WIDEN access under
	// WARDYN_OIDC_DEFAULT_ROLE=admin) — this is not a best-effort read.
	ListRoleMappings(ctx context.Context) ([]RoleMapping, error)
}

// mergeRoleMaps builds the map deriveRole looks values up in from the chart's
// WARDYN_OIDC_ROLE_MAP (chart) plus the console's role-mapping rows (rows,
// Config.RoleMappings) — kept as a small pure function so the merge rules
// table-test cleanly, without a store or a signed ID token.
//
// chart always wins a duplicate key: rows is edited at runtime through the
// console, chart is a boot-time deployment decision, and the API layer that
// writes rows is expected to refuse CREATING a new collision — this arm only
// exists for a later helm upgrade that introduces one out from under an
// already-saved row, which is reported (not silently dropped) via the
// returned shadowed list.
//
// A row whose Value matches a legacyAdminEmails entry (case-insensitively) is
// shadowed the same way: the operator allowlist is a stronger, harder-to-edit
// admin source, and a console row racing it to a different role would be a
// confusing no-op regardless (emailInList / deriveRole's admin check already
// wins over anything the map says).
//
// shadowed ALSO carries a row rejected as non-canonical or invalid (see
// RoleMapping's doc) — mergeRoleMaps does not trust the write boundary's
// canonicalization contract blindly, since a row that violated it could
// otherwise become a dead key (never matched by deriveRole's lowered claim
// lookup) or, worse, an empty/whitespace key that matches ANY empty/
// whitespace claim. Every arm of shadowed means the same thing to a caller:
// this row contributed nothing to merged, and here is why (logged, not
// silent).
//
// A canonical, valid row is stored under Value VERBATIM, never re-lowered:
// the canonicalization contract lives at the write boundary, and by the time
// a row survives the rejection check below it is already lowercase.
func mergeRoleMaps(chart map[string]string, legacyAdminEmails []string, rows []RoleMapping) (merged map[string]string, shadowed []string) {
	merged = make(map[string]string, len(chart)+len(rows))
	for k, v := range chart {
		merged[k] = v
	}
	for _, row := range rows {
		// Enforce the canonical contract RoleMapping's doc documents rather
		// than trusting it: a non-canonical Value (empty, whitespace,
		// non-ASCII, or not already lowercase) or an invalid Role is dropped
		// here, not stored as a dead-or-dangerous key. An empty/whitespace
		// Value would match ANY empty/whitespace claim in deriveRole's loop
		// (ASCIIOnly("") is true) — an admin escalation if Role is admin. An
		// invalid Role can never resolve to RoleAdmin/RoleMember in
		// deriveRole's switch, but its mere presence still flips roleMap from
		// empty to non-empty, moving deriveRole from its no-role-map arm
		// (legacy allowlist alone) to its role-map-present arm — denying
		// every login that arm 1 would have allowed, with no DefaultRole set.
		if row.Value == "" || row.Value != strings.ToLower(strings.TrimSpace(row.Value)) || !ASCIIOnly(row.Value) || !ValidRole(row.Role) {
			shadowed = append(shadowed, row.Value)
			continue
		}
		// Explicit lowering here even though row.Value is already canonical
		// (the check above enforced it): keeps this collision test correct
		// and testable on its own terms, matching the operator-email arm
		// below which is EqualFold (case-insensitive) rather than relying on
		// chart keys happening to already be lowercase.
		if _, dup := merged[strings.ToLower(row.Value)]; dup {
			shadowed = append(shadowed, row.Value)
			continue
		}
		if emailInList(row.Value, legacyAdminEmails) {
			shadowed = append(shadowed, row.Value)
			continue
		}
		merged[row.Value] = row.Role
	}
	return merged, shadowed
}

// PreviewRole runs the SAME derivation CallbackHandler would for a login
// carrying roles/groups/email — including a real read of Config.RoleMappings
// when wired — so the console's People page can show an operator "who would
// this row make an admin" without waiting for that person to sign in. err is
// non-nil only when the store read itself failed (couldn't-check, distinct
// from ok=false's "checked, and nothing matched"); it never falls back to an
// env-only preview, for the same fail-closed reason CallbackHandler doesn't.
//
// roles/groups/email need no pre-normalization from the caller: deriveRole
// does its own lowering at lookup time, and PreviewRole reuses it rather than
// duplicating that rule here.
func (a *Authenticator) PreviewRole(ctx context.Context, roles, groups []string, email string) (role string, matched []Match, ok bool, err error) {
	roleMap := a.cfg.RoleMap
	if a.cfg.RoleMappings != nil {
		rows, lerr := a.cfg.RoleMappings.ListRoleMappings(ctx)
		if lerr != nil {
			return "", nil, false, lerr
		}
		roleMap, _ = mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	}
	role, matched, ok = deriveRole(roles, groups, email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole)
	return role, matched, ok, nil
}

// ChartRoleMap returns a COPY of the boot-time WARDYN_OIDC_ROLE_MAP
// (Config.RoleMap) entries — the console's People page uses it to show which
// rows are chart-owned (and therefore always win a collision, see
// mergeRoleMaps) versus console-managed. A copy, not the live map, so a
// handler can never mutate boot config through the returned value.
func (a *Authenticator) ChartRoleMap() map[string]string {
	out := make(map[string]string, len(a.cfg.RoleMap))
	for k, v := range a.cfg.RoleMap {
		out[k] = v
	}
	return out
}

// DefaultRole returns Config.DefaultRole — the role a login falls through to
// when the merged map (chart or console) is non-empty but nothing in it
// matched. Empty means "deny", the same as an unset WARDYN_OIDC_DEFAULT_ROLE.
func (a *Authenticator) DefaultRole() string {
	return a.cfg.DefaultRole
}

// HasOperatorEmails reports whether Config.LegacyAdminEmails
// (WARDYN_OIDC_OPERATOR_EMAILS) is non-empty — the console's People page
// needs this to explain why a row's value already resolves to admin
// independent of anything it manages (see mergeRoleMaps' shadow rule).
func (a *Authenticator) HasOperatorEmails() bool {
	return len(a.cfg.LegacyAdminEmails) > 0
}

// HasEmailDomains reports whether Config.AllowedEmailDomains
// (WARDYN_OIDC_EMAIL_DOMAINS) is non-empty — the console's People page
// EMAIL_KEY badge copy depends on this: without a domains list configured,
// email_verified is not enforced (see oidc.go's boot/callback warnings), a
// distinction the response otherwise has no way to express.
func (a *Authenticator) HasEmailDomains() bool {
	return len(a.cfg.AllowedEmailDomains) > 0
}

// Issuer returns the PUBLIC OIDC issuer URL (Config.IssuerURL) — the console's
// People page derives a human-facing provider name from it (e.g. an Entra
// tenant issuer -> "Microsoft Entra ID") so the SSO chip names WHERE sign-in
// comes from, not just THAT it is SSO.
func (a *Authenticator) Issuer() string {
	return a.cfg.IssuerURL
}

// MergedMapEmpty reports whether the ACTUAL merged role map (chart plus the
// given console rows, applying mergeRoleMaps' own collision/shadow rules) is
// empty — the console's People page needs this instead of a raw row count
// (len(chart)==0 && len(rows)==0), which diverges from the real map whenever
// a stored row is shadowed by the chart or the operator allowlist and
// therefore contributes nothing to what deriveRole actually looks values up
// in.
func (a *Authenticator) MergedMapEmpty(rows []RoleMapping) bool {
	merged, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	return len(merged) == 0
}

// IsOperatorEmail reports whether v case-insensitively matches an entry on
// Config.LegacyAdminEmails (WARDYN_OIDC_OPERATOR_EMAILS) — the console's
// People page write boundary uses this to name the SAME shadow cause
// mergeRoleMaps' operator-allowlist arm already enforces at login time
// (collision_operator_allowlist, not a second, possibly-drifting rule): a
// row a write would add is refused up front with the reason it would be
// silently shadowed anyway, rather than accepted and only discovered inert
// the next time someone signs in. Delegates to emailInList, the exact match
// deriveRole's own allowlist check uses.
func (a *Authenticator) IsOperatorEmail(v string) bool {
	return emailInList(v, a.cfg.LegacyAdminEmails)
}

// PreviewRoleAgainst is PreviewRole's PURE twin: it runs the identical
// mergeRoleMaps + deriveRole derivation against a CALLER-SUPPLIED candidate
// rows slice instead of a real Config.RoleMappings read, so the console's
// write-boundary lockout guard (POST/DELETE /access/mappings) can ask "would
// the acting admin still derive admin AFTER this proposed write" by
// constructing the candidate rows slice itself (the current store list, with
// the one row added/removed) — without a second store round trip, and
// without reimplementing merge/derive precedence at the API layer where it
// could drift from what a REAL login would actually decide.
//
// No store read, no error return: rows is exactly what merged, this is
// resolution over data already in hand. Compare PreviewRole, which reads
// Config.RoleMappings itself and can fail on that read (err) — this method
// never does, because it never reads anything.
func (a *Authenticator) PreviewRoleAgainst(rows []RoleMapping, roles, groups []string, email string) (role string, ok bool) {
	roleMap, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	role, _, ok = deriveRole(roles, groups, email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole)
	return role, ok
}

// deriveRole computes the Wardyn role for a signed-in human from the ID
// token's roles/groups claims, their email, and the derivation config
// (Config.RoleMap / Config.LegacyAdminEmails / Config.DefaultRole). ok is
// false only when roleMap is non-empty, nothing matched, and defaultRole is
// empty — the caller (CallbackHandler) must then deny the login. matches
// carries provenance for every value that contributed (see Match) — CONSUMED
// today only for logging/preview, never for the role decision itself, which
// stays exactly the precedence below.
//
// Precedence:
//  1. An empty roleMap disables claim-based derivation: the role comes from the
//     legacy operator allowlist alone — an email on legacyAdminEmails is
//     RoleAdmin, anyone else RoleMember (main's operator/viewer split, preserved
//     with no role map). With NEITHER a role map nor an allowlist every human is
//     RoleAdmin (true pre-0.5) — so adopting WARDYN_OIDC_ROLE_MAP is opt-in and
//     upgrade-safe, and so is running on only WARDYN_OIDC_OPERATOR_EMAILS.
//  2. Otherwise, build the case-insensitive union of rolesClaim, groupsClaim,
//     and email, look each value up in roleMap, and keep the HIGHEST-RANKING
//     match (roleRank: member < security_admin < admin), no matter which claim
//     produced it. An email on legacyAdminEmails (WARDYN_OIDC_OPERATOR_EMAILS)
//     counts as an additional top-rank RoleAdmin match — it still wins over
//     any map entry the same email also hits, security_admin included.
//  3. If nothing matched at all: defaultRole if set, else deny.
//
// Arm 1 is UNTOUCHED by the security_admin tier and must stay that way: a
// deployment with no role map has no way to express the third tier at all, so
// nothing changes for it (the upgrade-safe absent-row doctrine). The allowlist
// is an ADMIN allowlist; there is no security-admin twin of it.
func deriveRole(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string) (role string, matches []Match, ok bool) {
	if len(roleMap) == 0 {
		// No role map: claim-based derivation is disabled, but the legacy
		// operator allowlist still splits admin from member. WARDYN_OIDC_OPERATOR_EMAILS
		// is the mandatory-minimum SSO config (validateOperatorPosture) and a role
		// map is opt-in on top, so honoring the list here is what keeps main's
		// operator/viewer split working after an upgrade — without this, a 0.4.5
		// deployment that set only the allowlist would silently promote every
		// signed-in human to admin. Only when NEITHER is set does every human
		// default to admin (true pre-0.5, before the operator allowlist existed).
		if len(legacyAdminEmails) == 0 {
			return RoleAdmin, nil, true
		}
		if emailInList(email, legacyAdminEmails) {
			return RoleAdmin, []Match{{Value: email, Role: RoleAdmin, Source: MatchSourceOperatorAllowlist}}, true
		}
		return RoleMember, nil, true
	}
	// best is the highest-ranking match found so far ("" = nothing yet). The
	// fold replaced a pair of booleans when the third tier landed: two bools
	// already encoded "admin beats member", and a third would have made the
	// resolution switch a hand-ordered cascade that a FOURTH tier silently gets
	// wrong. One ordering (roleRank), one comparison, one place to change.
	best := ""
	if emailInList(email, legacyAdminEmails) {
		// Top rank by construction, so no later map row can outrank it — the
		// pre-0.7 "the allowlist wins even over a member entry the same email
		// hits" rule, unchanged and now covering security_admin entries too.
		best = RoleAdmin
		matches = append(matches, Match{Value: email, Role: RoleAdmin, Source: MatchSourceOperatorAllowlist})
	}
	values := make([]string, 0, len(rolesClaim)+len(groupsClaim)+1)
	values = append(values, rolesClaim...)
	values = append(values, groupsClaim...)
	if email != "" {
		values = append(values, email)
	}
	// seenMapRow dedupes MatchSourceMapRow entries keyed on the lowered claim
	// value: the same value can legitimately appear in BOTH rolesClaim and
	// groupsClaim (or repeated within one), which without this would append a
	// duplicate Match for what a human reads as the same thing matching twice
	// — PreviewRole's console preview renders this list verbatim. Keyed only
	// on value, not source, because every match appended in THIS loop shares
	// MatchSourceMapRow; it never dedupes against the operator-allowlist
	// match above, which is a different source and stays even when the same
	// email also hits a map row — that double entry is correct, distinct
	// provenance (an email on both the allowlist and a map row), not a
	// repeat of the same fact.
	seenMapRow := make(map[string]bool, len(values))
	for _, v := range values {
		if !ASCIIOnly(v) {
			continue // fail closed: see ASCIIOnly
		}
		key := strings.ToLower(strings.TrimSpace(v))
		mapped := roleMap[key]
		// ValidRole, not "mapped != \"\"": an absent key and an unrecognized
		// value must behave identically — neither contributes to the fold, and
		// neither becomes a Match. This is the pre-0.7 switch's exhaustive-case
		// behavior stated once instead of enumerated per tier (a value that
		// survived ParseRoleMap/mergeRoleMaps is already valid; this is the
		// defense-in-depth arm for a row that reached the map some other way).
		if !ValidRole(mapped) {
			continue
		}
		if roleRank(mapped) > roleRank(best) {
			best = mapped
		}
		// Provenance records EVERY contributing value at ITS OWN role, not the
		// winning one — PreviewRole's console preview shows the human why the
		// outcome came out this way, which needs the losing matches too.
		if !seenMapRow[key] {
			seenMapRow[key] = true
			matches = append(matches, Match{Value: v, Role: mapped, Source: MatchSourceMapRow})
		}
	}
	switch {
	case best != "":
		return best, matches, true
	case defaultRole != "":
		return defaultRole, []Match{{Role: defaultRole, Source: MatchSourceDefaultRole}}, true
	default:
		return "", nil, false
	}
}

// maxSessionGroupsBytes bounds what Session.Groups may contribute to the JSON
// payload — NOT to the cookie, which is a bigger number by a factor that is
// easy to forget:
//
//	cookie ≈ len("wardyn_session=") + ceil(4/3 · payload) + 1 + 44
//
// The base64 in encodeSession expands the payload by a THIRD before the dot
// and the 32-byte HMAC (44 base64 chars) are appended. A browser drops a
// cookie over ~4096 bytes ENTIRELY — no error, no truncation, just a human who
// cannot stay signed in — so budgeting the payload as if it were the cookie
// overshoots by ~1100 bytes and breaks login for exactly the group-heavy
// directory this field exists to serve.
//
// 2048 for groups plus a few hundred for sub/email/role/expiry lands the whole
// cookie near 3100, with headroom for an unusually long sub or email. That
// still carries ~100 typical group names; a human in more groups than that is
// one where per-group grants were never the workable answer anyway (grant the
// user directly, or prefer Entra App Roles, which arrive on the much smaller
// "roles" claim). TestSessionGroupsCapKeepsTheCookieUsable measures the real
// encoded cookie rather than trusting this arithmetic.
const maxSessionGroupsBytes = 2048

// sessionGroups normalizes the ID token's roles+groups claims into the group
// identity a `group`-subject capability grant matches against: the union of
// both claims, trimmed, lowercased, printable-ASCII only, deduped, sorted, and
// truncated to maxSessionGroupsBytes.
//
// NEVER returns nil — an empty result is the empty non-nil slice, because nil
// is reserved for "this cookie predates 0.6" (see Session.Groups).
//
// Printable-ASCII only (CanonicalGroupSubject, which internal/api's group-subject
// write boundaries share so the two surfaces cannot drift), for the same reason
// deriveRole's loop skips non-ASCII claim values: a grant subject is an
// operator-authored ASCII string, and Unicode case folding lets a crafted claim
// fold ONTO one — so the guard runs on the RAW value, BEFORE the fold. Control
// characters are dropped with the rest — they cannot appear in a real group
// name, and excluding them keeps the byte budget below exact.
//
// SORTED, then truncated FROM THE END: the drop must be deterministic, so the
// same human with the same claims loses the same groups on every login. An
// admin debugging "why does this grant not apply" gets a stable answer instead
// of a coin flip. (Ordering is lexical, so a truncated human loses their
// alphabetically-last groups — arbitrary, but arbitrary and REPEATABLE.)
//
// The second return says whether the snapshot is PARTIAL, and it is not a
// diagnostic — it is an authorization input (PF-26). A group-subject governance
// assignment a member's ceiling depends on can fall off this cap, and the
// resolver would then hand them the DEPLOYMENT ceiling with no refusal, no
// audit and nothing to notice: the restrictive profile simply evaporates. So
// the bit rides the session to internal/api's ceiling resolver, which treats a
// truncated snapshot exactly as it treats a missing one.
//
// claimNames is the ID token's `_claim_names` object (OIDC Core distributed
// claims; nil when the token carries none), and it is the SECOND way this
// snapshot can be partial — the IdP-side one, which no byte cap here can see.
// Entra ID stops emitting `groups` (or `roles`) entirely once the human is in
// more than the token limit and sends a `_claim_names`/`_claim_sources`
// pointer to Microsoft Graph instead. The claim then decodes to nil, which is
// byte-for-byte the same as "asked, there were none" — so without this input
// an overage login reads COMPLETE-AND-EMPTY and evaporates exactly the walled
// member's group-tier assignment that the cap branch below was closed for.
// Wardyn does not dereference the pointer (that is a Graph call with its own
// credential and egress, at login latency); it fails closed instead and marks
// the snapshot partial, which is what "unanswerable" already means downstream.
//
// A value CanonicalGroupSubject refuses is the THIRD way, and it is the one
// that used to be invisible: the drop happens before uniq is built, so the
// len(out) < len(uniq) reading below cannot see it and the snapshot reported
// COMPLETE while a group the human really holds was missing. A directory that
// names groups in a non-English locale ("Entwickler-Büro") hits this on an
// ordinary login. It stamps the same bit — see unrepresentable below.
func sessionGroups(rolesClaim, groupsClaim []string, claimNames map[string]any) (groups []string, truncated bool) {
	seen := make(map[string]bool, len(rolesClaim)+len(groupsClaim))
	uniq := make([]string, 0, len(rolesClaim)+len(groupsClaim))
	// unrepresentable counts claim values this snapshot CANNOT carry — the
	// THIRD way it is partial, and the one no byte cap and no `_claim_names`
	// pointer can see. A group the human really holds that CanonicalGroupSubject
	// refuses never reaches uniq, so `len(out) < len(uniq)` cannot see the loss
	// either: the snapshot would read COMPLETE while a real group is missing
	// from it, and a group-subject DENY or a group-tier ceiling written against
	// that group would simply not match — no refusal, no audit, no warning.
	// That is the evaporation this bit exists to prevent, so a drop here stamps
	// it exactly as the cap and the overage do.
	unrepresentable := 0
	for _, v := range slices.Concat(rolesClaim, groupsClaim) {
		if strings.TrimSpace(v) == "" {
			continue // names no group; nothing was lost
		}
		g, ok := CanonicalGroupSubject(v)
		if !ok {
			unrepresentable++
			continue
		}
		if seen[g] {
			continue
		}
		seen[g] = true
		uniq = append(uniq, g)
	}
	slices.Sort(uniq)

	out := make([]string, 0, len(uniq))
	used := 0
	for _, g := range uniq {
		// Exact JSON cost: two quotes, a comma, and one extra byte for each
		// character the encoder escapes. Control characters (the only other
		// escapes) are already filtered out above.
		cost := len(g) + 3 + strings.Count(g, `"`) + strings.Count(g, `\`)
		if used+cost > maxSessionGroupsBytes {
			break
		}
		used += cost
		out = append(out, g)
	}
	// Either kind of partial snapshot — dropped at the byte cap here, or never
	// sent by the IdP (overage) — stamps the same bit.
	if claimsOverage(claimNames) {
		return out, true
	}
	return out, len(out) < len(uniq) || unrepresentable > 0
}

// CanonicalGroupSubject canonicalizes an operator-authored group name into the
// EXACT string a session snapshot carries for a claim of that name, or reports
// ok=false when NO snapshot can ever carry it (empty/whitespace-only, or any
// character outside printable ASCII).
//
// It is exported because sessionGroups is the MATCH surface and internal/api's
// three group-subject WRITE surfaces — a capability grant, a governance
// assignment, a user-drive grant — must refuse exactly what this one drops.
// Every one of those subject columns is matched by exact string equality
// against this snapshot, so a subject this function refuses is a row that can
// never match anyone: a DENY that protects nothing, or a group tier that
// counts as "assigned" while resolving to no profile. One implementation, so
// the two surfaces cannot drift about what a group name is.
//
// THE ASCII GUARD RUNS ON THE RAW VALUE, BEFORE THE FOLD, and the order is the
// security property — the same ordering ParseRoleMap and deriveRole's own
// lookup loop already use, for the same reason. strings.ToLower does Unicode case
// mapping: KELVIN SIGN U+212A folds to ASCII 'k' and U+0130 folds to 'i', so
// guarding the LOWERED value lets a crafted claim "Kubernetes-admins" (U+212A)
// fold ONTO the operator-authored ASCII group "kubernetes-admins" and enter the
// snapshot as it — inheriting every grant and every governance profile bound to
// the real group. Fold first, guard second, and the guard is decorative.
func CanonicalGroupSubject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !printableASCII(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// claimsOverage reports whether the ID token's `_claim_names` says the IdP
// OMITTED a claim this package derives identity from, rather than sending an
// empty one. Entra ID stops emitting `groups` (or `roles`) altogether once the
// human is in more groups than the token limit — 200 for a JWT — and sends an
// OIDC Core distributed-claim pointer (`_claim_names`/`_claim_sources`) to
// Microsoft Graph in its place. The claim then decodes to nil, which is
// byte-for-byte "asked, there were none", so this marker is the only thing that
// tells the two apart. Wardyn does not dereference the pointer (a Graph call
// with its own credential and egress, at login latency) — it fails closed.
//
// BOTH claims are checked, because both feed both derivations: sessionGroups
// unions them into one group identity, and deriveRole looks both up in the role
// map. An App Roles overage hides a `roles`-derived identity just as completely
// as a `groups` one.
//
// One function, two callers — sessionGroups' truncation bit and
// overageWidensRole — because "was this token answerable" must have exactly one
// answer. They diverged once: the snapshot half was made overage-aware and the
// role half was not, and on WARDYN_OIDC_DEFAULT_ROLE=admin that silently
// promoted a group-mapped member to super admin.
func claimsOverage(claimNames map[string]any) bool {
	for _, claim := range []string{"groups", "roles"} {
		if _, overage := claimNames[claim]; overage {
			return true
		}
	}
	return false
}

// overageWidensRole reports whether an IdP claim overage turned deriveRole's
// "nothing matched" into a WIDENING default — the login CallbackHandler must
// deny rather than serve.
//
// deriveRole's arm 3 falls through to defaultRole when no claim value and no
// allowlist entry matched. On an overage that "nothing matched" is not a fact,
// it is an absence of evidence: the IdP declined to send the very claim the
// role map is keyed on. Serving the default then hands the human a role NO
// configured rule granted them, decided by a directory change nobody in Wardyn
// made or can see.
//
// The check is deliberately narrow, so it costs nothing in the ordinary
// posture:
//
//   - A real MATCH is served as-is. Hiding a claim can only REMOVE matches from
//     a highest-wins fold, so an overage can only ever narrow a matched role.
//     Narrowing is the safe direction and needs no denial.
//   - A default that cannot widen is served too. defaultRole=member is the
//     narrowest tier there is, so no hidden claim could have produced less; a
//     human in 200+ groups still signs in exactly as before. Keyed on roleRank
//     rather than the literal "admin" so a fourth tier inherits the rule.
//   - Only a fallthrough to a default that OUTRANKS the narrowest tier is
//     refused — today exactly WARDYN_OIDC_DEFAULT_ROLE=admin, the "everyone not
//     specifically walled is an admin" posture, where the hidden claim is
//     precisely the one that would have walled them.
//
// Arm 1 (no role map at all) is untouched: it derives from the allowlist and
// the email, never from a claim, and its Matches are never MatchSourceDefaultRole.
func overageWidensRole(claimNames map[string]any, role string, matches []Match) bool {
	return unanswerableWidensRole(claimsOverage(claimNames), role, matches)
}

// unanswerableWidensRole is the rule overageWidensRole documents with its CAUSE
// factored out, because an IdP overage is not the only way the claim a role map
// is keyed on goes unanswered.
//
// The second way is a claim the IdP DID send, in a shape this build cannot
// decode. CallbackHandler decodes `roles` and `groups` tolerantly, one struct
// each, so a scalar string ("eng-team" rather than ["eng-team"]) cannot fail the
// whole login — a real IdP sends that shape and a fatal decode there is a 100%
// login outage. But the claim then contributes nothing, and arm 3's fallthrough
// to defaultRole is once again "nothing matched" standing in for "nobody could
// read it". Serving a WIDENING default on that input is the same escalation
// reached by a different road, so it takes the same refusal.
//
// unanswerable is the caller's answer to "was the claim readable at all". The
// two conjuncts after it are unchanged and are what keep the rule narrow (see
// overageWidensRole): a real match is authoritative, and a default that cannot
// outrank the narrowest tier could not have been widened by anything the
// unreadable claim would have said.
func unanswerableWidensRole(unanswerable bool, role string, matches []Match) bool {
	if !unanswerable {
		return false
	}
	if !slices.ContainsFunc(matches, func(m Match) bool { return m.Source == MatchSourceDefaultRole }) {
		return false
	}
	return roleRank(role) > roleRank(RoleMember)
}

// printableASCII reports whether every rune of s is a printable ASCII
// character (U+0020..U+007E). Stricter than ASCIIOnly — see sessionGroups.
func printableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}

// emailInList reports whether email case-insensitively matches an entry in
// list (mirrors the operator-allowlist match in internal/api's isOperator).
// email is trimmed here too (each list entry is trimmed below, at the point
// of comparison) — without trimming email, a padded ID-token claim would
// silently miss legacyAdminEmails while still matching the role map, whose
// own lookup (deriveRole's loop) already trims its values.
func emailInList(email string, list []string) bool {
	email = strings.TrimSpace(email)
	if email == "" || !ASCIIOnly(email) {
		return false
	}
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), email) {
			return true
		}
	}
	return false
}

// ASCIIOnly reports whether s contains no rune above ASCII. It is exported for
// internal/api, whose console-managed role-map writes must refuse exactly what
// this package refuses at login — one implementation, so the write surface and
// the match surface can never disagree about what "ASCII" means.
//
// Case-insensitive
// matching (ToLower/EqualFold) does Unicode case folding, under which e.g.
// "roſs" (U+017F) or a KELVIN SIGN "k" (U+212A) MATCHES an ASCII string — the
// escalating direction (same fold-escalation guard as internal/api's
// isOperator, applied here to the RoleMap / LegacyAdminEmails match: both are
// operator-authored ASCII allowlists — WARDYN_OIDC_ROLE_MAP and
// WARDYN_OIDC_OPERATOR_EMAILS — that a crafted non-ASCII claim must never
// fold onto).
func ASCIIOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// claimNamesKeys returns the distributed-claim names present in a token's
// `_claim_names`, sorted, for the one log line an operator debugging a
// claims_overage denial has to go on. Keys only — the VALUES are
// `_claim_sources` references into the IdP's own endpoints and say nothing this
// log needs.
func claimNamesKeys(claimNames map[string]any) []string {
	keys := make([]string, 0, len(claimNames))
	for k := range claimNames {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

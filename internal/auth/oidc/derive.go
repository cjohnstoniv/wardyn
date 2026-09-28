// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// derive.go holds the IDENTITY-DERIVATION seam: everything that turns an ID
// token's claims into WHO this human is (deriveRole, Config.RoleMap, the
// legacy operator allowlist) and their group snapshot (sessionGroups). Pure
// claims-in, identity-out — nothing here touches HTTP, cookies, or the
// OAuth2 exchange, which is also why it is unit-testable without a signed
// token or a fake IdP.
package oidc

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── role derivation ─────────────────────────────────────────────────────────

// Wardyn roles a session can carry. See Session.Role / Config.RoleMap.
//
// RoleSecurityAdmin is the THIRD tier (0.7): governs security posture
// (approvals, audit, permission grants) WITHOUT the super admin's reach into
// other people's runs or credentials. NOT a rung below RoleAdmin: internal/api
// keeps two predicates (isOperator = super only, isSecurityOperator = super OR
// security admin) since security_admin ⊄ admin at the role-SNAPSHOT stamp
// sites (ssh keys, attach tickets).
//
// MAPPED TIER ONLY: reachable only through the merged role map, never an
// email-allowlist twin, and cmd/wardynd REFUSES it as a fallthrough default.
const (
	RoleAdmin         = "admin"
	RoleSecurityAdmin = "security_admin"
	RoleUser          = "user"
)

// LegacyRoleMember is the pre-0.8 name of RoleUser. WARDYN_OIDC_ROLE_MAP
// values and WARDYN_OIDC_DEFAULT_ROLE still accept it, with a boot WARN, as
// the user tier on the built-in "standard" type. Never stored, never a
// session role. ponytail: removed in 0.9.
const LegacyRoleMember = "member"

// LegacyRoleMemberWarning is the boot WARN for one aliased "member" value.
// variable names the setting; entry is the role-map pair as written, or ""
// for WARDYN_OIDC_DEFAULT_ROLE, which only the chart sets.
func LegacyRoleMemberWarning(variable, entry string) string {
	subject, fix := variable+"=member", "change it in your chart"
	if entry != "" {
		subject, fix = "Entry "+strconv.Quote(entry), "remap it in Getting started -> People, or in your chart"
	}
	return variable + `: "member" is no longer a role. ` + subject +
		` maps to the built-in user type "standard" (Standard user) until you ` + fix + `. The alias is removed in 0.9.`
}

// ValidRole reports whether s is a recognized role value; all callers fail
// closed on a typo. Roles is the closed set, in rank order, pinned against
// the DDL parity guard (TestClosedEnumChecksMatchConstants).
var Roles = []string{RoleAdmin, RoleSecurityAdmin, RoleUser}

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

// roleRank orders role values for deriveRole's highest-wins fold ("" 0 <
// member 1 < security_admin 2 < admin 3); unrecognized ranks 0. Unexported,
// used ONLY here — an ordering over DERIVATION inputs, not an authorization
// ladder (see RoleSecurityAdmin's doc).
func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleSecurityAdmin:
		return 2
	case RoleUser:
		return 1
	default:
		return 0
	}
}

// ParseRoleMap parses WARDYN_OIDC_ROLE_MAP: comma-separated "value=role"
// pairs (e.g. "Wardyn.Admin=admin,eng-team=user"), matched case-insensitively
// against an ID token's roles/groups/email. Empty/blank input returns a nil
// map (derivation disabled) and no error; non-empty input yielding no usable
// entry is an error, never a silent nil — nil means "everyone is admin".
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
		if v == LegacyRoleMember {
			slog.Warn(LegacyRoleMemberWarning("WARDYN_OIDC_ROLE_MAP", k+"="+v))
			v = RoleUser
		}
		if !ValidMappingTarget(v) {
			return nil, fmt.Errorf("entry %q: invalid role %q (want %q, %q, %q or a user type id)", pair, v, RoleAdmin, RoleSecurityAdmin, RoleUser)
		}
		// A non-ASCII key can never match; under DEFAULT_ROLE=admin that would
		// silently invert intent, granting the default instead of the lesser
		// role the operator meant to name.
		if !ASCIIOnly(k) {
			return nil, fmt.Errorf("entry %q: non-ASCII value can never match (matching is ASCII-only)", pair)
		}
		key := strings.ToLower(k)
		// A duplicate key would silently let the LAST entry win; reject rather
		// than guess which one the operator meant.
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

// MatchSource names which config source produced a Match: a chart or
// console role-map row (MatchSourceMapRow), the operator-email allowlist
// (MatchSourceOperatorAllowlist), or DefaultRole's fallthrough
// (MatchSourceDefaultRole).
type MatchSource string

const (
	MatchSourceMapRow            MatchSource = "map_row"
	MatchSourceOperatorAllowlist MatchSource = "operator_allowlist"
	MatchSourceDefaultRole       MatchSource = "default_role"
)

// Match is one claim/email value that contributed to a derived role, with
// where it came from. deriveRole returns every match found, not only the
// winner, so a caller can show WHY. The json tags are POST /access/preview's
// wire shape.
type Match struct {
	// Value is the claim/email value that matched, exactly as carried, not
	// lowered. Empty for a MatchSourceDefaultRole match.
	Value string `json:"value"`
	// Role is the tier this value maps to; UserType the type, set only when
	// Role is the user tier.
	Role     string      `json:"role"`
	UserType string      `json:"user_type,omitempty"`
	Source   MatchSource `json:"source"`
}

// RoleMapping is one console-managed (Getting Started → People) value=>role
// row. Value is EXPECTED already canonical (trimmed, ASCII, lowercase); the
// API write boundary owns that. mergeRoleMaps does not trust it blindly,
// though — a non-canonical row or invalid Role is DROPPED AND LOGGED rather
// than stored verbatim. UserType names the row's type when Role is
// RoleUser; a tier row carrying a type is non-canonical, dropped too.
type RoleMapping struct {
	Value    string
	Role     string
	UserType string
}

// target is the role-map value a row contributes: its tier, or its type for a
// user row. ok is false for a row mergeRoleMaps must drop.
func (m RoleMapping) target() (string, bool) {
	switch {
	case !ValidRole(m.Role):
		return "", false
	case m.Role != RoleUser:
		return m.Role, m.UserType == ""
	case m.UserType == "":
		return RoleUser, true
	}
	// A type id only: "admin" here must never turn a user row into an admin.
	return m.UserType, UserTypeIDWellFormed(m.UserType) && !UserTypeIDReserved(m.UserType)
}

// RoleMappingSource is the console's store-backed role-mapping source, read
// once per login and merged with the chart's WARDYN_OIDC_ROLE_MAP (see
// mergeRoleMaps). nil (the default) disables the console source entirely.
type RoleMappingSource interface {
	// ListRoleMappings: a non-nil error DENIES the login in progress rather
	// than falling back to env-only, which could WIDEN access under
	// WARDYN_OIDC_DEFAULT_ROLE=admin.
	ListRoleMappings(ctx context.Context) ([]RoleMapping, error)
}

// mergeRoleMaps builds the map deriveRole looks values up in, from the
// chart's WARDYN_OIDC_ROLE_MAP plus the console's role-mapping rows — a small
// pure function so the merge rules table-test without a store or a signed ID token.
//
// chart always wins a duplicate key: rows is edited at runtime, chart is a
// boot-time decision. A row matching a legacyAdminEmails entry is shadowed
// too — that allowlist is a stronger, harder-to-edit admin source, and a
// console row racing it would be a confusing no-op regardless.
//
// shadowed ALSO carries a row rejected as non-canonical or invalid (it could
// otherwise become a dead key, or one matching ANY empty/whitespace claim);
// every arm means the row contributed nothing to merged, logged not silent.
func mergeRoleMaps(chart map[string]string, legacyAdminEmails []string, rows []RoleMapping) (merged map[string]string, shadowed []string) {
	merged = make(map[string]string, len(chart)+len(rows))
	for k, v := range chart {
		merged[k] = v
	}
	for _, row := range rows {
		// Enforce the canonical contract rather than trusting it: a
		// non-canonical Value or invalid Role is dropped here, not stored as
		// a dead-or-dangerous key — an empty/whitespace Value would match ANY
		// such claim in deriveRole's loop, an admin escalation if Role is admin.
		target, validTarget := row.target()
		if row.Value == "" || row.Value != strings.ToLower(strings.TrimSpace(row.Value)) || !ASCIIOnly(row.Value) || !validTarget {
			shadowed = append(shadowed, row.Value)
			continue
		}
		if _, dup := merged[strings.ToLower(row.Value)]; dup {
			shadowed = append(shadowed, row.Value)
			continue
		}
		if emailInList(row.Value, legacyAdminEmails) {
			shadowed = append(shadowed, row.Value)
			continue
		}
		merged[row.Value] = target
	}
	return merged, shadowed
}

// PreviewRole runs the SAME derivation CallbackHandler would, including a
// real read of Config.RoleMappings when wired, so the console's People page
// can show "who would this row make an admin" before that person signs in.
// err is non-nil only when the store read failed; it never falls back to
// an env-only preview, the same fail-closed reason CallbackHandler doesn't.
func (a *Authenticator) PreviewRole(ctx context.Context, roles, groups []string, email string) (Derivation, error) {
	roleMap := a.cfg.RoleMap
	if a.cfg.RoleMappings != nil {
		rows, lerr := a.cfg.RoleMappings.ListRoleMappings(ctx)
		if lerr != nil {
			return Derivation{}, lerr
		}
		roleMap, _ = mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	}
	userTypes, err := a.loadUserTypes(ctx)
	if err != nil {
		return Derivation{}, err
	}
	return deriveRole(roles, groups, email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole, userTypes), nil
}

// ChartRoleMap returns a COPY of the boot-time WARDYN_OIDC_ROLE_MAP entries,
// not the live map, so a handler can never mutate boot config through it.
func (a *Authenticator) ChartRoleMap() map[string]string {
	out := make(map[string]string, len(a.cfg.RoleMap))
	for k, v := range a.cfg.RoleMap {
		out[k] = v
	}
	return out
}

// DefaultRole returns Config.DefaultRole — the role a login falls through to
// when the merged map is non-empty but nothing in it matched. Empty means
// "deny", same as an unset WARDYN_OIDC_DEFAULT_ROLE.
func (a *Authenticator) DefaultRole() string {
	return a.cfg.DefaultRole
}

// DefaultRoleIsAdmin reports whether Config.DefaultRole resolves to admin —
// kept in the oidc package (rather than a bare `== RoleAdmin` comparison in
// internal/api) so this package, not a caller, owns what counts as "the admin
// role".
func (a *Authenticator) DefaultRoleIsAdmin() bool {
	return a.cfg.DefaultRole == RoleAdmin
}

// DefaultRoleOutcome reports what an UNMATCHED sign-in would actually derive
// from Config.DefaultRole, validated against userTypes the same way
// deriveRole validates a matched default — so a display surface (GET
// /access) never claims a target a real sign-in would refuse with
// DenialUserTypeUnknown. ok is false when DefaultRole is unset/malformed or
// names a type the store does not hold, except on the admin tier, which
// falls to "standard" instead of refusing, matching deriveRole.
func (a *Authenticator) DefaultRoleOutcome(userTypes []types.UserType) (role, userType string, ok bool) {
	role, userType, ok = SplitMappingTarget(a.cfg.DefaultRole)
	if !ok {
		return "", "", false
	}
	var named []string
	if userType != "" {
		named = []string{userType}
	}
	picked, denial, _, _ := pickUserType(named, userTypeIndex(userTypes))
	if denial == "" {
		return role, picked, true
	}
	if role != RoleAdmin {
		return "", "", false
	}
	return role, types.UserTypeStandard, true
}

// HasOperatorEmails reports whether Config.LegacyAdminEmails
// (WARDYN_OIDC_OPERATOR_EMAILS) is non-empty — the console's People page
// needs this to explain why a row already resolves to admin independent of
// anything it manages.
func (a *Authenticator) HasOperatorEmails() bool {
	return len(a.cfg.LegacyAdminEmails) > 0
}

// HasEmailDomains reports whether Config.AllowedEmailDomains
// (WARDYN_OIDC_EMAIL_DOMAINS) is non-empty — without a domains list,
// email_verified is not enforced, a distinction the People page badge
// otherwise has no way to express.
func (a *Authenticator) HasEmailDomains() bool {
	return len(a.cfg.AllowedEmailDomains) > 0
}

// Issuer returns the PUBLIC OIDC issuer URL — the People page derives a
// human-facing provider name from it (e.g. an Entra tenant issuer ->
// "Microsoft Entra ID") so the SSO chip names WHERE sign-in comes from.
func (a *Authenticator) Issuer() string {
	return a.cfg.IssuerURL
}

// MergedMapEmpty reports whether the ACTUAL merged role map (chart plus rows,
// applying mergeRoleMaps' collision/shadow rules) is empty — needed instead
// of a raw row count, which diverges whenever a stored row is shadowed and
// contributes nothing to what deriveRole actually looks up.
func (a *Authenticator) MergedMapEmpty(rows []RoleMapping) bool {
	merged, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	return len(merged) == 0
}

// IsOperatorEmail reports whether v case-insensitively matches an entry on
// Config.LegacyAdminEmails — the People page write boundary uses this to
// refuse a row up front with the shadow reason it would meet anyway, rather
// than accept it and discover it inert at the next sign-in. Delegates to
// emailInList, the exact match deriveRole's own allowlist check uses.
func (a *Authenticator) IsOperatorEmail(v string) bool {
	return emailInList(v, a.cfg.LegacyAdminEmails)
}

// PreviewRoleAgainst is PreviewRole's PURE twin: runs the identical
// mergeRoleMaps + deriveRole derivation against a CALLER-SUPPLIED candidate
// rows slice instead of a real Config.RoleMappings read, so the write-boundary
// lockout guard (POST/DELETE /access/mappings) can ask "would the acting
// admin still derive admin AFTER this proposed write" without a second store
// round trip or reimplementing merge/derive precedence at the API layer.
//
// No store read, no error return: rows and userTypes are exactly what the
// derivation reads. Compare PreviewRole, which reads Config itself and can
// fail on that read.
func (a *Authenticator) PreviewRoleAgainst(rows []RoleMapping, userTypes []types.UserType, roles, groups []string, email string) Derivation {
	roleMap, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	return deriveRole(roles, groups, email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole, userTypeIndex(userTypes))
}

// deriveRole computes the Wardyn role and user type for a signed-in human from
// the ID token's roles/groups claims, their email, the derivation config and
// the store's user types. The result's Denial is set when the caller
// (CallbackHandler) must deny the login. Matches carries provenance for every
// contributing value — CONSUMED only for logging/preview, never the decision
// itself.
//
// Precedence:
//  1. An empty roleMap disables claim-based derivation: the role comes from the
//     legacy operator allowlist alone — an email on legacyAdminEmails is
//     RoleAdmin, anyone else RoleUser. With NEITHER a role map nor an
//     allowlist every human is RoleAdmin (true pre-0.5), so adopting either
//     is opt-in and upgrade-safe. Everyone here is on "standard".
//  2. Otherwise, build the case-insensitive union of rolesClaim, groupsClaim,
//     and email, look each up in roleMap, and keep the HIGHEST-RANKING tier
//     (user < security_admin < admin), no matter which claim produced it. An
//     email on legacyAdminEmails counts as an additional top-rank RoleAdmin
//     match, winning over any map entry the same email also hits.
//  3. If nothing matched: defaultRole if set, else deny (DenialNoRole).
//  4. The type (pickUserType) comes from every type the matched values named
//     — the default of an admin's user view, and bounds a security admin's
//     runs. None named: the default role's type if it names one, else
//     "standard". A tie or missing type refuses the sign-in, except on the
//     admin tier, which falls to "standard".
//
// Arm 1 is UNTOUCHED by the security_admin tier: a deployment with no role
// map has no way to express the third tier, so nothing changes for it. The
// allowlist is an ADMIN allowlist; there is no security-admin twin.
func deriveRole(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string, userTypes map[string]types.UserType) Derivation {
	if len(roleMap) == 0 {
		// No role map: claim-based derivation is disabled, but the legacy
		// operator allowlist still splits admin from user — the mandatory-
		// minimum SSO config, honored here so a deployment that set only the
		// allowlist doesn't silently promote every signed-in human to admin.
		// Only when NEITHER is set does every human default to admin
		// (true pre-0.5).
		d := Derivation{Role: RoleAdmin, UserType: types.UserTypeStandard}
		switch {
		case len(legacyAdminEmails) == 0:
		case emailInList(email, legacyAdminEmails):
			d.Matches = []Match{{Value: email, Role: RoleAdmin, Source: MatchSourceOperatorAllowlist}}
		default:
			d.Role = RoleUser
		}
		return d
	}
	// best is the highest-ranking tier found so far ("" = nothing yet). One
	// ordering (roleRank), one comparison, one place a fourth tier needs to
	// change.
	best := ""
	var matches []Match
	// named is every user type a matched value names, in match order, deduped
	// — pickUserType's input.
	var named []string
	if emailInList(email, legacyAdminEmails) {
		// Top rank by construction: the allowlist wins even over a map row
		// the same email hits, security_admin entries included.
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
	// value: the same value can appear in BOTH rolesClaim and groupsClaim,
	// which without this would append a duplicate Match for what a human
	// reads as one thing matching twice. It never dedupes against the
	// operator-allowlist match above — an email on both is distinct
	// provenance, not a repeat.
	seenMapRow := make(map[string]bool, len(values))
	for _, v := range values {
		if !ASCIIOnly(v) {
			continue // fail closed: see ASCIIOnly
		}
		key := strings.ToLower(strings.TrimSpace(v))
		// SplitMappingTarget, not "mapped != \"\"": an absent key and an
		// unrecognized value must behave identically. A value that survived
		// ParseRoleMap/mergeRoleMaps is already valid; this is defense in
		// depth for a row that reached the map some other way.
		role, userType, ok := SplitMappingTarget(roleMap[key])
		if !ok {
			continue
		}
		if roleRank(role) > roleRank(best) {
			best = role
		}
		if userType != "" && !slices.Contains(named, userType) {
			named = append(named, userType)
		}
		// Provenance records EVERY contributing value at ITS OWN role, not the
		// winning one — PreviewRole needs the losing matches too.
		if !seenMapRow[key] {
			seenMapRow[key] = true
			matches = append(matches, Match{Value: v, Role: role, UserType: userType, Source: MatchSourceMapRow})
		}
	}
	defRole, defType, defOK := SplitMappingTarget(defaultRole)
	if best == "" {
		if !defOK {
			return Derivation{Denial: DenialNoRole}
		}
		best = defRole
		matches = []Match{{Role: defRole, UserType: defType, Source: MatchSourceDefaultRole}}
	}
	if len(named) == 0 && defType != "" {
		named = []string{defType}
	}
	userType, denial, tied, unknown := pickUserType(named, userTypes)
	if denial != "" {
		if best != RoleAdmin {
			return Derivation{Matches: matches, Denial: denial, Tied: tied, Unknown: unknown}
		}
		// The admin tier is exempt from everything a type decides, so
		// refusing it here would lock out every admin over a setting that
		// grants or withholds nothing for them. Security admins stay
		// refused: their type bounds their runs.
		userType = types.UserTypeStandard
	}
	return Derivation{Role: best, UserType: userType, Matches: matches, Tied: tied, Unknown: unknown}
}

// maxSessionGroupsBytes bounds what Session.Groups may contribute to the JSON
// payload — NOT the cookie, a bigger number by a factor easy to forget: the
// base64 in encodeSession expands the payload by a THIRD, plus a dot and the
// 44-char HMAC. A browser drops a cookie over ~4096 bytes ENTIRELY, no error,
// so budgeting the payload as if it were the cookie overshoots by ~1100
// bytes and breaks login for the group-heavy directory this field serves.
//
// 2048 for groups plus a few hundred for sub/email/role/expiry lands the
// cookie near 3100. That still carries ~100 typical group names; beyond that,
// per-group grants were never the workable answer anyway (grant the user
// directly, or prefer Entra App Roles, on the smaller "roles" claim).
// TestSessionGroupsCapKeepsTheCookieUsable measures the real encoded cookie
// rather than trusting this arithmetic.
const maxSessionGroupsBytes = 2048

// sessionGroups normalizes the ID token's roles+groups claims into the group
// identity a `group`-subject capability grant matches against: the union of
// both claims, trimmed, lowercased, printable-ASCII only, deduped, sorted, and
// truncated to maxSessionGroupsBytes.
//
// NEVER returns nil — an empty result is the empty non-nil slice; nil is
// reserved for "this cookie predates 0.6".
//
// Printable-ASCII only (CanonicalGroupSubject, shared with internal/api's
// write boundaries): a grant subject is an operator-authored ASCII string,
// and Unicode case folding lets a crafted claim fold ONTO one, so the guard
// runs on the RAW value, BEFORE the fold.
//
// SORTED, then truncated FROM THE END: the drop must be deterministic, so the
// same human loses the same groups on every login — a stable answer instead
// of a coin flip for an admin debugging "why does this grant not apply".
//
// The second return says whether the snapshot is PARTIAL, and it is an
// authorization input (PF-26), not a diagnostic: a group-subject governance
// assignment can fall off this cap and the resolver would hand the member the
// DEPLOYMENT ceiling with no refusal or audit. The bit rides the session to
// internal/api's ceiling resolver, which treats a truncated snapshot exactly
// as a missing one.
//
// claimNames is the ID token's `_claim_names` (OIDC Core distributed claims;
// nil when none) — the SECOND, IdP-side way this snapshot can be partial.
// Entra ID stops emitting `groups`/`roles` entirely past the token limit and
// sends a Graph pointer instead; the claim then decodes to nil, identical to
// "asked, there were none". Wardyn does not dereference the pointer (a Graph
// call with its own credential and egress, at login latency) — it fails
// closed and marks the snapshot partial.
//
// A value CanonicalGroupSubject refuses is the THIRD way, invisible to the
// check below since the drop happens before uniq is built (a directory
// naming groups in a non-English locale hits this on an ordinary login). It
// stamps the same bit — see unrepresentable below.
func sessionGroups(rolesClaim, groupsClaim []string, claimNames map[string]any) (groups []string, truncated bool) {
	seen := make(map[string]bool, len(rolesClaim)+len(groupsClaim))
	uniq := make([]string, 0, len(rolesClaim)+len(groupsClaim))
	// unrepresentable counts claim values this snapshot CANNOT carry — the
	// THIRD way it is partial, invisible to both the byte cap and the
	// `_claim_names` pointer: the snapshot would read COMPLETE while a real
	// group is missing, and a DENY or ceiling written against it simply
	// wouldn't match. A drop here stamps the same bit as the cap and overage.
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
		// character the encoder escapes. Control characters are already
		// filtered out above.
		cost := len(g) + 3 + strings.Count(g, `"`) + strings.Count(g, `\`)
		if used+cost > maxSessionGroupsBytes {
			break
		}
		used += cost
		out = append(out, g)
	}
	// Either kind of partial snapshot — dropped at the byte cap here, or
	// never sent by the IdP (overage) — stamps the same bit.
	if claimsOverage(claimNames) {
		return out, true
	}
	return out, len(out) < len(uniq) || unrepresentable > 0
}

// CanonicalGroupSubject canonicalizes an operator-authored group name into
// the EXACT string a session snapshot carries, or reports ok=false when NO
// snapshot can ever carry it (empty/whitespace-only, or non-printable-ASCII).
//
// Exported because sessionGroups is the MATCH surface and internal/api's
// three group-subject WRITE surfaces must refuse exactly what this drops —
// one implementation, so write and match can never disagree about what a
// group name is.
//
// THE ASCII GUARD RUNS ON THE RAW VALUE, BEFORE THE FOLD — strings.ToLower
// does Unicode case mapping (KELVIN SIGN U+212A folds to ASCII 'k'), so
// guarding the LOWERED value would let a crafted claim fold onto a real
// operator-authored group and inherit every grant bound to it. Fold first,
// guard second, and the guard is decorative.
func CanonicalGroupSubject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !printableASCII(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// claimsOverage reports whether the ID token's `_claim_names` says the IdP
// OMITTED a claim this package derives identity from, rather than sending an
// empty one. Entra ID stops emitting `groups`/`roles` past the token limit
// (200 for a JWT) and sends a distributed-claim pointer instead; the claim
// then decodes to nil, identical to "asked, there were none" — this marker
// is the only thing that tells the two apart. Wardyn fails closed rather than
// dereference the pointer.
//
// BOTH claims are checked, since both feed both derivations: sessionGroups
// unions them, and deriveRole looks both up in the role map. One function,
// two callers (sessionGroups' truncation bit and overageWidensRole), because
// "was this token answerable" must have exactly one answer — they diverged
// once, and on WARDYN_OIDC_DEFAULT_ROLE=admin that silently promoted a
// group-mapped member to super admin.
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
// deny rather than serve, since an overage's "nothing matched" is an absence
// of evidence, not a fact.
//
// Deliberately narrow, so it costs nothing in the ordinary posture:
//   - A real MATCH is served as-is (hiding a claim can only REMOVE matches).
//   - A default that cannot widen is served too (defaultRole=member is the
//     narrowest tier there is; keyed on roleRank so a fourth tier inherits
//     the rule).
//   - Only a fallthrough to a default that OUTRANKS the narrowest tier is
//     refused — today WARDYN_OIDC_DEFAULT_ROLE=admin.
//
// Arm 1 (no role map at all) is untouched: it never derives from a claim, and
// its Matches are never MatchSourceDefaultRole.
func overageWidensRole(claimNames map[string]any, role string, matches []Match) bool {
	return unanswerableWidensRole(claimsOverage(claimNames), role, matches)
}

// unanswerableWidensRole is overageWidensRole's rule with its CAUSE factored
// out: an IdP overage is not the only way a claim goes unanswered.
//
// The second way is a claim sent in a shape this build cannot decode.
// CallbackHandler decodes tolerantly so a scalar string doesn't fail the
// whole login, but the claim then contributes nothing, and arm 3's
// fallthrough is once again "nothing matched" standing in for "nobody could
// read it" — the same escalation, so it takes the same refusal.
func unanswerableWidensRole(unanswerable bool, role string, matches []Match) bool {
	if !unanswerable {
		return false
	}
	if !slices.ContainsFunc(matches, func(m Match) bool { return m.Source == MatchSourceDefaultRole }) {
		return false
	}
	return roleRank(role) > roleRank(RoleUser)
}

// printableASCII reports whether every rune of s is a printable ASCII
// character (U+0020..U+007E). Stricter than ASCIIOnly — see sessionGroups.
func printableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}

// emailInList reports whether email case-insensitively matches an entry in
// list (mirrors internal/api's isOperator). email is trimmed here too —
// without it, a padded ID-token claim would silently miss legacyAdminEmails
// while still matching the role map.
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

// ASCIIOnly reports whether s contains no rune above ASCII. Exported so
// internal/api's console-managed role-map writes refuse exactly what this
// package refuses at login — one implementation for what "ASCII" means.
//
// Case-insensitive matching (ToLower/EqualFold) does Unicode case folding,
// under which e.g. a KELVIN SIGN "k" (U+212A) MATCHES an ASCII string — the
// same fold-escalation guard as isOperator, applied here to RoleMap /
// LegacyAdminEmails, both operator-authored ASCII allowlists a crafted
// non-ASCII claim must never fold onto.
func ASCIIOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// claimNamesKeys returns the distributed-claim names present in a token's
// `_claim_names`, sorted, for the one log line an operator debugging a
// claims_overage denial has to go on. Keys only — the values are
// `_claim_sources` references and say nothing this log needs.
func claimNamesKeys(claimNames map[string]any) []string {
	keys := make([]string, 0, len(claimNames))
	for k := range claimNames {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

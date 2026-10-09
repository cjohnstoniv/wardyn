// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// derive.go holds the IDENTITY-DERIVATION seam: everything that turns an ID token's claims into WHO
// this human is (deriveRole, Config.RoleMap, the legacy operator allowlist) and their group snapshot
// (sessionGroups). Pure claims-in, identity-out — nothing here touches HTTP, cookies, or the OAuth2
// exchange, so it is unit-testable without a signed token or a fake IdP.
package oidc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── role derivation ─────────────────────────────────────────────────────────

// Wardyn roles a session can carry. See Session.Role / Config.RoleMap.
//
// RoleSecurityAdmin is the THIRD tier: governs security posture (approvals, audit, permission
// grants) WITHOUT the super admin's reach into other people's runs or credentials. NOT a rung below
// RoleAdmin — internal/api keeps two predicates (isOperator = super only, isSecurityOperator = super
// OR security admin) since security_admin ⊄ admin at the role-snapshot stamp sites. MAPPED TIER
// ONLY: reachable only through the merged role map, never an email-allowlist twin, and cmd/wardynd
// REFUSES it as a fallthrough default.
const (
	RoleAdmin         = "admin"
	RoleSecurityAdmin = "security_admin"
	RoleUser          = "user"
)

// RemovedRoleMember is the pre-0.8 name of RoleUser. 0.8 accepted it as the user tier on the built-in
// "standard" type; 0.9 treats it as any other unknown role value. It stays a reserved word so it can
// never parse as a user type id, and it is the one value RemovedRoleHint explains.
const RemovedRoleMember = "member"

// RemovedRoleHint is the sentence appended to a bad-role error when v is RemovedRoleMember, "" for any
// other value.
func RemovedRoleHint(v string) string {
	if v != RemovedRoleMember {
		return ""
	}
	return ` ("` + RemovedRoleMember + `" was removed in 0.9: use "` + RoleUser + `", or the id of a user type, in its place)`
}

// Roles is the closed set of recognized role values, in rank order, pinned against the DDL parity
// guard.
var Roles = []string{RoleAdmin, RoleSecurityAdmin, RoleUser}

// ValidRole reports whether s is a recognized role value; all callers fail closed on a typo.
// WARDYN_OIDC_DEFAULT_ROLE validates through validDefaultRole (cmd/wardynd), stricter than this:
// it additionally refuses RoleSecurityAdmin.
func ValidRole(s string) bool {
	for _, r := range Roles {
		if s == r {
			return true
		}
	}
	return false
}

// roleRank orders role values for deriveRole's highest-wins fold; unrecognized ranks 0. An ordering
// over DERIVATION inputs, not an authorization ladder (see RoleSecurityAdmin's doc).
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

// ParseRoleMap parses WARDYN_OIDC_ROLE_MAP: comma-separated "value=role" pairs, matched
// case-insensitively against an ID token's roles/groups/email. Empty/blank input returns a nil map
// (derivation disabled) and no error; non-empty input yielding no usable entry is an error, never a
// silent nil — nil means "everyone is admin".
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
		if !ValidMappingTarget(v) {
			return nil, fmt.Errorf("entry %q: invalid role %q (want %q, %q, %q or a user type id)%s", pair, v, RoleAdmin, RoleSecurityAdmin, RoleUser, RemovedRoleHint(v))
		}
		// A non-ASCII key can never match; under DEFAULT_ROLE=admin that would silently grant the
		// default instead of the lesser role the operator meant to name.
		if !ASCIIOnly(k) {
			return nil, fmt.Errorf("entry %q: non-ASCII value can never match (matching is ASCII-only)", pair)
		}
		key := strings.ToLower(k)
		// A duplicate key would silently let the LAST entry win; reject instead of guessing.
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

// MatchSource names which config source produced a Match: a role-map row, the operator-email
// allowlist, or DefaultRole's fallthrough.
type MatchSource string

const (
	MatchSourceMapRow            MatchSource = "map_row"
	MatchSourceOperatorAllowlist MatchSource = "operator_allowlist"
	MatchSourceDefaultRole       MatchSource = "default_role"
)

// Match is one claim/email value that contributed to a derived role, with where it came from.
// deriveRole returns every match found, not only the winner, so a caller can show WHY. json tags
// are POST /access/preview's wire shape.
type Match struct {
	// Value is the claim/email value that matched, exactly as carried. Empty for a default match.
	Value string `json:"value"`
	// Role is the tier this value maps to; UserType is set only when Role is the user tier.
	Role     string      `json:"role"`
	UserType string      `json:"user_type,omitempty"`
	Source   MatchSource `json:"source"`
}

// RoleMapping is one console-managed value=>role row. Value is EXPECTED already canonical (the API
// write boundary owns that); mergeRoleMaps does not trust it blindly — a non-canonical row or
// invalid Role is DROPPED AND LOGGED rather than stored verbatim.
type RoleMapping struct {
	Value    string
	Role     string
	UserType string
}

// target is the role-map value a row contributes: its tier, or its type for a user row. ok is
// false for a row mergeRoleMaps must drop.
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

// RoleMappingSource is the console's store-backed role-mapping source, read once per login and
// merged with the chart's WARDYN_OIDC_ROLE_MAP. nil (the default) disables the console source.
type RoleMappingSource interface {
	// ListRoleMappings: a non-nil error DENIES the login rather than falling back to env-only,
	// which could WIDEN access under WARDYN_OIDC_DEFAULT_ROLE=admin.
	ListRoleMappings(ctx context.Context) ([]RoleMapping, error)
}

// mergeRoleMaps builds the map deriveRole looks values up in, from the chart's WARDYN_OIDC_ROLE_MAP
// plus the console's role-mapping rows. chart always wins a duplicate key (rows are edited at
// runtime, chart is a boot-time decision). A row matching a legacyAdminEmails entry is shadowed too
// — that allowlist is a stronger, harder-to-edit admin source. shadowed also carries a row rejected
// as non-canonical or invalid, so every arm that contributes nothing to merged is logged, not silent.
func mergeRoleMaps(chart map[string]string, legacyAdminEmails []string, rows []RoleMapping) (merged map[string]string, shadowed []string) {
	merged = make(map[string]string, len(chart)+len(rows))
	for k, v := range chart {
		merged[k] = v
	}
	for _, row := range rows {
		// Enforce the canonical contract rather than trusting it: an empty/whitespace Value would
		// match ANY such claim in deriveRole's loop, an admin escalation if Role is admin.
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

// PreviewRole runs the SAME derivation CallbackHandler would, so the console's People page can show
// "who would this row make an admin" before that person signs in. err is non-nil only when the
// store read failed; it never falls back to an env-only preview (same fail-closed reason as login).
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

// ChartRoleMap returns a COPY of the boot-time role map so a handler can never mutate boot config.
func (a *Authenticator) ChartRoleMap() map[string]string {
	out := make(map[string]string, len(a.cfg.RoleMap))
	for k, v := range a.cfg.RoleMap {
		out[k] = v
	}
	return out
}

// DefaultRole returns Config.DefaultRole, the fallthrough when the merged map matched nothing.
// Empty means "deny".
func (a *Authenticator) DefaultRole() string {
	return a.cfg.DefaultRole
}

// DefaultRoleIsAdmin reports whether Config.DefaultRole resolves to admin, kept here so this
// package, not a caller, owns what counts as "the admin role".
func (a *Authenticator) DefaultRoleIsAdmin() bool {
	return a.cfg.DefaultRole == RoleAdmin
}

// DefaultRoleOutcome reports what an UNMATCHED sign-in would actually derive from Config.DefaultRole,
// validated the same way deriveRole validates a matched default, so a display surface never claims a
// target a real sign-in would refuse. ok is false when DefaultRole is unset/malformed or names a
// type the store lacks, except on the admin tier, which falls to "standard" instead, matching deriveRole.
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

// HasOperatorEmails reports whether Config.LegacyAdminEmails is non-empty — the People page needs
// this to explain why a row already resolves to admin independent of anything it manages.
func (a *Authenticator) HasOperatorEmails() bool {
	return len(a.cfg.LegacyAdminEmails) > 0
}

// EnforcesEmailVerified reports whether sign-in requires a verified email: a domains allowlist is
// set or Config.RequireEmailVerified is on. Without either, email_verified is not enforced, a
// distinction the People page badge otherwise can't express.
func (a *Authenticator) EnforcesEmailVerified() bool {
	return len(a.cfg.AllowedEmailDomains) > 0 || a.cfg.RequireEmailVerified
}

// Issuer returns the PUBLIC OIDC issuer URL, from which the People page derives a human-facing
// provider name (e.g. an Entra tenant -> "Microsoft Entra ID") for the SSO chip.
func (a *Authenticator) Issuer() string {
	return a.cfg.IssuerURL
}

// MergedMapEmpty reports whether the ACTUAL merged role map is empty — needed instead of a raw row
// count, which diverges whenever a stored row is shadowed and contributes nothing.
func (a *Authenticator) MergedMapEmpty(rows []RoleMapping) bool {
	merged, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	return len(merged) == 0
}

// IsOperatorEmail reports whether v matches an entry on Config.LegacyAdminEmails — the People page
// write boundary uses this to refuse a row up front rather than discover it inert at sign-in.
// Delegates to emailInList, the exact match deriveRole's own allowlist check uses.
func (a *Authenticator) IsOperatorEmail(v string) bool {
	return emailInList(v, a.cfg.LegacyAdminEmails)
}

// PreviewRoleAgainst is PreviewRole's PURE twin: runs the identical mergeRoleMaps + deriveRole
// derivation against a CALLER-SUPPLIED candidate rows slice, so the write-boundary lockout guard can
// ask "would the acting admin still derive admin AFTER this proposed write" without a second store
// round trip. No store read, no error return — compare PreviewRole, which reads Config itself.
func (a *Authenticator) PreviewRoleAgainst(rows []RoleMapping, userTypes []types.UserType, roles, groups []string, email string) Derivation {
	roleMap, _ := mergeRoleMaps(a.cfg.RoleMap, a.cfg.LegacyAdminEmails, rows)
	return deriveRole(roles, groups, email, roleMap, a.cfg.LegacyAdminEmails, a.cfg.DefaultRole, userTypeIndex(userTypes))
}

// deriveRole computes the Wardyn role and user type for a signed-in human from the ID token's
// roles/groups claims, their email, the derivation config and the store's user types. Denial is set
// when CallbackHandler must deny the login. Matches carries provenance for every contributing value
// — consumed only for logging/preview, never the decision itself.
//
// Precedence: (1) an empty roleMap disables claim-based derivation — the role comes from the legacy
// operator allowlist alone (RoleAdmin if listed, else RoleUser); with NEITHER set, every human is
// RoleAdmin (true pre-0.5, so adopting either is opt-in and upgrade-safe). This arm is UNTOUCHED by
// the security_admin tier: the allowlist is an ADMIN allowlist with no security-admin twin. (2)
// otherwise, union rolesClaim/groupsClaim/email case-insensitively, look each up in roleMap, and
// keep the HIGHEST-RANKING tier regardless of which claim produced it — an allowlist email counts
// as an additional top-rank RoleAdmin match. (3) nothing matched: defaultRole if set, else deny. (4)
// the type (pickUserType) comes from every type the matched values named (none named: the default
// role's type, else "standard"); a tie or missing type refuses the sign-in except on the admin
// tier, which falls to "standard".
func deriveRole(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string, userTypes map[string]types.UserType) Derivation {
	if len(roleMap) == 0 {
		// No role map: claim-based derivation is disabled, but the legacy operator allowlist still
		// splits admin from user, so a deployment that set only the allowlist doesn't silently
		// promote every signed-in human to admin.
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
	// best is the highest-ranking tier found so far ("" = nothing yet).
	best := ""
	var matches []Match
	// named is every user type a matched value names, in match order, deduped — pickUserType's input.
	var named []string
	if emailInList(email, legacyAdminEmails) {
		// Top rank by construction: the allowlist wins even over a map row the same email hits.
		best = RoleAdmin
		matches = append(matches, Match{Value: email, Role: RoleAdmin, Source: MatchSourceOperatorAllowlist})
	}
	values := make([]string, 0, len(rolesClaim)+len(groupsClaim)+1)
	values = append(values, rolesClaim...)
	values = append(values, groupsClaim...)
	if email != "" {
		values = append(values, email)
	}
	// seenMapRow dedupes MatchSourceMapRow entries keyed on the lowered claim value: the same value
	// can appear in BOTH rolesClaim and groupsClaim. Never dedupes against the allowlist match above
	// — an email on both is distinct provenance.
	seenMapRow := make(map[string]bool, len(values))
	for _, v := range values {
		if !ASCIIOnly(v) {
			continue // fail closed: see ASCIIOnly
		}
		key := strings.ToLower(strings.TrimSpace(v))
		// SplitMappingTarget, not "mapped != \"\"": an absent key and an unrecognized value must
		// behave identically.
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
		// Provenance records EVERY contributing value at ITS OWN role, not the winning one.
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
		// The admin tier is exempt from everything a type decides; security admins stay refused
		// since their type bounds their runs.
		userType = types.UserTypeStandard
	}
	return Derivation{Role: best, UserType: userType, Matches: matches, Tied: tied, Unknown: unknown}
}

// maxSessionGroupsBytes bounds what Session.Groups may contribute to the JSON payload — NOT the
// cookie: base64 in encodeSession expands the payload by a THIRD plus a dot and the 44-char HMAC,
// and a browser drops a cookie over ~4096 bytes ENTIRELY with no error, so budgeting as if this were
// the cookie size would overshoot and break login. 2048 for groups plus a few hundred for the rest
// of the session lands the cookie near 3100, still carrying ~100 typical group names; beyond that,
// per-group grants were never the workable answer anyway. TestSessionGroupsCapKeepsTheCookieUsable
// measures the real encoded cookie rather than trusting this arithmetic.
const maxSessionGroupsBytes = 2048

// sessionGroups normalizes the ID token's roles+groups claims into the group identity a
// `group`-subject capability grant matches against: the union of both claims, trimmed, lowercased,
// printable-ASCII only, deduped, sorted, and truncated to maxSessionGroupsBytes.
//
// NEVER returns nil — an empty result is the empty non-nil slice; nil is reserved for "this cookie
// predates 0.6". Printable-ASCII only (CanonicalGroupSubject): a grant subject is operator-authored
// ASCII, and Unicode case folding could fold a crafted claim onto one, so the guard runs on the RAW
// value before the fold. SORTED, then truncated FROM THE END, so the drop is deterministic — the
// same human loses the same groups every login.
//
// SECURITY: the second return says whether the snapshot is PARTIAL, and it is an AUTHORIZATION
// INPUT, not a diagnostic — a group-subject grant can fall off this cap and the ceiling resolver
// would hand the member the DEPLOYMENT ceiling with no refusal or audit, so it treats a truncated
// snapshot exactly as a missing one. Three ways it goes partial: the byte cap above; claimNames, the
// ID token's `_claim_names` (Entra stops emitting `groups`/`roles` past the token limit and sends a
// Graph pointer instead, decoding to nil identical to "asked, none" — Wardyn fails closed rather
// than dereference the pointer); and a value CanonicalGroupSubject refuses, invisible to the
// byte-cap check since the drop happens before uniq is built — see unrepresentable below.
func sessionGroups(rolesClaim, groupsClaim []string, claimNames map[string]any) (groups []string, truncated bool) {
	seen := make(map[string]bool, len(rolesClaim)+len(groupsClaim))
	uniq := make([]string, 0, len(rolesClaim)+len(groupsClaim))
	// unrepresentable counts claim values this snapshot CANNOT carry — the THIRD way it is partial,
	// invisible to both the byte cap and the `_claim_names` pointer. A drop here stamps the same bit
	// as the cap and overage.
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
		// Exact JSON cost: two quotes, a comma, and one extra byte per escaped character.
		cost := len(g) + 3 + strings.Count(g, `"`) + strings.Count(g, `\`)
		if used+cost > maxSessionGroupsBytes {
			break
		}
		used += cost
		out = append(out, g)
	}
	// Either kind of partial snapshot — dropped here, or never sent (overage) — stamps the same bit.
	if claimsOverage(claimNames) {
		return out, true
	}
	return out, len(out) < len(uniq) || unrepresentable > 0
}

// CanonicalGroupSubject canonicalizes an operator-authored group name into the EXACT string a
// session snapshot carries, or reports ok=false when NO snapshot can ever carry it. Exported
// because sessionGroups is the MATCH surface and internal/api's group-subject WRITE surfaces must
// refuse exactly what this drops — one implementation, so write and match can never disagree.
//
// SECURITY: the ASCII guard runs on the RAW value, BEFORE the fold — see ASCIIOnly's doc for why
// (case folding can turn a non-ASCII claim into a real operator-authored ASCII name).
func CanonicalGroupSubject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !printableASCII(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// claimsOverage reports whether the ID token's `_claim_names` says the IdP OMITTED a claim this
// package derives identity from, rather than sending an empty one. Entra stops emitting
// `groups`/`roles` past the token limit and sends a distributed-claim pointer instead, decoding to
// nil identical to "asked, none" — this marker is the only thing that tells the two apart, and
// Wardyn fails closed rather than dereference the pointer.
//
// BOTH claims are checked since both feed both derivations, in one function with two callers
// (sessionGroups' truncation bit and overageWidensRole) — they diverged once, and on
// WARDYN_OIDC_DEFAULT_ROLE=admin that silently promoted a group-mapped member to super admin.
func claimsOverage(claimNames map[string]any) bool {
	for _, claim := range []string{"groups", "roles"} {
		if _, overage := claimNames[claim]; overage {
			return true
		}
	}
	return false
}

// overageWidensRole reports whether an IdP claim overage turned deriveRole's "nothing matched" into
// a WIDENING default — CallbackHandler must deny rather than serve, since an overage's "nothing
// matched" is an absence of evidence, not a fact. Deliberately narrow: a real MATCH is served as-is
// (hiding a claim can only REMOVE matches), and a default that cannot widen (keyed on roleRank, so
// a fourth tier inherits the rule) is served too — only a fallthrough that OUTRANKS the narrowest
// tier is refused. Arm 1 (no role map) is untouched: it never derives from a claim.
func overageWidensRole(claimNames map[string]any, role string, matches []Match) bool {
	return unanswerableWidensRole(claimsOverage(claimNames), role, matches)
}

// unanswerableWidensRole is overageWidensRole's rule with its CAUSE factored out: an IdP overage is
// not the only way a claim goes unanswered. The second way is a claim sent in a shape this build
// cannot decode — CallbackHandler decodes tolerantly so a scalar string doesn't fail the whole
// login, but the claim then contributes nothing, the same escalation as an overage.
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

// emailInList reports whether email case-insensitively matches an entry in list. email is trimmed
// here too, or a padded ID-token claim would silently miss legacyAdminEmails while still matching
// the role map.
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

// ASCIIOnly reports whether s contains no rune above ASCII. Exported so internal/api's
// console-managed role-map writes refuse exactly what this package refuses at login.
//
// SECURITY: case-insensitive matching (ToLower/EqualFold) does Unicode case folding, under which a
// KELVIN SIGN "k" (U+212A) MATCHES ASCII "k" — so guarding the LOWERED value would let a crafted
// non-ASCII claim fold onto a real operator-authored ASCII entry (RoleMap, LegacyAdminEmails, a
// group name) and inherit everything bound to it. The guard must run on the RAW value, before the
// fold, everywhere this package or internal/api compares an ASCII allowlist case-insensitively.
func ASCIIOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// claimNamesKeys returns the distributed-claim names in a token's `_claim_names`, sorted, for the
// one log line an operator debugging a claims_overage denial has to go on.
func claimNamesKeys(claimNames map[string]any) []string {
	keys := make([]string, 0, len(claimNames))
	for k := range claimNames {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

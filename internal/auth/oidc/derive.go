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
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// ─── role derivation ─────────────────────────────────────────────────────────

// Wardyn roles a session can carry. See Session.Role / Config.RoleMap.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ValidRole reports whether s is a recognized role value. Used to validate
// WARDYN_OIDC_ROLE_MAP entries (ParseRoleMap) and WARDYN_OIDC_DEFAULT_ROLE
// (cmd/wardynd, at boot) — both fail closed on a typo rather than letting a
// garbage role value silently reach a session cookie.
func ValidRole(s string) bool {
	return s == RoleAdmin || s == RoleMember
}

// ParseRoleMap parses WARDYN_OIDC_ROLE_MAP: a comma-separated list of
// "value=role" pairs, e.g.
// "Wardyn.Admin=admin,eng-team=member,alice@corp.com=admin". value is matched
// case-insensitively against an ID token's roles/groups claims or its email
// (see deriveRole); role must be RoleAdmin or RoleMember. Empty/blank input
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
			return nil, fmt.Errorf("entry %q: invalid role %q (want %q or %q)", pair, v, RoleAdmin, RoleMember)
		}
		// A non-ASCII key can NEVER match: deriveRole skips non-ASCII claim
		// values before lookup (asciiOnly, the fold-escalation guard), so
		// this would silently be a dead entry — worse, one that INVERTS
		// intent under WARDYN_OIDC_DEFAULT_ROLE=admin, where the operator
		// meant to name this value out for a lesser role but it can never
		// match and every such login instead gets the default.
		if !asciiOnly(k) {
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

// deriveRole computes the Wardyn role for a signed-in human from the ID
// token's roles/groups claims, their email, and the derivation config
// (Config.RoleMap / Config.LegacyAdminEmails / Config.DefaultRole). ok is
// false only when roleMap is non-empty, nothing matched, and defaultRole is
// empty — the caller (CallbackHandler) must then deny the login.
//
// Precedence:
//  1. An empty roleMap disables claim-based derivation: the role comes from the
//     legacy operator allowlist alone — an email on legacyAdminEmails is
//     RoleAdmin, anyone else RoleMember (main's operator/viewer split, preserved
//     with no role map). With NEITHER a role map nor an allowlist every human is
//     RoleAdmin (true pre-0.5) — so adopting WARDYN_OIDC_ROLE_MAP is opt-in and
//     upgrade-safe, and so is running on only WARDYN_OIDC_OPERATOR_EMAILS.
//  2. Otherwise, build the case-insensitive union of rolesClaim, groupsClaim,
//     and email, and look each value up in roleMap. ANY match resolving to
//     RoleAdmin wins over one resolving to RoleMember, no matter which claim
//     produced it. An email on legacyAdminEmails (WARDYN_OIDC_OPERATOR_EMAILS)
//     counts as an additional RoleAdmin match — it wins even over a
//     RoleMember entry the same email also hits.
//  3. If nothing matched at all: defaultRole if set, else deny.
func deriveRole(rolesClaim, groupsClaim []string, email string, roleMap map[string]string, legacyAdminEmails []string, defaultRole string) (role string, ok bool) {
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
			return RoleAdmin, true
		}
		if emailInList(email, legacyAdminEmails) {
			return RoleAdmin, true
		}
		return RoleMember, true
	}
	admin := emailInList(email, legacyAdminEmails)
	member := false
	values := make([]string, 0, len(rolesClaim)+len(groupsClaim)+1)
	values = append(values, rolesClaim...)
	values = append(values, groupsClaim...)
	if email != "" {
		values = append(values, email)
	}
	for _, v := range values {
		if !asciiOnly(v) {
			continue // fail closed: see asciiOnly
		}
		switch roleMap[strings.ToLower(strings.TrimSpace(v))] {
		case RoleAdmin:
			admin = true
		case RoleMember:
			member = true
		}
	}
	switch {
	case admin:
		return RoleAdmin, true
	case member:
		return RoleMember, true
	case defaultRole != "":
		return defaultRole, true
	default:
		return "", false
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
// Printable-ASCII only, for the same reason deriveRole's loop skips non-ASCII
// claim values (asciiOnly): a grant subject is an operator-authored ASCII
// string, and Unicode case folding lets a crafted claim fold ONTO one. Control
// characters are dropped with the rest — they cannot appear in a real group
// name, and excluding them keeps the byte budget below exact.
//
// SORTED, then truncated FROM THE END: the drop must be deterministic, so the
// same human with the same claims loses the same groups on every login. An
// admin debugging "why does this grant not apply" gets a stable answer instead
// of a coin flip. (Ordering is lexical, so a truncated human loses their
// alphabetically-last groups — arbitrary, but arbitrary and REPEATABLE.)
func sessionGroups(rolesClaim, groupsClaim []string) []string {
	seen := make(map[string]bool, len(rolesClaim)+len(groupsClaim))
	uniq := make([]string, 0, len(rolesClaim)+len(groupsClaim))
	for _, v := range slices.Concat(rolesClaim, groupsClaim) {
		g := strings.ToLower(strings.TrimSpace(v))
		if g == "" || !printableASCII(g) || seen[g] {
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
	return out
}

// printableASCII reports whether every rune of s is a printable ASCII
// character (U+0020..U+007E). Stricter than asciiOnly — see sessionGroups.
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
	if email == "" || !asciiOnly(email) {
		return false
	}
	for _, e := range list {
		if strings.EqualFold(strings.TrimSpace(e), email) {
			return true
		}
	}
	return false
}

// asciiOnly reports whether s contains no rune above ASCII. Case-insensitive
// matching (ToLower/EqualFold) does Unicode case folding, under which e.g.
// "roſs" (U+017F) or a KELVIN SIGN "k" (U+212A) MATCHES an ASCII string — the
// escalating direction (same fold-escalation guard as internal/api's
// isOperator, applied here to the RoleMap / LegacyAdminEmails match: both are
// operator-authored ASCII allowlists — WARDYN_OIDC_ROLE_MAP and
// WARDYN_OIDC_OPERATOR_EMAILS — that a crafted non-ASCII claim must never
// fold onto).
func asciiOnly(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

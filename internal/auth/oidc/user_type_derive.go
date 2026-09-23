// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// user_type_derive.go is the user-type half of role derivation (0.8, design
// user-types-design.md §2.4). A role-map value names a TARGET: one of the two
// admin tiers, or a user type id, which is the user tier on that type. The
// fold in deriveRole picks the tier by rank as before, and pickUserType picks
// the type: the highest-priority CUSTOM type among the matches, with the
// built-in "standard" type as the tie floor that never wins and never ties.
package oidc

import (
	"context"
	"regexp"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Why a derivation refused a sign-in. Each is also the auth_error code the
// callback redirects with, so the sign-in screen and the audit row name the
// same cause.
const (
	// DenialNoRole: the map is non-empty, nothing matched, and no default
	// role is set.
	DenialNoRole = "no_role"
	// DenialUserTypeAmbiguous: two or more custom user types tie at the top
	// priority among the values that matched. Wardyn never picks one.
	DenialUserTypeAmbiguous = "user_type_ambiguous"
	// DenialUserTypeUnknown: a matched value (or the default role) names a
	// user type that does not exist. Never a wider default.
	DenialUserTypeUnknown = "user_type_unknown"
)

// Derivation is what a sign-in derives: the tier, the user type, and every
// value that contributed. When Denial is set the sign-in is refused and Role
// and UserType are empty; Matches, Tied and Unknown still say why, which is
// what the People page's preview renders.
type Derivation struct {
	Role     string
	UserType string
	Matches  []Match
	Denial   string
	// Tied is the custom types that tied at the top priority, sorted
	// (DenialUserTypeAmbiguous, or an admin whose type fell to "standard").
	Tied []string
	// Unknown is the named types that do not exist, sorted
	// (DenialUserTypeUnknown, or an admin whose type fell to "standard").
	Unknown []string
}

// OK reports whether the derivation lets the sign-in through. The zero value
// (what an errored preview returns) is not OK.
func (d Derivation) OK() bool { return d.Denial == "" && d.Role != "" }

// UserTypeSource is the store of user types, read once per login beside
// RoleMappingSource. A non-nil error DENIES the login in progress, exactly as
// a role-mapping read error does: a type that cannot be looked up cannot be
// shown to exist, and falling back to "standard" could widen a person whose
// real type is narrower. Config.UserTypes wires it; nil means only the
// built-in type exists, so any role-map value naming a custom type refuses.
type UserTypeSource interface {
	ListUserTypes(ctx context.Context) ([]types.UserType, error)
}

// userTypeIDRe is the slug shape migration 0069_user_types CHECKs.
var userTypeIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const maxUserTypeIDLen = 63

// UserTypeIDWellFormed reports whether id has the shape the user_types table
// accepts: lowercase ASCII words joined by single hyphens, at most 63 bytes.
func UserTypeIDWellFormed(id string) bool {
	return len(id) <= maxUserTypeIDLen && userTypeIDRe.MatchString(id)
}

// UserTypeIDReserved reports whether id is a word a role-map value already
// means, so no user type may use it: the three tiers, the retired "member",
// and "denied" (internal/api's spelling of "no role at all" on the People
// page).
func UserTypeIDReserved(id string) bool {
	switch id {
	case RoleAdmin, RoleSecurityAdmin, RoleUser, LegacyRoleMember, "denied":
		return true
	}
	return false
}

// SplitMappingTarget splits a role-map value into the tier and the user type it
// names: admin and security_admin carry no type, "user" is the user tier on
// the built-in "standard" type, and any other well-formed, unreserved id is
// the user tier on that type. ok is false for anything else, "member"
// included: the alias is resolved where configuration is parsed, never here.
func SplitMappingTarget(v string) (role, userType string, ok bool) {
	switch v {
	case RoleAdmin, RoleSecurityAdmin:
		return v, "", true
	case RoleUser:
		return RoleUser, types.UserTypeStandard, true
	}
	if UserTypeIDReserved(v) || !UserTypeIDWellFormed(v) {
		return "", "", false
	}
	return RoleUser, v, true
}

// ValidMappingTarget reports whether v may be a role-map value or
// WARDYN_OIDC_DEFAULT_ROLE: a tier or a well-formed user type id. Whether the
// type EXISTS is a store question, answered at sign-in (DenialUserTypeUnknown)
// and at the console's write boundary.
func ValidMappingTarget(v string) bool {
	_, _, ok := SplitMappingTarget(v)
	return ok
}

// userTypeIndex keys the store's types by id. The built-in type is always
// present, even when no source is wired or the list predates its seed row:
// it is what arm 1 and every "user" value resolve to, and it can never be
// deleted.
func userTypeIndex(list []types.UserType) map[string]types.UserType {
	idx := make(map[string]types.UserType, len(list)+1)
	idx[types.UserTypeStandard] = types.UserType{ID: types.UserTypeStandard, BuiltIn: true}
	for _, t := range list {
		idx[t.ID] = t
	}
	return idx
}

// loadUserTypes reads Config.UserTypes, or the built-in type alone when none
// is wired.
func (a *Authenticator) loadUserTypes(ctx context.Context) (map[string]types.UserType, error) {
	if a.cfg.UserTypes == nil {
		return userTypeIndex(nil), nil
	}
	list, err := a.cfg.UserTypes.ListUserTypes(ctx)
	if err != nil {
		return nil, err
	}
	return userTypeIndex(list), nil
}

// pickUserType chooses one type from the types the matched values named
// (named, in match order). Any unknown id refuses: a value that names a type
// the store does not hold is a configuration nobody finished, and guessing
// "standard" for it could be wider than what was meant. Otherwise the
// highest-priority custom type wins; the built-in type never wins against a
// custom one and never counts toward a tie, so a leftover "=member" line
// beside a new custom type can never lock an org out. Two custom types at the
// top priority refuse: Wardyn never picks between them.
func pickUserType(named []string, idx map[string]types.UserType) (id, denial string, tied, unknown []string) {
	best := -1
	for _, n := range named {
		t, ok := idx[n]
		switch {
		case !ok:
			unknown = append(unknown, n)
		case t.BuiltIn:
		case t.Priority > best:
			best, tied = t.Priority, []string{n}
		case t.Priority == best:
			tied = append(tied, n)
		}
	}
	switch {
	case len(unknown) > 0:
		slices.Sort(unknown)
		return "", DenialUserTypeUnknown, nil, unknown
	case len(tied) > 1:
		slices.Sort(tied)
		return "", DenialUserTypeAmbiguous, tied, nil
	case len(tied) == 1:
		return tied[0], "", nil, nil
	}
	return types.UserTypeStandard, "", nil, nil
}

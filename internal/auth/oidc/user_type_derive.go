// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// user_type_derive.go is the user-type half of role derivation. A role-map
// value names a TARGET: one of the two admin tiers, or a user type id (the
// user tier on that type). deriveRole picks the tier by rank; pickUserType
// picks the highest-priority CUSTOM type among the matches, with the built-in
// "standard" type as the tie floor that never wins and never ties.
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
	// DenialNoRole: the map is non-empty, nothing matched, no default role set.
	DenialNoRole = "no_role"
	// DenialUserTypeAmbiguous: two or more custom user types tie at top priority.
	DenialUserTypeAmbiguous = "user_type_ambiguous"
	// DenialUserTypeUnknown: a matched value names a user type that doesn't exist.
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
	// Tied is the custom types that tied at the top priority, sorted.
	Tied []string
	// Unknown is the named types that do not exist, sorted.
	Unknown []string
}

// OK reports whether the derivation lets the sign-in through. The zero value
// (what an errored preview returns) is not OK.
func (d Derivation) OK() bool { return d.Denial == "" && d.Role != "" }

// UserTypeSource is the store of user types, read once per login. A non-nil
// error DENIES the login: a type that can't be looked up can't be shown to
// exist, and falling back to "standard" could widen a narrower type.
// Config.UserTypes wires it; nil means only the built-in type exists.
type UserTypeSource interface {
	ListUserTypes(ctx context.Context) ([]types.UserType, error)
}

// userTypeIDRe is the slug shape migration 0071_user_types CHECKs.
var userTypeIDRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

const maxUserTypeIDLen = 63

// UserTypeIDWellFormed reports whether id has the shape the user_types table
// accepts: lowercase ASCII words joined by single hyphens, at most 63 bytes.
func UserTypeIDWellFormed(id string) bool {
	return len(id) <= maxUserTypeIDLen && userTypeIDRe.MatchString(id)
}

// UserTypeIDReserved reports whether id is a word a role-map value already
// means: the three tiers, the retired "member", and "denied".
func UserTypeIDReserved(id string) bool {
	switch id {
	case RoleAdmin, RoleSecurityAdmin, RoleUser, LegacyRoleMember, "denied":
		return true
	}
	return false
}

// SplitMappingTarget splits a role-map value into the tier and user type it
// names: admin/security_admin carry no type, "user" maps to the built-in
// "standard" type, and any other well-formed, unreserved id is the user tier
// on that type. ok is false for anything else, "member" included.
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
// type EXISTS is a store question, answered at sign-in.
func ValidMappingTarget(v string) bool {
	_, _, ok := SplitMappingTarget(v)
	return ok
}

// userTypeIndex keys the store's types by id. The built-in type is always
// present, even when no source is wired: it is what every "user" value
// resolves to, and it can never be deleted.
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

// pickUserType chooses one type from the types the matched values named.
// Any unknown id refuses: guessing "standard" for it could be wider than
// intended. The highest-priority custom type wins; the built-in type never
// wins or counts toward a tie. Two custom types tied at top priority refuse.
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

// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// personDerivation is what a person's recorded sign-in derives now: the role, as personRoleResolver answers it
// (roleUnknown for a truncated verified group list, accessDeniedRole when sign-in would refuse them), and the
// user type the same derivation picked, "" unless the role is the user tier.
type personDerivation struct {
	Role     string
	UserType string
}

// personDerivationResolver is personRoleResolver's derivation returned whole: the same inputs (the current role
// mappings and user types, no roles claim) through the one derivation, deriveRecordRole, and the same role, with
// the user type beside it. Without SSO the role is "" and so is the type.
func (s *Server) personDerivationResolver(r *http.Request) (func(p store.PersonListing) personDerivation, error) {
	if s.cfg.OIDC == nil {
		return func(store.PersonListing) personDerivation { return personDerivation{} }, nil
	}
	rows, err := s.cfg.Store.ListRoleMappings(r.Context())
	if err != nil {
		return nil, err
	}
	userTypes, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		return nil, err
	}
	mappings := toOIDCRoleMappings(rows)
	return func(p store.PersonListing) personDerivation {
		if p.GroupsVerified && p.GroupsTruncated {
			return personDerivation{Role: roleUnknown}
		}
		d := s.deriveRecordRole(mappings, userTypes, nil, p.Groups, p.Email)
		if !d.OK() {
			return personDerivation{Role: accessDeniedRole}
		}
		return personDerivation{Role: d.Role, UserType: d.UserType}
	}, nil
}

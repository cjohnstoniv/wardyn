// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// The auth.fail reasons the sign-in callback emits: the role map named a user
// type for this person that is ambiguous, or that does not exist; or the
// person's subject is a reserved principal (isReservedPrincipal). Equal to
// oidc.DenialUserTypeAmbiguous / DenialUserTypeUnknown / DenialReservedPrincipal;
// restated here so the reason enum stays readable from this package alone.
// authFailedReservedPrincipal is also the reason a session or wdn_ token
// carrying a reserved principal is refused with at request time.
const (
	authFailedUserTypeAmbiguous = "user_type_ambiguous"
	authFailedUserTypeUnknown   = oidc.DenialUserTypeUnknown
	authFailedReservedPrincipal = oidc.DenialReservedPrincipal
)

// auditSignInDenied records a sign-in the callback refused over its user type
// or a reserved subject as auth.fail. Any other reason records nothing: the
// row's enum is closed.
func (s *Server) auditSignInDenied(r *http.Request, reason string) {
	switch reason {
	case oidc.DenialUserTypeAmbiguous:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeAmbiguous)
	case oidc.DenialUserTypeUnknown:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeUnknown)
	case oidc.DenialReservedPrincipal:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedReservedPrincipal)
	}
}

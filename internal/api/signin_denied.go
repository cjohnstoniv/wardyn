// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// The two auth.fail reasons the sign-in callback emits: the role map named a
// user type for this person that is ambiguous, or that does not exist. Equal
// to oidc.DenialUserTypeAmbiguous / DenialUserTypeUnknown; restated here so
// the reason enum stays readable from this package alone.
const (
	authFailedUserTypeAmbiguous = "user_type_ambiguous"
	authFailedUserTypeUnknown   = "user_type_unknown"
)

// auditSignInDenied records a sign-in the callback refused over its user type
// as auth.fail. Any other reason records nothing: the row's enum is closed.
func (s *Server) auditSignInDenied(r *http.Request, reason string) {
	switch reason {
	case oidc.DenialUserTypeAmbiguous:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeAmbiguous)
	case oidc.DenialUserTypeUnknown:
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedUserTypeUnknown)
	}
}

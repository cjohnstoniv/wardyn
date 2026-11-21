// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// This file stands in, on the registration lane only, for the one in delegation_portal.go that the exchange
// lane owns (the same two consts, the same function). It exists so this lane compiles alone, and is deleted
// when the lanes merge.

const (
	reasonDelegatePersonNotAllowed    = "delegate_person_not_allowed"    // a person list that confers an admin, sponsor, approver or console role named a portal person
	reasonDelegatePersonUnknownPortal = "delegate_person_unknown_portal" // a person list named a portal person whose issuer has no binding
)

// writePortalRulingError answers 422 for the two rulings of the store's writes of admin-written person lists
// and reports whether it did.
func writePortalRulingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrDelegatePersonNotAllowed):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegatePersonNotAllowed, "a portal person cannot hold this role")
	case errors.Is(err, store.ErrDelegatePersonUnknownPortal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal, "no issuer portal is registered for this person's issuer")
	default:
		return false
	}
	return true
}

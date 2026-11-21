// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrDelegatePersonNotAllowed refuses a person list that confers an admin, sponsor, approver or console role
// on a portal person (a delegate:person: principal). It is the types value, so a package outside the store
// that calls types.RefuseDelegatePersonSubject returns the same error.
var ErrDelegatePersonNotAllowed = types.ErrDelegatePersonNotAllowed

// ErrDelegatePersonUnknownPortal refuses a person list that names a portal person whose issuer key has no
// binding: the principal could never have been introduced by any registered portal.
var ErrDelegatePersonUnknownPortal = errors.New("store: no issuer portal is bound to this person's issuer key")

// ErrSubjectTokenStale is a portal grant mint refused because less than five seconds of life would remain
// once the database clock was read under the mint's locks. Nothing is written.
var ErrSubjectTokenStale = errors.New("store: the subject token is too old for a grant")

// ErrPortalGone is an exchange write refused because the portal's issuer binding or the portal itself is no
// longer there (retired, or revoked after the exchange verified its token).
var ErrPortalGone = errors.New("store: the portal or its issuer binding is gone")

// ErrPortalPersonPurged is an exchange write refused because the person was permanently purged after the
// exchange validated them: a purged identity is a tombstone, and a grant or history row minted for it would
// restore what the purge removed. Nothing is written.
var ErrPortalPersonPurged = errors.New("store: the portal person was purged")

// ErrAdmissionChanged is an exchange write refused because the portal's admission revision moved after the
// exchange verified its token; the portal retries under the new rule.
var ErrAdmissionChanged = errors.New("store: the portal's admission rule changed")

// ErrStaleCompare is a compare-and-set write that matched no row: the value it expected is no longer there.
var ErrStaleCompare = fmt.Errorf("%w: the stored value is no longer the one this write expected", ErrConflict)

// The three refusals of an issuer registration against a binding. Each wraps ErrConflict.
var (
	ErrIssuerAddressConflict  = fmt.Errorf("%w: the issuer is bound to another internal address", ErrConflict)
	ErrIssuerSpellingConflict = fmt.Errorf("%w: another spelling of this issuer is already bound", ErrConflict)
	ErrIssuerProfileConflict  = fmt.Errorf("%w: the issuer is bound to another person-id claim", ErrConflict)
)

// ErrIssuerInUse refuses the retirement of an issuer binding that a portal, a person, a grant or a stored
// reference still uses.
var ErrIssuerInUse = fmt.Errorf("%w: the issuer binding is in use", ErrConflict)

// ErrPersonNotSuspended refuses a portal purge of a person who is not (or no longer) suspended, or, for the
// scheduled mode, not due under their own suspension.
var ErrPersonNotSuspended = errors.New("store: the person is not suspended")

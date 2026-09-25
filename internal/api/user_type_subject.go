// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The user type as a subject (user-types design §2.3, §7): the caller's one
// type is matched by capability grants, governance assignments and drive
// grants beside their user and group identities.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// oidcUserTypeCtxKey carries the user type of the same verified human: the
// session's stamp on the SSO branch, the token row's on the api-token branch
// (#611), published by withHumanIdentity beside the role so the auth
// middleware stays the single place that trusts the oidc package. Read it
// here, never through oidc.UserTypeFromContext, which the token lane does not
// publish. "" for a caller with no stamp: the admin token and local mode.
type oidcUserTypeCtxKey struct{}

func withOIDCUserType(ctx context.Context, userType string) context.Context {
	return context.WithValue(ctx, oidcUserTypeCtxKey{}, userType)
}

func oidcUserTypeFromContext(ctx context.Context) string {
	t, _ := ctx.Value(oidcUserTypeCtxKey{}).(string)
	return t
}

// callerSubjects is everything a subject-matched row can name about the
// caller: capabilitySubjects' identities and group snapshot, plus their one
// user type.
type callerSubjects struct {
	users, groups []string
	userType      string
	// stale is capabilitySubjects' bit: the group half is unanswerable.
	stale bool
}

// errUserTypeUnknown is the 403-class resolver failure: the caller's stamped
// user type has no row any more (deleted after they signed in). Every control
// that names a type refuses rather than resolving without it — dropping the
// type would silently lift a type-tier deny and hand the person the `all`
// tier's answer instead of their type's.
//
//lint:ignore ST1005 the text is the sentence the refused person reads, as errGroupsSnapshotStale's is
var errUserTypeUnknown = errors.New(userTypeUnknownMsg)

const userTypeUnknownMsg = "Your user type no longer exists, so Wardyn can't tell what you may use. " +
	"Ask an admin to give you another type, then sign in again."

// userTypeRefusalKey carries the request's "user_type_unknown is audited" bit.
// One request resolves the caller's subjects several times (a POST /runs asks
// the capability batch, the image grant, the ceiling and the drive) and each
// refuses, but it is one denial: the count its groups_snapshot_stale twin
// gets through the ceiling memo. Installed beside that memo by
// ceilingMemoMiddleware.
type userTypeRefusalKey struct{}

func withUserTypeRefusalOnce(ctx context.Context) context.Context {
	return context.WithValue(ctx, userTypeRefusalKey{}, new(atomic.Bool))
}

// firstUserTypeRefusal reports whether this is the request's first
// user_type_unknown refusal. A caller outside a request (a background job, a
// unit test) audits every one.
func firstUserTypeRefusal(ctx context.Context) bool {
	once, ok := ctx.Value(userTypeRefusalKey{}).(*atomic.Bool)
	return !ok || once.CompareAndSwap(false, true)
}

// callerSubjects resolves the caller's subjects for a control, failing closed
// on a type that no longer exists (errUserTypeUnknown, audited here once per
// request as authz.denied reason user_type_unknown).
//
// A human with no stamped type is the built-in type. Every SSO session carries
// a stamp (the session codec refuses one without), so the only unstamped
// human is an API token minted before tokens carried a type — and the built-in
// type is exactly what those tokens are backfilled with when they do. A
// caller with no human at all (admin token, local mode) has no type.
//
// The built-in type is never read back: it is seeded by migration and
// DeleteUserType refuses it, so it always exists.
func (s *Server) callerSubjects(ctx context.Context) (callerSubjects, error) {
	users, groups, stale := capabilitySubjects(ctx)
	c := callerSubjects{users: users, groups: groups, stale: stale, userType: oidcUserTypeFromContext(ctx)}
	if len(users) == 0 {
		c.userType = ""
		return c, nil
	}
	if c.userType == "" || c.userType == types.UserTypeStandard {
		c.userType = types.UserTypeStandard
		return c, nil
	}
	if s.cfg.Store == nil {
		return callerSubjects{}, fmt.Errorf("api: user type %q cannot be resolved: no store configured", c.userType)
	}
	_, err := s.cfg.Store.GetUserType(ctx, c.userType)
	if errors.Is(err, store.ErrNotFound) {
		if s.cfg.Audit != nil && !isDisplayRead(ctx) && firstUserTypeRefusal(ctx) {
			s.recordRefusal(ctx, nil, authz.Deny(authz.ReasonUserTypeUnknown, "user_type", userTypeUnknownMsg))
		}
		return callerSubjects{}, errUserTypeUnknown
	}
	if err != nil {
		return callerSubjects{}, fmt.Errorf("api: resolve user type %q: %w", c.userType, err)
	}
	return c, nil
}

// userTypeSubjectExists is the write-boundary half, shared by the three tables
// a user type can be written against: a row naming a type must name one that
// exists, or it would bind nobody today and silently bind whoever a later type
// of the same id is given to. Writes the 400 (or the 500) and returns false
// when the row may not be written; other subject types pass without a read.
// The shape (a well-formed slug) is the validator's; this is the store half.
func (s *Server) userTypeSubjectExists(w http.ResponseWriter, r *http.Request, subjectType types.CapabilitySubjectType, subject string) bool {
	if subjectType != types.CapabilitySubjectUserType || subject == types.UserTypeStandard {
		return true
	}
	_, err := s.cfg.Store.GetUserType(r.Context(), subject)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, accessUnknownUserType(subject))
		return false
	}
	if err != nil {
		writeServerError(w, r, "read user type", err)
		return false
	}
	return true
}

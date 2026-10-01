// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// membermode.go is the ONE writer of Session.MemberMode: "view as member", letting an admin see
// what a member sees without a second identity. Security invariants, all copied VERBATIM rather
// than derived: Role (clamped downward only at contextWithPrincipal); IssuedAt (a revoked session
// can't toggle back to life via SessionRevocations.IsSessionRevoked); Expiry (a toggle is not a
// renewal); Sub/Email/Name/Groups/GroupsTruncated (a member-mode admin is still, provably,
// themselves).

import (
	"context"
	"net/http"
)

// SetUserView flips this request's session into or out of the user view ("view as member" until
// 0.8), writes the re-encoded cookie to w, and returns the session's STAMPED role — what the view
// pauses. typeID is Session.UserViewType, pre-validated by the caller; any switch clears
// UserViewDropped. The stamped role is returned because RoleFromContext is already clamped to
// member while the mode is on. Decodes the CURRENT cookie, not a Session (context values would
// drop IssuedAt); a bad cookie surfaces decodeSession's own error as a 4xx.
//
// ONE exception: a STAMPED member turning the mode ON gets no cookie at all — downstream readers
// key on the flag, not the tier (a banner this human can't back, a refused token mint, capped SSH
// keys). OFF always re-signs and never CLEARS the session, so toggling off leaves the human signed
// in. noCredential is "view as a member who hasn't signed in", stored as `on && noCredential` so
// OFF clears it by construction.
//
// typeName caches typeID's display name (Session.UserViewTypeName) purely so
// a LATER DropUserView can still name the type once its row is gone — see
// that field's own doc. The caller resolves it; this function never reads
// the store.
func (a *Authenticator) SetUserView(w http.ResponseWriter, r *http.Request, on bool, typeID, typeName string, noCredential bool) (stampedRole string, err error) {
	sess, err := a.decodeSession(r)
	if err != nil {
		return "", err
	}
	if on && sess.Role == RoleUser {
		return sess.Role, nil
	}
	sess.MemberMode = on
	sess.MemberModeNoCredential = on && noCredential
	sess.UserViewType = ""
	sess.UserViewTypeName = ""
	if on {
		sess.UserViewType = typeID
		sess.UserViewTypeName = typeName
	}
	sess.UserViewDropped = ""
	sess.UserViewDroppedName = ""
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, cookie)
	return sess.Role, nil
}

// UserViewStampedRole is the role stamped on this request's signed session while the user view is
// ON, and "" otherwise — outside the view, with no cookie, or with one that does not verify. It is
// the only reader of the stamped role beside SetUserView: the context's role is already clamped
// to user in the view, so the console's "is this a super admin" bit cannot come from there. It
// reads and verifies the cookie exactly as SetUserView does and never writes one.
func (a *Authenticator) UserViewStampedRole(r *http.Request) string {
	sess, err := a.decodeSession(r)
	if err != nil || !sess.MemberMode {
		return ""
	}
	return sess.Role
}

// DropUserView turns the user view off when its type no longer exists: the cookie re-signs with
// view bits cleared and the type recorded in UserViewDropped (so GET /me can say why), returning
// the context republished from that session with the admin's real tier. Only GET /me may serve
// its own request from that context — every other request meeting a deleted type is refused, not
// re-evaluated.
func (a *Authenticator) DropUserView(w http.ResponseWriter, r *http.Request) (context.Context, error) {
	sess, err := a.decodeSession(r)
	if err != nil {
		return nil, err
	}
	if !sess.MemberMode {
		return r.Context(), nil
	}
	sess.UserViewDropped = viewedUserType(sess)
	sess.UserViewDroppedName = sess.UserViewTypeName
	sess.MemberMode = false
	sess.MemberModeNoCredential = false
	sess.UserViewType = ""
	sess.UserViewTypeName = ""
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, cookie)
	return contextWithPrincipal(r.Context(), sess), nil
}

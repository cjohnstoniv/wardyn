// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// membermode.go is the ONE writer of Session.MemberMode: "view as member",
// letting an admin see what a member sees without a second identity.
//
// Security invariants, all copied VERBATIM rather than derived: Role (clamped
// downward only at contextWithPrincipal); IssuedAt (a revoked session can't
// toggle back to life via SessionRevocations.IsSessionRevoked); Expiry (a
// toggle is not a renewal); Sub/Email/Name/Groups/GroupsTruncated (a
// member-mode admin is still, provably, themselves).

import (
	"context"
	"net/http"
)

// SetUserView flips this request's session into or out of the user view
// ("view as member" until 0.8), writes the re-encoded cookie to w, and
// returns the session's STAMPED role — what the view pauses.
//
// typeID is Session.UserViewType, pre-validated by the caller; any switch
// clears UserViewDropped. The stamped role is returned because
// RoleFromContext is already clamped to member while the mode is on.
//
// Decodes the CURRENT cookie, not a Session (context values would drop
// IssuedAt); a bad cookie surfaces decodeSession's own error as a 4xx.
//
// ONE exception: a STAMPED member turning the mode ON gets no cookie at all —
// downstream readers key on the flag, not the tier (a banner this human
// can't back, a refused token mint, capped SSH keys). OFF always re-signs
// and never CLEARS the session, so toggling off leaves the human signed in.
//
// noCredential (0.7.5) is "view as a member who hasn't signed in", stored as
// `on && noCredential` so OFF clears it by construction.
func (a *Authenticator) SetUserView(w http.ResponseWriter, r *http.Request, on bool, typeID string, noCredential bool) (stampedRole string, err error) {
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
	if on {
		sess.UserViewType = typeID
	}
	sess.UserViewDropped = ""
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, cookie)
	return sess.Role, nil
}

// DropUserView turns the user view off when its type no longer exists: the
// cookie re-signs with view bits cleared and the type recorded in
// UserViewDropped (so GET /me can say why), returning the context
// republished from that session with the admin's real tier.
//
// Only GET /me may serve its own request from that context — every other
// request meeting a deleted type is refused, not re-evaluated.
func (a *Authenticator) DropUserView(w http.ResponseWriter, r *http.Request) (context.Context, error) {
	sess, err := a.decodeSession(r)
	if err != nil {
		return nil, err
	}
	if !sess.MemberMode {
		return r.Context(), nil
	}
	sess.UserViewDropped = viewedUserType(sess)
	sess.MemberMode = false
	sess.MemberModeNoCredential = false
	sess.UserViewType = ""
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return nil, err
	}
	http.SetCookie(w, cookie)
	return contextWithPrincipal(r.Context(), sess), nil
}

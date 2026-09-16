// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

// membermode.go — the ONE writer of Session.MemberMode: "view as member", the
// toggle that lets an admin see what a member sees without a second identity
// (v0.7.4, field-report P2).
//
// It lives in its own file for the reason context.go and session_codec.go do —
// oidc.go is at the file-size gate — and because the whole security argument of
// the feature is one function long and should be readable in one screen:
//
//   - The stamped Role is copied VERBATIM. Nothing here derives a role, so
//     there is no path by which this toggle widens anybody; the effective role
//     is clamped downward at contextWithPrincipal and nowhere else.
//   - IssuedAt is copied VERBATIM, and that is load-bearing rather than tidy: it
//     is the value SessionRevocations.IsSessionRevoked compares against a
//     revoke cutoff, so a session an admin has cut off must not be able to
//     toggle its way to a fresh issue time and back to life.
//   - Expiry is copied VERBATIM: a toggle is not a renewal.
//   - Sub/Email/Name/Groups/GroupsTruncated are copied VERBATIM: the human on
//     the other side of this cookie has not changed, and every audit row,
//     ownership check and capability grant still resolves against them. That is
//     the design's whole claim — a member-mode admin is still, provably,
//     themselves.

import "net/http"

// SetMemberMode flips this request's session into or out of "view as member"
// mode and writes the re-encoded cookie to w.
//
// It decodes the caller's CURRENT cookie rather than taking a Session: the
// cookie is the only authority on what this human's session says, and building
// one from context values would silently drop every field the context does not
// publish (IssuedAt above all). A missing/invalid/expired cookie returns
// decodeSession's own error — ErrNoSession or ErrInvalidSession — which the
// caller answers as a 4xx; this function never mints a session, only re-signs
// the one presented.
//
// Setting the mode it is already in is a no-op in effect and still re-writes
// the cookie, which costs one Set-Cookie header and keeps the handler free of a
// branch whose only job would be to answer 200 twice.
//
// Note the deliberate asymmetry with clearCookie's callers: this never CLEARS
// the session. Toggling off has to leave the human signed in — it is the exit
// from the mode, and an exit that signed you out would be a trap.
func (a *Authenticator) SetMemberMode(w http.ResponseWriter, r *http.Request, on bool) error {
	sess, err := a.decodeSession(r)
	if err != nil {
		return err
	}
	sess.MemberMode = on
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return err
	}
	http.SetCookie(w, cookie)
	return nil
}

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
// mode, writes the re-encoded cookie to w, and returns the session's STAMPED
// role — what the mode pauses.
//
// The stamped role is returned rather than left for the caller to re-derive
// because the caller CANNOT: RoleFromContext is already clamped to member while
// the mode is on, so a caller reading it back would record "member" as the role
// being paused, and a security_admin would be recorded as an admin. This
// function has the cookie open anyway; it is one return value, not a second
// parse of the session.
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
// ONE exception, and it is not cosmetic (W6-4): a caller whose STAMPED role is
// already member, asking to turn the mode ON, gets no cookie at all. There is
// nothing to pause — but the flag does not know that, and everything that reads
// it keys on the flag rather than on the tier: /me would answer
// member_mode:true, the console would paint a banner naming an admin role this
// human does not hold, the API-token mint (which reads MemberModeFromContext,
// not the stamped role) would refuse this member their own token with "Exit
// member mode…", and their SSH keys would be stored capped — breaking the member
// Getting Started's own "Connect your tools" card until they found the banner's
// Exit. The route is classMember so that the EXIT is always reachable, which
// makes this state reachable too. Turning it OFF still re-signs, always: that
// direction has to work from inside the mode.
//
// Note the deliberate asymmetry with clearCookie's callers: this never CLEARS
// the session. Toggling off has to leave the human signed in — it is the exit
// from the mode, and an exit that signed you out would be a trap.
//
// noCredential (0.7.5) selects the SECOND posture, "view as a new member who has
// not signed in" — see Session.MemberModeNoCredential. It is stored as
// `on && noCredential` rather than verbatim, which is what makes turning the
// mode OFF clear it by construction: there is no path that leaves the preview
// bit set on a session whose mode bit is not, so nothing downstream has to
// defend against that pair. It rides BELOW the real-member early return above
// for the same reason the mode bit does — a real member has no credential of
// their own to hide from themselves, and the doors that key on the preview
// would refuse them their own sign-in.
func (a *Authenticator) SetMemberMode(w http.ResponseWriter, r *http.Request, on, noCredential bool) (stampedRole string, err error) {
	sess, err := a.decodeSession(r)
	if err != nil {
		return "", err
	}
	if on && sess.Role == RoleMember {
		return sess.Role, nil
	}
	sess.MemberMode = on
	sess.MemberModeNoCredential = on && noCredential
	cookie, err := a.encodeSession(sess)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, cookie)
	return sess.Role, nil
}

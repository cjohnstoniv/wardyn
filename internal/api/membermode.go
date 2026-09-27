// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode.go — the two mint-door refusal sentences the user view carries.
// Renamed in 0.8 from "view as member" / POST /me/member-mode
// (docs/OPERATIONS.md's "Renamed in 0.8" appendix; history not rewritten — a
// pre-0.8 audit row still reads auth.member_mode). handleSetUserView itself —
// the route this file used to serve — lives in user_view.go, which grew a
// chosen user type (#835/UT-13) before this rename sweep (#617) reached it;
// the authz.denied marker moved with it, into internal/authz.Datum.
//
// What the view is. An admin asks to be treated as a user for the rest of
// this browser session. internal/auth/oidc does the whole of the clamping:
// Session.MemberMode rides the existing cookie and contextWithPrincipal — the
// one read of the stamped role — publishes RoleUser instead. Nothing in this
// package re-derives a tier, so every predicate here (isOperator,
// isSecurityOperator, every ownsRunOrAdmin) follows for free.
//
// What it is not. It is not impersonation and it is not a second identity: the
// sub, email, groups, ownership and every audit row stay the admin's own. It is
// also not proof that a USER would be refused — a real second identity is that
// proof (docs/OPERATIONS.md names both, and the view's three ceilings).

// DRAFT (M2 canon pending)
const (
	// userViewNoHumanRefusal is the 400 for the no-per-human-role lane: the
	// admin token, local mode, and a deployment with no IdP configured at all.
	// All three are ONE shared credential that isOperator answers true for with
	// no session role to demote — so there is genuinely nothing to pause, and
	// pretending otherwise would ship the exact contradiction: a console that
	// says "you are a user" over an API that still says admin.
	userViewNoHumanRefusal = "The user view needs a signed-in SSO human: the admin token, local mode " +
		"and a deployment with no identity provider all use one shared credential with no per-person " +
		"role to pause. Sign in through SSO to use it."
	// userViewMintRefusal is the 409 the API-token mint door answers with
	// while the view is on: a token minted here would be re-stamped with the
	// caller's REAL role at their next sign-in (store.RefreshAPITokenIdentity,
	// fired by OnLogin), so a "user" token would quietly become an admin one
	// and outlive the view that created it. The SSH-key door no longer refuses:
	// a key registered in the view is stored capped instead (sshkeys.go,
	// migration 0070).
	userViewMintRefusal = "Exit the user view to mint a token."
)

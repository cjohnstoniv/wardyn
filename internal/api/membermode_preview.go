// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode_preview.go — "view as a NEW member (not signed in)", the second
// posture of member mode.
//
// It lives in its own file rather than in membermode.go because the whole
// argument for the one guard it publishes is a screen long, and because
// harnesscred.go — where the guard is APPLIED — is one line under the file-size
// cap and its audit citations are checked with a zero-line window, so the
// reasoning cannot live beside the call.

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// DRAFT (M2 canon pending)
const (
	// memberPreviewSignInRefusal is the 409 POST /setup/harness-login answers
	// inside the preview. A capture made here would land on the ADMIN'S OWN
	// namespace — the preview hides their credential, it does not give them a
	// second identity to capture into — so allowing the launch would let the
	// admin overwrite their real session while believing they were signing in as
	// somebody else. It is refused AFTER authorizeHarnessLogin, so a deployment
	// whose roster row is `shared` still answers the member's own
	// harness_login_not_per_user 403: that is what a real member meets, and the
	// preview exists to show what a real member meets.
	memberPreviewSignInRefusal = "Exit member mode to sign in to AWS — the capture would land on your own identity."
)

// previewHidesOwnCredential reports whether THIS request is being made inside
// the no-credential preview, in which the caller's own per-user model
// credential must read as absent.
//
// Why the read chokepoint and not the scope. awsSSOScopeFor is pure and
// ctx-less, and three call sites hold a site config and call it directly, so a
// bit on awsSSOScope would ripple a signature change through every one of them
// and put the preview into a value that is also used at WRITE doors. readAWSSSOBlob
// is the ONE place a captured session is read, so one guard there makes every
// downstream state — /setup/status's probe, the create-time mechanism gate,
// dispatch's resolveBedrockAuth, the pane's status re-read — answer
// "not signed in" with no rule of their own. Every one of them already fails
// closed on an absent credential; the preview simply reaches that arm.
//
// Per-user only, and that is the security property, not a scoping detail: the
// operator namespace is the credential a `shared` deployment gives EVERY run,
// so hiding it there would show an admin a state no member on that deployment
// is ever in, and would do it by suppressing the one credential the whole
// install runs on. Under `shared` the preview therefore changes nothing — which
// is correct: a member there has no sign-in of their own to be missing.
//
// Not a widening in any direction. It can only make a read answer ABSENT, and
// absent is the fail-closed answer everywhere it lands. The writers
// (storeAWSSSOBlob, the run-token upload routes) never consult it, and the one
// door that could have written inside the preview — the login launch — is
// refused with memberPreviewSignInRefusal above.
//
// It is a request-scoped fact. It rides the context published by
// oidc.contextWithPrincipal, so it is visible exactly as long as the request's
// context is: every dispatch this reaches today is handler-synchronous and
// detaches with context.WithoutCancel, which preserves values. A future
// DEFERRED dispatcher — one that re-creates a context from the run row rather
// than carrying the caller's — would stop seeing it, and would then resolve the
// admin's REAL credential for a run created inside the preview. Nothing is
// stored on the run to prevent that, deliberately (the preview is a view, not a
// run attribute); a deferred dispatcher must therefore refuse or re-derive it,
// not inherit it silently.
func previewHidesOwnCredential(ctx context.Context) bool {
	return oidc.MemberPreviewNoCredential(ctx)
}

// memberPreviewApplies reports whether the no-credential posture would MEAN
// anything on this deployment for this caller: the model-access agent's roster
// row has to name a credential PER PERSON.
//
// It is the AVAILABILITY rule, enforced at the write (the toggle) and published
// on /me, rather than left to the console. Under a `shared` row — or any install
// with no enabled bedrock_sso/per_user row — the guard in readAWSSSOBlob
// deliberately never fires, so entering the posture there would paint a banner
// saying "not signed in to AWS … runs that need it are refused" over a
// /setup/status that grades `live` and a POST /runs that answers 201. A sentence
// false on some deployments is the exact defect class this release exists to
// close, and the strings are canon — so the fix is availability, not wording.
// On `shared` the caller lands in the PLAIN mode, whose banner is true
// everywhere.
//
// FAIL-CLOSED on a roster that could not be read: ok=false answers false, so a
// store blip downgrades to the plain mode rather than promising a hiding that
// will not happen. A nil store is not that blip — awsSSOScopeForAgent reads it
// as an install with no roster, which is a deployment with no per-user estate
// and therefore correctly unavailable.
//
// Residual, documented rather than coded (docs/OPERATIONS.md): an admin already
// inside the preview when an admin flips the roster per_user -> shared keeps the
// variant banner until they exit. The cookie is the record of what they asked
// for, and re-reading the roster on every render to expire a banner would put a
// store read on every screen.
func (s *Server) memberPreviewApplies(ctx context.Context, r *http.Request) bool {
	scope, ok := s.awsSSOScopeForAgent(ctx, modelAccessAgent,
		runIdentitySubject(ctx, principalFromRequest(r)))
	return ok && scope.perUser
}

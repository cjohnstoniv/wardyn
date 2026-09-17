// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode_preview.go — "view as a NEW member (not signed in)", the second
// posture of member mode (v0.7.5, field report finding 3).
//
// It lives in its own file rather than in membermode.go because the whole
// argument for the one guard it publishes is a screen long, and because
// harnesscred.go — where the guard is APPLIED — is one line under the file-size
// cap and its audit citations are checked with a zero-line window, so the
// reasoning cannot live beside the call.

import (
	"context"

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
// WHY THE READ CHOKEPOINT AND NOT THE SCOPE. awsSSOScopeFor is pure and
// ctx-less, and three call sites hold a site config and call it directly, so a
// bit on awsSSOScope would ripple a signature change through every one of them
// and put the preview into a value that is also used at WRITE doors. readAWSSSOBlob
// is the ONE place a captured session is read, so one guard there makes every
// downstream state — /setup/status's probe, the create-time mechanism gate,
// dispatch's resolveBedrockAuth, the pane's status re-read — answer
// "not signed in" with no rule of their own. Every one of them already fails
// closed on an absent credential; the preview simply reaches that arm.
//
// PER-USER ONLY, and that is the security property, not a scoping detail: the
// operator namespace is the credential a `shared` deployment gives EVERY run,
// so hiding it there would show an admin a state no member on that deployment
// is ever in, and would do it by suppressing the one credential the whole
// install runs on. Under `shared` the preview therefore changes nothing — which
// is correct: a member there has no sign-in of their own to be missing.
//
// NOT A WIDENING IN ANY DIRECTION. It can only make a read answer ABSENT, and
// absent is the fail-closed answer everywhere it lands. The writers
// (storeAWSSSOBlob, the run-token upload routes) never consult it, and the one
// door that could have written inside the preview — the login launch — is
// refused with memberPreviewSignInRefusal above.
//
// IT IS A REQUEST-SCOPED FACT. It rides the context published by
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

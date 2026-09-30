// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode_preview.go — "view as a NEW user (not signed in)", the second
// posture of the user view (renamed in 0.8 from member mode).
//
// It lives in its own file rather than in membermode.go because the whole
// argument for the one guard it publishes is a screen long.

import (
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// previewHidesOwnCredential reports whether THIS request is being made inside
// the no-credential preview, in which the caller's own per-user model
// credential must read as absent.
//
// Why the read chokepoints and not the scope. readAWSSSOBlob and ownSecret are
// the places a person's own model credential is read, so one guard at each
// makes every downstream state — /setup/status's provider_access, the create
// door's liveness check, dispatch — answer "not connected" with no rule of
// their own. Every one of them already fails closed on an absent credential;
// the preview simply reaches that arm.
//
// Not a widening in any direction. It can only make a read answer ABSENT, and
// absent is the fail-closed answer everywhere it lands. The writers never
// consult it, and a provider's sign-in door refuses inside it, since a capture
// would land on the admin's own identity.
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

// userPreviewApplies reports whether the no-credential posture would MEAN
// anything on this deployment: a model provider has to be on, since every model
// credential is each person's own and there is otherwise nothing a new user
// would be missing.
//
// It is the AVAILABILITY rule, enforced at the write (the toggle) and published
// on /me, rather than left to the console. FAIL-CLOSED on a site config that
// could not be read: false downgrades to the plain mode rather than promising a
// hiding that will not happen.
func (s *Server) userPreviewApplies(ctx context.Context) bool {
	sc, ok := s.siteConfigSnapshot(ctx)
	return ok && slices.ContainsFunc(modelProviderRows(sc), func(p types.ModelProvider) bool { return !p.Disabled })
}

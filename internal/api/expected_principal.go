// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/url"
)

// expectedPrincipalHeader names the person a console draft was composed under.
// The console sends it, URL-escaped, on the writes that submit a long-lived
// draft (ui/src/app/lib/api/core.ts, WfetchInit.asPerson).
const expectedPrincipalHeader = "X-Wardyn-Expected-Principal"

// expectedPrincipalMismatch is the operator API sentence for the refusal. None
// of the existing stale sentences fits every draft (a launch is not a providers
// document), so there is one generic one.
const expectedPrincipalMismatch = "this page was opened as a different person than the one now signed in — reload and retry"

// expectedPrincipalGuard answers 412 before any write when a draft was composed
// under a different person than the one the shared session cookie now names:
// another tab signed in as someone else after this tab last confirmed its own
// identity. It is a stale answer like an If-Match miss, but with a reason of its
// own (reasonExpectedPrincipalMismatch), so nothing is re-sent on its own and the
// person retries explicitly. No header (an older console, the CLI, a script) is
// permitted, exactly as with If-Match. Reads never carry the header's meaning.
// Mounted after humanOrAdminAuth, which is what makes principalFromRequest the
// same value /me reported to the console.
func (s *Server) expectedPrincipalGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get(expectedPrincipalHeader)
		if h == "" || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// An undecodable value cannot name anyone, so it fails closed as a mismatch.
		if want, err := url.PathUnescape(h); err == nil && want == principalFromRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeErrorReason(w, http.StatusPreconditionFailed, reasonExpectedPrincipalMismatch, expectedPrincipalMismatch)
	})
}

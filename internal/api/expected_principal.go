// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/url"
)

// expectedPrincipalHeader names the person a console Save was composed under.
// The console sends it, URL-escaped, on the two providers screens' Save.
const expectedPrincipalHeader = "X-Wardyn-Expected-Principal"

// refuseExpectedPrincipalMismatch answers 412 before any write when the Save was
// composed under a different person than the one the shared session cookie now
// names: another tab signed in as someone else after this tab last confirmed
// its own identity. The same stale answer an If-Match miss gives, so the console
// shows its existing reload-and-retry state and nothing is re-sent on its own.
// No header (an older console, the CLI, a script) is permitted, exactly as with
// If-Match. Returns true when it wrote the refusal.
func refuseExpectedPrincipalMismatch(w http.ResponseWriter, r *http.Request, sentence string) bool {
	h := r.Header.Get(expectedPrincipalHeader)
	if h == "" {
		return false
	}
	// An undecodable value cannot name anyone, so it fails closed as a mismatch.
	if want, err := url.PathUnescape(h); err == nil && want == principalFromRequest(r) {
		return false
	}
	writeErrorReason(w, http.StatusPreconditionFailed, reasonSiteConfigStale, sentence)
	return true
}

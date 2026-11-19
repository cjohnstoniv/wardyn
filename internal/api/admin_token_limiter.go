// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "net/http"

// admitAdminTokenGuess spends one of the client address's admin bearer attempts before the token is compared,
// and answers 429 itself when the address has none left. refundAdminTokenGuess gives it back once the token
// matches, so only wrong guesses count and an address that is out of them is refused without being compared.
func (s *Server) admitAdminTokenGuess(w http.ResponseWriter, r *http.Request) bool {
	if s.adminTokenPeerLimiter.allow(peerKey(r.RemoteAddr), s.cfg.Now()) {
		return true
	}
	s.auditAuthFailed(r, "admin_token_rate_limited")
	w.Header().Set("Retry-After", "10")
	writeErrorReason(w, http.StatusTooManyRequests, reasonAdminTokenRateLimited, "too many failed admin token attempts from this address; retry later")
	return false
}

func (s *Server) refundAdminTokenGuess(r *http.Request) {
	s.adminTokenPeerLimiter.refund(peerKey(r.RemoteAddr), s.cfg.Now())
}

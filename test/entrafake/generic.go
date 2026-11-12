// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake

import (
	"net/http"
	"net/url"
	"slices"
)

// Generic-mode and reuse-detection controls, and the authorize tail they share with the Entra mode.

// SetGenericMode makes this fake a plain OIDC authority, the shape most non-Microsoft providers have: no
// resource model, so /authorize and /token accept whatever scope is asked for and grant exactly that, a
// refresh grant ignores any scope it carries, and token responses OMIT `scope` (RFC 6749 section 5.1 allows
// it when the granted set is the requested one). A control plane has to read the omission as "the requested
// set"; an Entra tenant never omits it.
func (s *Server) SetGenericMode(v bool) { s.set(func() { s.generic = v }) }

// SetReuseDetection makes a refresh token that was rotated away and is presented again revoke the whole
// family it belongs to, including the token that replaced it, the way an authority that detects theft does.
// The presented token answers invalid_grant either way; with this on, the NEWER token of the family is dead
// too, so a client that lost its rotation write loses the session at its next redemption.
func (s *Server) SetReuseDetection(v bool) { s.set(func() { s.reuseDetection = v }) }

// RefreshRequestScopes is the `scope` of every refresh_token request this fake has answered, in order, with
// "" for a request that sent none.
func (s *Server) RefreshRequestScopes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.refreshScopes)
}

func (s *Server) isGeneric() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generic
}

// finishAuthorize picks the person, issues the code for requested and redirects back with it.
func (s *Server) finishAuthorize(w http.ResponseWriter, r *http.Request, redirectURI, state string, requested []string) {
	who, picked := s.pickIdentity(w, r)
	if !picked {
		return
	}

	code := randHex(16)
	s.mu.Lock()
	s.codes[code] = codeGrant{
		challenge:   r.URL.Query().Get("code_challenge"),
		scopes:      requested,
		nonce:       r.URL.Query().Get("nonce"),
		redirectURI: redirectURI,
		who:         who,
	}
	s.mu.Unlock()

	dest := redirectURI + "?code=" + url.QueryEscape(code)
	if state != "" {
		dest += "&state=" + url.QueryEscape(state)
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

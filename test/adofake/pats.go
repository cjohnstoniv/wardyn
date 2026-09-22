// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import "net/http"

// pat is one minted personal access token.
type pat struct {
	authorizationID string
	displayName     string
	scope           string
	validTo         string
	allOrgs         bool
	token           string
}

func patToMap(p *pat) map[string]any {
	return map[string]any{
		"authorizationId": p.authorizationID,
		"displayName":     p.displayName,
		"scope":           p.scope,
		"validTo":         p.validTo,
		"allOrgs":         p.allOrgs,
		"token":           p.token,
	}
}

// PatTokenError is the real Azure DevOps Tokens API's patTokenError enum — the
// value a CreatePat/UpdatePat response carries alongside a nil patToken when
// creation is refused for a policy reason rather than a scope failure.
type PatTokenError string

const (
	PatTokenErrorNone                     PatTokenError = "none"
	PatTokenErrorFullScopePolicyViolation PatTokenError = "fullScopePatPolicyViolation"
	PatTokenErrorAccessDenied             PatTokenError = "accessDenied"
	PatTokenErrorLifespanPolicyViolation  PatTokenError = "patLifespanPolicyViolation"
)

// SetPatCreateError makes every subsequent PAT-create call answer err instead
// of minting a token (PatTokenErrorNone, the default, restores normal
// creation) — for exercising the policy-violation paths a real tenant can
// return without a real tenant.
func (s *Server) SetPatCreateError(err PatTokenError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patCreateError = err
}

// handlePatsList answers GET .../_apis/tokens/pats with every minted PAT.
func (s *Server) handlePatsList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := make([]map[string]any, 0, len(s.pats))
	for _, p := range s.pats {
		out = append(out, patToMap(p))
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
}

// handlePatsCreate answers POST .../_apis/tokens/pats, honouring
// displayName/scope/validTo/allOrgs from the request body and minting a new
// authorizationId + token — unless SetPatCreateError has injected a policy
// violation, in which case it answers exactly as the real service does: a nil
// patToken and the violation's name in patTokenError.
func (s *Server) handlePatsCreate(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)

	s.mu.Lock()
	injected := s.patCreateError
	s.mu.Unlock()
	if injected != "" && injected != PatTokenErrorNone {
		writeJSON(w, http.StatusOK, map[string]any{"patToken": nil, "patTokenError": string(injected)})
		return
	}

	displayName, _ := body["displayName"].(string)
	scope, _ := body["scope"].(string)
	validTo, _ := body["validTo"].(string)
	allOrgs, _ := body["allOrgs"].(bool)

	p := &pat{
		authorizationID: randHex(16),
		displayName:     displayName,
		scope:           scope,
		validTo:         validTo,
		allOrgs:         allOrgs,
		token:           "adofake-pat-" + randHex(16),
	}
	s.mu.Lock()
	s.pats[p.authorizationID] = p
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"patToken": patToMap(p), "patTokenError": string(PatTokenErrorNone)})
}

// handlePatsUpdate answers PUT .../_apis/tokens/pats: the body names the PAT
// to change by authorizationId and carries the fields to overwrite, exactly
// as CreatePat does; the real service rotates the token value on update, so
// this fake does too.
func (s *Server) handlePatsUpdate(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)
	authID, _ := body["authorizationId"].(string)

	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pats[authID]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "adofake: unknown authorizationId " + authID})
		return
	}
	if v, ok := body["displayName"].(string); ok {
		p.displayName = v
	}
	if v, ok := body["scope"].(string); ok {
		p.scope = v
	}
	if v, ok := body["validTo"].(string); ok {
		p.validTo = v
	}
	if v, ok := body["allOrgs"].(bool); ok {
		p.allOrgs = v
	}
	p.token = "adofake-pat-" + randHex(16)
	writeJSON(w, http.StatusOK, map[string]any{"patToken": patToMap(p), "patTokenError": string(PatTokenErrorNone)})
}

// handlePatsRevoke answers DELETE .../_apis/tokens/pats?authorizationId=...,
// the real API's revoke shape. Revoking an unknown id is a no-op success, as
// on the real service.
func (s *Server) handlePatsRevoke(w http.ResponseWriter, r *http.Request) {
	authID := r.URL.Query().Get("authorizationId")
	s.mu.Lock()
	delete(s.pats, authID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

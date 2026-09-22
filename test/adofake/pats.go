// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"net/http"
	"strings"
	"time"
)

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

// parseValidTo parses the RFC3339 validTo a caller declared; a blank or
// unparseable value means no expiry (zero time), matching a plain
// RegisterToken grant rather than silently refusing every request the caller
// makes with a PAT whose date they got wrong.
func parseValidTo(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// grantForPat is the tokenGrant a minted/updated PAT registers into
// s.tokens: the scopes it declares (Azure DevOps' scope field is a
// space-separated list, e.g. "vso.code vso.work") and its validTo, honoured
// by checkScope — an ALREADY-expired validTo therefore mints a token that
// exists (patToMap/list still show it) but is refused the moment anything
// tries to use it, rather than working forever because nothing ever
// consulted the date.
func grantForPat(p *pat) *tokenGrant {
	return &tokenGrant{scopes: setOf(strings.Fields(p.scope)), validTo: parseValidTo(p.validTo)}
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
//
// The minted token is registered into s.tokens with the scopes it declares
// (and its validTo, if any): without this, the PAT this call hands back
// could never actually authorize anything, and the whole lifecycle would
// prove nothing beyond "the fake accepted a create call".
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
	s.tokens[p.token] = grantForPat(p)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"patToken": patToMap(p), "patTokenError": string(PatTokenErrorNone)})
}

// handlePatsUpdate answers PUT .../_apis/tokens/pats: the body names the PAT
// to change by authorizationId and carries the fields to overwrite, exactly
// as CreatePat does; the real service rotates the token value on update, so
// this fake does too — and moves the s.tokens grant from the old token value
// to the new one (with any updated scope/validTo), so the OLD token stops
// working and the new one carries whatever the update actually declared.
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
	oldToken := p.token
	p.token = "adofake-pat-" + randHex(16)
	delete(s.tokens, oldToken)
	s.tokens[p.token] = grantForPat(p)
	writeJSON(w, http.StatusOK, map[string]any{"patToken": patToMap(p), "patTokenError": string(PatTokenErrorNone)})
}

// handlePatsRevoke answers DELETE .../_apis/tokens/pats?authorizationId=...,
// the real API's revoke shape, and removes the PAT's grant from s.tokens —
// without that, a "revoked" token kept authorizing every request forever.
// Revoking an unknown id is a no-op success, as on the real service.
func (s *Server) handlePatsRevoke(w http.ResponseWriter, r *http.Request) {
	authID := r.URL.Query().Get("authorizationId")
	s.mu.Lock()
	if p, ok := s.pats[authID]; ok {
		delete(s.tokens, p.token)
	}
	delete(s.pats, authID)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Issuer kinds a listed person is shown under.
const (
	issuerKindEntra = "entra"
	issuerKindOIDC  = "oidc"
	issuerKindLocal = "local"
)

// handleListPeople is GET /api/v1/people: the people this deployment knows, with each one's
// role and the counts behind the leaver actions. It is on securityOps because it discloses the
// email of everyone who has signed in, the audience that can already read the audit trail.
//
// The role is what the role mappings give that email today. A person's groups and roles claims
// only exist at sign-in, so a mapping keyed on one of those shows here only once it also names
// the email or sets the default.
func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.cfg.Store.(store.PeopleDirectoryStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "listing people requires the Postgres store backend")
		return
	}
	q := r.URL.Query()
	filter := store.PeopleDirectoryFilter{Query: strings.TrimSpace(q.Get("q")), State: q.Get("state")}
	if filter.State != "" && filter.State != "active" && filter.State != "deactivated" {
		writeErrorReason(w, http.StatusBadRequest, reasonPeopleListParamInvalid, "state: want active or deactivated")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidLimitParam, "invalid limit")
			return
		}
		filter.Limit = n
	}
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(raw) == 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonPeopleListParamInvalid, "cursor: not one this endpoint returned")
			return
		}
		filter.After = string(raw)
	}

	page, err := ds.ListPeopleDirectory(r.Context(), filter)
	if err != nil {
		writeServerError(w, r, "list people", err)
		return
	}
	roleOf, err := s.personRoleResolver(r)
	if err != nil {
		writeServerError(w, r, "list people", err)
		return
	}
	out := types.PersonList{People: make([]types.PersonSummary, len(page.People))}
	for i, p := range page.People {
		out.People[i] = types.PersonSummary{
			Principal: p.Principal, Email: p.Email, IssuerKind: personIssuerKind(p),
			PreCreated: p.PreCreated, FirstSignedInAt: p.FirstSignInAt, LastSignedInAt: p.LastSignInAt,
			DeactivatedAt: p.DeactivatedAt, Role: roleOf(p.Email),
			ActiveSessions: p.ActiveSessions, APITokens: p.APITokens, SSHKeys: p.SSHKeys,
			Credentials: p.Credentials, ActiveRuns: p.ActiveRuns,
		}
	}
	if page.Next != "" {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.Next))
	}
	writeJSON(w, http.StatusOK, out)
}

// personIssuerKind names where a person's identity comes from. A local-mode seat ("local:" is
// reserved, see isReservedPrincipal) never signs in through an issuer, so none is normally
// listed; the kind is here so one never reads as an OIDC person.
func personIssuerKind(p store.PersonListing) string {
	switch {
	case strings.HasPrefix(p.Principal, "local:"):
		return issuerKindLocal
	case p.Entra:
		return issuerKindEntra
	}
	return issuerKindOIDC
}

// personRoleResolver reads the role mappings and user types once and returns the role each email
// derives against them: "denied" when sign-in would refuse it, and "" when sign-in is not SSO.
func (s *Server) personRoleResolver(r *http.Request) (func(email string) string, error) {
	if s.cfg.OIDC == nil {
		return func(string) string { return "" }, nil
	}
	rows, err := s.cfg.Store.ListRoleMappings(r.Context())
	if err != nil {
		return nil, err
	}
	userTypes, err := s.cfg.Store.ListUserTypes(r.Context())
	if err != nil {
		return nil, err
	}
	mappings := toOIDCRoleMappings(rows)
	return func(email string) string {
		d := s.cfg.OIDC.PreviewRoleAgainst(mappings, userTypes, nil, nil, email)
		if !d.OK() {
			return accessDeniedRole
		}
		return d.Role
	}, nil
}

// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleUnknown is the People role of a person whose last sign-in's groups were truncated: the role
// those groups would derive cannot be known.
const roleUnknown = "unknown"

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
// The role is what the role mappings give the person's email and the groups of their last verified
// sign-in. It is "unknown" when that sign-in's groups claim was truncated, so the groups are
// incomplete, and the email alone when the person has no recorded sign-in groups.
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
	out := personDirectoryPage{People: make([]personDirectoryEntry, len(page.People))}
	for i, p := range page.People {
		out.People[i] = personDirectoryEntry{PersonSummary: types.PersonSummary{
			Principal: p.Principal, Email: p.Email, IssuerKind: personIssuerKind(p),
			PreCreated: p.PreCreated, FirstSignedInAt: p.FirstSignInAt, LastSignedInAt: p.LastSignInAt,
			DeactivatedAt: p.DeactivatedAt, Role: personRole(p, roleOf),
			ActiveSessions: p.ActiveSessions, APITokens: p.APITokens, SSHKeys: p.SSHKeys,
			Credentials: p.Credentials, ActiveRuns: p.ActiveRuns,
		}, Issuer: p.PortalIssuer, InternalAddress: p.PortalInternalIssuer}
	}
	if page.Next != "" {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.Next))
	}
	writeJSON(w, http.StatusOK, out)
}

// personDirectoryEntry is one listed person. A portal person also carries the issuer its portal's identity
// provider is and the internal address its binding fetches keys from ("" when the issuer's own address is
// used); both are absent for everyone else.
type personDirectoryEntry struct {
	types.PersonSummary
	Issuer          string `json:"issuer,omitempty"`
	InternalAddress string `json:"internal_address,omitempty"`
}

// personDirectoryPage is one page of GET /people, types.PersonList with portal people's two address fields.
type personDirectoryPage struct {
	People     []personDirectoryEntry `json:"people"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// personRole is a person's role in the list. A portal person is always a user: no role mapping is read for
// one, since none applies to a person the portal's own provider vouches for.
func personRole(p store.PersonListing, roleOf func(store.PersonListing) string) string {
	if p.PortalIssuer != "" {
		return oidc.RoleUser
	}
	return roleOf(p)
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
// derives against them from the email and last verified groups: "denied" when sign-in would refuse
// it, "unknown" when the stored groups are truncated, and "" when sign-in is not SSO.
func (s *Server) personRoleResolver(r *http.Request) (func(p store.PersonListing) string, error) {
	if s.cfg.OIDC == nil {
		return func(store.PersonListing) string { return "" }, nil
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
	return func(p store.PersonListing) string {
		if p.GroupsVerified && p.GroupsTruncated {
			return roleUnknown
		}
		d := s.cfg.OIDC.PreviewRoleAgainst(mappings, userTypes, nil, p.Groups, p.Email)
		if !d.OK() {
			return accessDeniedRole
		}
		return d.Role
	}, nil
}
